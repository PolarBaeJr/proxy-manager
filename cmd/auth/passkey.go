// Opt-in WebAuthn passkey login + enrollment for the SSO portal.
//
// The portal is multi-tenant (polardev.org AND the-aquarium.com). A WebAuthn
// credential is bound to ONE RP ID, so we keep one *Manager per cookie domain
// with RP ID = the bare domain (e.g. "polardev.org", NOT "auth.polardev.org")
// and RPOrigins = ["https://auth.<domain>"]. Credentials are stored keyed by
// (domain, lowercased-username). A polardev.org credential can NEVER validate
// against the-aquarium.com: every handler picks the manager for the request's
// OWN domain and the login handler only walks that domain's credentials.
//
// Enabled only when -passkey-rp-domains is non-empty; otherwise none of these
// routes are registered (they 404) and no store is opened.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
	"github.com/PolarBaeJr/proxy-manager/internal/sso"
	iwebauthn "github.com/PolarBaeJr/proxy-manager/internal/webauthn"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// buildPasskeyManagers returns one Manager per domain. RP ID is the bare
// domain; origins come from PASSKEY_RP_ORIGINS (comma-separated, scoped to
// each domain by suffix) or default to https://auth.<domain>.
func buildPasskeyManagers(domains []string, originsOverride string) map[string]*iwebauthn.Manager {
	var override []string
	for _, o := range strings.Split(originsOverride, ",") {
		if o = strings.TrimSpace(o); o != "" {
			override = append(override, o)
		}
	}
	out := map[string]*iwebauthn.Manager{}
	for _, d := range domains {
		var origins []string
		for _, o := range override {
			// Keep only origins under this domain — origins are a secondary
			// isolation check, so never let one domain's origin leak into
			// another's manager.
			if u, err := url.Parse(o); err == nil {
				h := strings.ToLower(u.Hostname())
				if h == d || strings.HasSuffix(h, "."+d) {
					origins = append(origins, o)
				}
			}
		}
		if len(origins) == 0 {
			origins = []string{"https://auth." + d}
		}
		m, err := iwebauthn.NewManager(d, origins, "proxy-manager")
		if err != nil {
			log.Printf("passkey: manager for %q disabled: %v", d, err)
			continue
		}
		out[d] = m
	}
	return out
}

// adapterFor builds a webauthn.User for (domain, username) from the store's
// current credentials.
func adapterFor(domain, username string, store *PasskeyStore) *iwebauthn.Adapter {
	creds := store.snapshotForDomain(domain)[strings.ToLower(username)]
	return iwebauthn.NewAdapter(iwebauthn.UserID(username), username, iwebauthn.Credentials(creds))
}

// userExists asks the dashboard whether the username is a real account. Any
// failure (network, non-200, exists=false) returns false — the caller treats
// that as fail-closed and mints no cookie.
func userExists(client *http.Client, dashboardURL, username string) bool {
	body, _ := json.Marshal(map[string]string{"username": username})
	resp, err := client.Post(dashboardURL+"/api/auth/user-exists", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("passkey: user-exists check unreachable: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var out struct {
		Exists bool `json:"exists"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return false
	}
	return out.Exists
}

// registerPasskeyRoutes wires the passkey endpoints. Early-returns (registering
// nothing) when managers is empty, so the feature stays fully off.
func registerPasskeyRoutes(mux *http.ServeMux, s *loginServer, managers map[string]*iwebauthn.Manager, store *PasskeyStore, rl *rateLimiter, dashboardURL string) {
	if len(managers) == 0 {
		return
	}

	// managerFor resolves the request's own domain and its manager, or "",nil.
	managerFor := func(r *http.Request) (string, *iwebauthn.Manager) {
		domain := s.matchDomain(hostOnly(r.Host))
		if domain == "" {
			return "", nil
		}
		return domain, managers[domain]
	}

	// GET /passkey/available — does this domain have any credential enrolled?
	mux.HandleFunc("/passkey/available", func(w http.ResponseWriter, r *http.Request) {
		domain, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"available": store.hasAnyForDomain(domain)})
	})

	// ---- Registration (requires a logged-in SSO session) ----

	mux.HandleFunc("/passkey/register/begin", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginOK(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		domain, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		user := s.ssoUser(r)
		if user == "" {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		opts, sess, err := m.W.BeginRegistration(adapterFor(domain, user, store),
			// Resident/discoverable key so login needs no username, and UV
			// required so a single gesture is both factors.
			webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
				ResidentKey:      protocol.ResidentKeyRequirementRequired,
				UserVerification: protocol.VerificationRequired,
			}),
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tok, err := m.PutCeremony(&iwebauthn.PendingCeremony{Session: *sess, Username: user})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"options": opts, "ceremony": tok})
	})

	mux.HandleFunc("/passkey/register/finish", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginOK(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		domain, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		user := s.ssoUser(r)
		if user == "" {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		ceremony, ok := m.PopCeremony(r.URL.Query().Get("ceremony"))
		if !ok {
			http.Error(w, "ceremony expired — restart", http.StatusBadRequest)
			return
		}
		if !strings.EqualFold(ceremony.Username, user) {
			http.Error(w, "ceremony username mismatch", http.StatusForbidden)
			return
		}
		parsed, err := protocol.ParseCredentialCreationResponseBody(r.Body)
		if err != nil {
			http.Error(w, "invalid credential response: "+err.Error(), http.StatusBadRequest)
			return
		}
		cred, err := m.W.CreateCredential(adapterFor(domain, user, store), ceremony.Session, parsed)
		if err != nil {
			http.Error(w, "credential creation failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		stored := iwebauthn.StoredCredential{
			ID:             cred.ID,
			PublicKey:      cred.PublicKey,
			AAGUID:         cred.Authenticator.AAGUID,
			SignCount:      cred.Authenticator.SignCount,
			Label:          r.URL.Query().Get("label"),
			UserPresent:    cred.Flags.UserPresent,
			UserVerified:   cred.Flags.UserVerified,
			BackupEligible: cred.Flags.BackupEligible,
			BackupState:    cred.Flags.BackupState,
		}
		for _, t := range parsed.Response.Transports {
			stored.Transports = append(stored.Transports, string(t))
		}
		if err := store.Add(domain, user, stored); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "registered", "label": stored.Label})
	})

	// ---- Login (public; rate-limited) ----

	mux.HandleFunc("/passkey/login/begin", rl.limit(func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginOK(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		_, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		// UV required so passkey login is strictly two-factor at assertion time
		// (matches the UV-required registration policy), not merely trusted.
		opts, sess, err := m.W.BeginDiscoverableLogin(
			webauthn.WithUserVerification(protocol.VerificationRequired),
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tok, err := m.PutCeremony(&iwebauthn.PendingCeremony{Session: *sess})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"options": opts, "ceremony": tok})
	}))

	mux.HandleFunc("/passkey/login/finish", rl.limit(func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginOK(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		domain, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		ceremony, ok := m.PopCeremony(r.URL.Query().Get("ceremony"))
		if !ok {
			http.Error(w, "ceremony expired — restart", http.StatusBadRequest)
			return
		}
		parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
		if err != nil {
			http.Error(w, "invalid assertion: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Walk THIS domain's credentials only, reversing the userHandle →
		// username convention so the library can look up the public key.
		snapshot := store.snapshotForDomain(domain)
		var matched string
		handler := func(rawID, userHandle []byte) (webauthn.User, error) {
			for username, creds := range snapshot {
				if string(iwebauthn.UserID(username)) == string(userHandle) {
					matched = username
					return iwebauthn.NewAdapter(iwebauthn.UserID(username), username, iwebauthn.Credentials(creds)), nil
				}
			}
			return nil, fmt.Errorf("no user matches handle")
		}
		cred, err := m.W.ValidateDiscoverableLogin(handler, ceremony.Session, parsed)
		if err != nil {
			http.Error(w, "passkey verification failed", http.StatusUnauthorized)
			return
		}
		store.UpdateSignCount(domain, matched, cred.ID, cred.Authenticator.SignCount, cred.Flags)

		// Fail-closed existence check: only mint if the dashboard confirms the
		// account still exists. Network error / non-200 / exists=false → 403,
		// no cookie.
		if !userExists(s.client, dashboardURL, matched) {
			http.Error(w, "account not found", http.StatusForbidden)
			return
		}

		// Passkey login is inherently 2FA (UV required) — mint the full SSO
		// cookie directly, reusing doLogin's cookie shape.
		exp := time.Now().Add(s.lifetime)
		http.SetCookie(w, &http.Cookie{
			Name:     sso.CookieName,
			Value:    sso.Sign(matched, exp, s.secret),
			Domain:   domain,
			Path:     "/",
			MaxAge:   int(s.lifetime / time.Second),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		target := "/login?ok=1"
		if redirect := r.URL.Query().Get("redirect"); s.validRedirect(redirect, domain) {
			target = redirect
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"username": matched, "target": target})
	}))

	// ---- List / delete (requires a logged-in SSO session) ----

	mux.HandleFunc("/passkeys/list", func(w http.ResponseWriter, r *http.Request) {
		domain, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		user := s.ssoUser(r)
		if user == "" {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, store.List(domain, user))
	})

	mux.HandleFunc("/passkeys/item", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !sameOriginOK(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		domain, m := managerFor(r)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		user := s.ssoUser(r)
		if user == "" {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		if err := store.Delete(domain, user, r.URL.Query().Get("id")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})

	// Enroll page (requires a logged-in SSO session).
	mux.HandleFunc("/passkeys", s.handlePasskeysPage)
}

func (s *loginServer) handlePasskeysPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/passkeys" {
		http.NotFound(w, r)
		return
	}
	if s.ssoUser(r) == "" {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(target), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, enrollPage)
}

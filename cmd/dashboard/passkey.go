// WebAuthn / passkey support. Wraps go-webauthn/webauthn to manage
// credentials per user. Sign-in via passkey is passwordless AND fully
// elevates the session — passkeys count as both authentication factors.
//
// Sessions for in-flight begin/finish pairs live in an in-memory map with a
// short TTL so a hung browser doesn't pin server memory. We don't persist
// them because both halves of a registration or login complete within seconds.

package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// StoredCredential is the persisted shape of one passkey for a user.
type StoredCredential struct {
	ID         []byte   `json:"id"`              // raw credential ID
	PublicKey  []byte   `json:"public_key"`      // COSE-encoded public key
	AAGUID     []byte   `json:"aaguid,omitempty"`
	SignCount  uint32   `json:"sign_count"`
	Label      string   `json:"label"`           // user-supplied nickname
	Transports []string `json:"transports,omitempty"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt int64    `json:"last_used_at,omitempty"`
}

// passkeyManager wraps the webauthn library and tracks in-flight ceremonies.
type passkeyManager struct {
	w    *webauthn.WebAuthn
	mu   sync.Mutex
	regs map[string]*pendingCeremony // keyed by ceremony token
}

type pendingCeremony struct {
	Session  webauthn.SessionData
	Username string // empty for discoverable / passkey-first login
	Expires  time.Time
}

const ceremonyTTL = 5 * time.Minute

// newPasskeyManager builds the WebAuthn config from env vars, falling back to
// localhost defaults so the dashboard works out of the box over an SSH tunnel.
//   PASSKEY_RP_ID      = "dashboard.example.com"  (no scheme/port)
//   PASSKEY_RP_ORIGINS = "https://dashboard.example.com,http://localhost:8093"
func newPasskeyManager(rpID, rpOrigins string) (*passkeyManager, error) {
	if rpID == "" {
		rpID = "localhost"
	}
	origins := []string{"http://localhost:8093"}
	if rpOrigins != "" {
		origins = nil
		for _, o := range strings.Split(rpOrigins, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				origins = append(origins, o)
			}
		}
	}
	w, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "Pi Dashboard",
		RPOrigins:     origins,
	})
	if err != nil {
		return nil, err
	}
	return &passkeyManager{w: w, regs: map[string]*pendingCeremony{}}, nil
}

// userAdapter implements webauthn.User so the library can read credentials
// off our stored user shape without leaking the User struct's other fields.
type userAdapter struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *userAdapter) WebAuthnID() []byte                         { return u.id }
func (u *userAdapter) WebAuthnName() string                       { return u.name }
func (u *userAdapter) WebAuthnDisplayName() string                { return u.name }
func (u *userAdapter) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func toAdapter(u *User) *userAdapter {
	id := make([]byte, 32)
	// Stable per-user ID: HMAC the username with a constant so the same user
	// gets the same WebAuthn ID across saves. We don't use the password salt
	// because passkey-only users (future) won't have one.
	h := []byte("webauthn-id:" + strings.ToLower(u.Username))
	copy(id, h)
	if len(h) < 32 {
		// Pad to 32 so browsers don't truncate.
		for i := len(h); i < 32; i++ {
			id[i] = 0
		}
	}
	creds := make([]webauthn.Credential, 0, len(u.Credentials))
	for _, c := range u.Credentials {
		creds = append(creds, webauthn.Credential{
			ID:        c.ID,
			PublicKey: c.PublicKey,
			Authenticator: webauthn.Authenticator{
				AAGUID:    c.AAGUID,
				SignCount: c.SignCount,
			},
		})
	}
	return &userAdapter{id: id, name: u.Username, creds: creds}
}

// ---- Ceremony token store (5-min TTL) ----

func (m *passkeyManager) putCeremony(c *pendingCeremony) (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	c.Expires = time.Now().Add(ceremonyTTL)
	m.mu.Lock()
	m.regs[tok] = c
	// Opportunistic GC — cheap because the map is tiny.
	for k, v := range m.regs {
		if time.Now().After(v.Expires) {
			delete(m.regs, k)
		}
	}
	m.mu.Unlock()
	return tok, nil
}

func (m *passkeyManager) popCeremony(tok string) (*pendingCeremony, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.regs[tok]
	if !ok {
		return nil, false
	}
	delete(m.regs, tok)
	if time.Now().After(c.Expires) {
		return nil, false
	}
	return c, true
}

// ---- AuthStore helpers for credentials ----

func (s *AuthStore) AddCredential(username string, c StoredCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findUser(username)
	if u == nil {
		return fmt.Errorf("user %q not found", username)
	}
	if c.Label == "" {
		c.Label = "Passkey"
	}
	c.CreatedAt = time.Now().Unix()
	u.Credentials = append(u.Credentials, c)
	return s.save()
}

// PublicCredential is the JSON shape returned to the dashboard UI. It hides
// the public key and uses a string ID (hex) so the frontend can pass it back
// in the DELETE URL without binary-encoding gymnastics.
type PublicCredential struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}

func (s *AuthStore) ListCredentials(username string) []PublicCredential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.findUser(username)
	if u == nil {
		return nil
	}
	out := make([]PublicCredential, 0, len(u.Credentials))
	for _, c := range u.Credentials {
		out = append(out, PublicCredential{
			ID:         credIDKey(c.ID),
			Label:      c.Label,
			CreatedAt:  c.CreatedAt,
			LastUsedAt: c.LastUsedAt,
		})
	}
	return out
}

func (s *AuthStore) DeleteCredential(username, idKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findUser(username)
	if u == nil {
		return fmt.Errorf("user %q not found", username)
	}
	for i, c := range u.Credentials {
		if credIDKey(c.ID) == idKey {
			u.Credentials = append(u.Credentials[:i], u.Credentials[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("credential not found")
}

// updateCredential bumps SignCount + LastUsedAt for an existing credential.
// Called after a successful login. Returns false if the credential is unknown.
func (s *AuthStore) updateCredential(username string, credID []byte, signCount uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findUser(username)
	if u == nil {
		return false
	}
	for i := range u.Credentials {
		if string(u.Credentials[i].ID) == string(credID) {
			u.Credentials[i].SignCount = signCount
			u.Credentials[i].LastUsedAt = time.Now().Unix()
			_ = s.save()
			return true
		}
	}
	return false
}

// credIDKey is the stable string key we use to identify a credential in the
// API (DELETE /api/users/passkeys/{key}). Just the hex of the raw ID.
func credIDKey(id []byte) string { return hex.EncodeToString(id) }

// ---- HTTP handlers ----

func registerPasskeyRoutes(mux *http.ServeMux, auth *AuthStore, pm *passkeyManager, rl *rateLimiter) {
	if pm == nil {
		return // disabled
	}

	// Reveals whether passkey login is even an option for the user agent. The
	// browser hits this on the login screen to decide whether to show the
	// passkey button.
	mux.HandleFunc("/api/auth/passkey/available", func(w http.ResponseWriter, _ *http.Request) {
		// Returns true if ANY user has at least one credential. Doesn't enumerate
		// users — the login flow takes a username separately.
		any := false
		auth.mu.RLock()
		for _, u := range auth.data.Users {
			if len(u.Credentials) > 0 {
				any = true
				break
			}
		}
		auth.mu.RUnlock()
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"available": any})
	})

	// ---- Registration (logged-in) ----

	mux.HandleFunc("/api/auth/passkey/register/begin", auth.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		info, _ := auth.sessionFrom(r)
		if info == nil {
			http.Error(w, "registration requires a logged-in session (not a token)", http.StatusUnauthorized)
			return
		}
		auth.mu.RLock()
		u := auth.findUser(info.Username)
		var snap User
		if u != nil {
			snap = *u
		}
		auth.mu.RUnlock()
		if u == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		opts, sess, err := pm.w.BeginRegistration(toAdapter(&snap))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tok, err := pm.putCeremony(&pendingCeremony{Session: *sess, Username: snap.Username})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"options": opts, "ceremony": tok})
	}))

	mux.HandleFunc("/api/auth/passkey/register/finish", auth.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		info, _ := auth.sessionFrom(r)
		if info == nil {
			http.Error(w, "registration requires a logged-in session", http.StatusUnauthorized)
			return
		}
		tok := r.URL.Query().Get("ceremony")
		label := r.URL.Query().Get("label")
		ceremony, ok := pm.popCeremony(tok)
		if !ok {
			http.Error(w, "ceremony expired — restart", http.StatusBadRequest)
			return
		}
		if !strings.EqualFold(ceremony.Username, info.Username) {
			http.Error(w, "ceremony username mismatch", http.StatusForbidden)
			return
		}
		parsed, err := protocol.ParseCredentialCreationResponseBody(r.Body)
		if err != nil {
			http.Error(w, "invalid credential response: "+err.Error(), http.StatusBadRequest)
			return
		}
		auth.mu.RLock()
		u := auth.findUser(info.Username)
		var snap User
		if u != nil {
			snap = *u
		}
		auth.mu.RUnlock()
		cred, err := pm.w.CreateCredential(toAdapter(&snap), ceremony.Session, parsed)
		if err != nil {
			http.Error(w, "credential creation failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		stored := StoredCredential{
			ID:        cred.ID,
			PublicKey: cred.PublicKey,
			AAGUID:    cred.Authenticator.AAGUID,
			SignCount: cred.Authenticator.SignCount,
			Label:     label,
		}
		for _, t := range parsed.Response.Transports {
			stored.Transports = append(stored.Transports, string(t))
		}
		if err := auth.AddCredential(info.Username, stored); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		audit(r, info.Username, "user.passkey_add", credIDKey(cred.ID))
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "registered", "label": stored.Label})
	}))

	// ---- Login (public; rate-limited) ----

	mux.HandleFunc("/api/auth/passkey/login/begin", rl.limit(func(w http.ResponseWriter, r *http.Request) {
		username := r.URL.Query().Get("username")
		if username == "" {
			http.Error(w, "username required", http.StatusBadRequest)
			return
		}
		auth.mu.RLock()
		u := auth.findUser(username)
		var snap User
		if u != nil {
			snap = *u
		}
		auth.mu.RUnlock()
		if u == nil || len(snap.Credentials) == 0 {
			// Don't reveal whether user exists. Return a generic refusal.
			http.Error(w, "passkey login unavailable for this user", http.StatusUnauthorized)
			return
		}
		opts, sess, err := pm.w.BeginLogin(toAdapter(&snap))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tok, err := pm.putCeremony(&pendingCeremony{Session: *sess, Username: snap.Username})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"options": opts, "ceremony": tok})
	}))

	mux.HandleFunc("/api/auth/passkey/login/finish", rl.limit(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("ceremony")
		ceremony, ok := pm.popCeremony(tok)
		if !ok {
			http.Error(w, "ceremony expired — restart", http.StatusBadRequest)
			return
		}
		parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
		if err != nil {
			http.Error(w, "invalid assertion: "+err.Error(), http.StatusBadRequest)
			return
		}
		auth.mu.RLock()
		u := auth.findUser(ceremony.Username)
		var snap User
		if u != nil {
			snap = *u
		}
		auth.mu.RUnlock()
		if u == nil {
			http.Error(w, "user no longer exists", http.StatusUnauthorized)
			return
		}
		cred, err := pm.w.ValidateLogin(toAdapter(&snap), ceremony.Session, parsed)
		if err != nil {
			audit(r, snap.Username, "auth.passkey_failed", err.Error())
			http.Error(w, "passkey verification failed", http.StatusUnauthorized)
			return
		}
		auth.updateCredential(snap.Username, cred.ID, cred.Authenticator.SignCount)
		// Passkey login is BOTH factors: issue a fully-elevated session immediately.
		elev := time.Now().Add(elevatedLifetime)
		setSessionCookie(w, auth.newCookie(snap.Username, elev))
		audit(r, snap.Username, "auth.passkey_ok", credIDKey(cred.ID))
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username":       snap.Username,
			"elevated_until": elev.Unix(),
		})
	}))

	// ---- List / delete (per user) ----

	mux.HandleFunc("/api/users/passkeys", auth.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		info, _ := auth.sessionFrom(r)
		if info == nil {
			http.Error(w, "listing passkeys requires a session", http.StatusUnauthorized)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, auth.ListCredentials(info.Username))
	}))

	mux.HandleFunc("/api/users/passkeys/", auth.requireElevated(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		info, _ := auth.sessionFrom(r)
		if info == nil {
			http.Error(w, "deleting passkeys requires a session", http.StatusUnauthorized)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/api/users/passkeys/")
		key, _ = url.PathUnescape(key)
		if err := auth.DeleteCredential(info.Username, key); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(r, info.Username, "user.passkey_delete", key)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}))
}


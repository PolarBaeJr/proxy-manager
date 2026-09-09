// Break-glass login: exchange an elevated dashboard API token (pmt_...) for a
// real SSO session cookie.
//
// Why it exists: every account is TOTP-enrolled, doLogin fail-closes on
// `*out.TOTPEnrolled && !out.CodeValid`, there is no TOTP reset endpoint, and
// enrolling a portal passkey itself requires an existing SSO session. An
// operator who loses their authenticator therefore has no way back in even
// with the correct password — an elevated API token is the only remaining
// proof of identity, and it is the credential that unblocks passkey
// enrollment.
//
// Deliberately narrower than the general auth path: only tokens that pass the
// dashboard's ELEVATED check are accepted. Auto-provisioned service tokens
// (statusbot's mounted credential) verify fine as an identity but must never
// mint a human session, so they are rejected here.

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// tokenPrefix mirrors the dashboard's apiTokenPrefix — a different main
// package, so it can't be imported.
const tokenPrefix = "pmt_"

func (s *loginServer) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOriginOK(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("token")
	redirect := r.PostFormValue("redirect")

	// Conditional CSRF: the login page's pmgr_csrf cookie is Path=/login, so a
	// browser submitting this form does send it and the double-submit pair is
	// enforced. A curl recovery call carries no cookie at all — skip the check
	// there rather than dead-ending the one path that exists when the browser
	// flow is unusable. csrfMaxAge is 10 minutes, so a long-open login page
	// also lands here and is left to sameOriginOK alone, which is sound:
	// browsers do send Origin on cross-site form POSTs.
	if _, err := r.Cookie(csrfCookie); err == nil && !csrfOK(r) {
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(redirect), http.StatusFound)
		return
	}

	domain := s.matchDomain(hostOnly(r.Host))
	if domain == "" {
		http.Error(w, "unrecognized host", http.StatusBadRequest)
		return
	}

	// Reject anything that isn't shaped like an API token without spending a
	// dashboard call (and without ever logging the value).
	if !strings.HasPrefix(token, tokenPrefix) {
		log.Print("token-login: rejected: not an API token")
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	body, _ := json.Marshal(map[string]string{"token": token})
	resp, err := s.client.Post(s.dashboardURL+"/api/auth/verify-token", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("token-login: dashboard unreachable: %v", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Carry the status: /api/auth/verify-token is rate-limited and every
		// portal call buckets under this container's single IP, so a 429 with
		// a perfectly good token is otherwise indistinguishable from a bad one.
		log.Printf("token-login: dashboard declined verification (status %d)", resp.StatusCode)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	// Pointer-typed like doLogin's totp_enrolled: an older dashboard image that
	// doesn't report elevation must be rejected, not trusted.
	var out struct {
		Username string `json:"username"`
		Elevated *bool  `json:"elevated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Username == "" || out.Elevated == nil {
		log.Printf("token-login: dashboard response missing identity or elevation signal (err=%v)", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	if !*out.Elevated {
		log.Printf("token-login: %q rejected: token is not elevated", out.Username)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	exp := time.Now().Add(s.lifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     sso.CookieName,
		Value:    sso.Sign(out.Username, exp, s.secret),
		Domain:   domain,
		Path:     "/",
		MaxAge:   int(s.lifetime / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	log.Printf("token-login: session issued for %s (domain %s)", out.Username, domain)
	target := "/login?ok=1"
	if s.validRedirect(redirect, domain) {
		target = redirect
	}
	http.Redirect(w, r, target, http.StatusFound)
}

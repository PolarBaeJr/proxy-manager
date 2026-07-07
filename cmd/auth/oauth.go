// Minimal stateless OAuth 2.0 authorization server for MCP clients
// (claude.ai custom connectors, Claude Code, MCP Inspector). Public clients
// only: dynamic registration (RFC 7591) packs the whole registration into a
// signed pmgcl_ client_id, authorization code + PKCE S256, refresh-token
// rotation. All tokens are HMAC blobs (internal/sso/token.go) — the only
// server-side state is the used-JTI set that makes codes and refresh tokens
// single-use, which degrades safely (a wiped entry just re-allows a token
// that still verifies).
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

const (
	authCodeTTL = 60 * time.Second
	maxUsedJTI  = 8192 // cap like the proxy's bearer cache — bounded memory under abuse
)

// consentPage is rendered via Fprintf: %s = client name, %s = audience text,
// %s = username (all pre-escaped), %s = hidden-fields HTML. Same visual
// system as loginPage.
const consentPage = `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Authorize access</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{width:100%%;max-width:380px}
  .code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.12em;color:#6a6a6a;margin-bottom:14px;text-transform:uppercase}
  h1{margin:0 0 14px;font-size:20px;font-weight:600;letter-spacing:-.015em;color:#fafafa}
  p{margin:0 0 18px;color:#8a8a8a;font-size:14px}
  p b{color:#e6e6e6;font-weight:600}
  .actions{display:flex;gap:10px}
  button{flex:1;padding:10px;border:0;border-radius:8px;font-weight:600;font-size:14px;font-family:inherit;cursor:pointer}
  .approve{background:#fafafa;color:#0a0a0a}
  .approve:hover{background:#e2e2e2}
  .deny{background:#141414;color:#e6e6e6;border:1px solid #262626}
  .deny:hover{border-color:#3f3f3f}
</style>
<div class=box>
  <div class=code>proxy-manager</div>
  <h1>Authorize access</h1>
  <p><b>%s</b> wants to access <b>%s</b> as <b>%s</b>.</p>
  <form method=post action=/oauth/authorize>
    %s<div class=actions>
      <button type=submit name=decision value=approve class=approve>Approve</button>
      <button type=submit name=decision value=deny class=deny>Deny</button>
    </div>
  </form>
</div>
`

// oauthErrorPage is rendered via Fprintf: %s = escaped reason. Used for
// authorize failures that must NOT redirect (bad client_id / redirect_uri,
// RFC 6749 §4.1.2.1).
const oauthErrorPage = `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Authorization error</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{max-width:420px;text-align:center}
  .code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.12em;color:#6a6a6a;margin-bottom:14px;text-transform:uppercase}
  h1{margin:0 0 10px;font-size:20px;font-weight:600;letter-spacing:-.015em;color:#fafafa}
  p{margin:0;color:#8a8a8a;font-size:14px}
</style>
<div class=box>
  <div class=code>proxy-manager</div>
  <h1>Authorization error</h1>
  <p>%s</p>
</div>
`

// corsOAuth sets permissive CORS on the metadata/register/token endpoints
// (MCP Inspector runs in a browser; claude.ai calls server-side). Returns
// true when the request was an OPTIONS preflight and is fully handled.
func corsOAuth(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// handleASMetadata serves RFC 8414 authorization-server metadata. Registered
// for both /.well-known/oauth-authorization-server and
// /.well-known/openid-configuration, including path-suffixed variants —
// clients derive these paths from issuer URLs in several ways.
func (s *loginServer) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	if corsOAuth(w, r) {
		return
	}
	if s.matchDomain(hostOnly(r.Host)) == "" {
		http.NotFound(w, r)
		return
	}
	base := "https://" + r.Host
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=300")
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{},
	})
}

// handleRegister is RFC 7591 dynamic client registration. The response
// client_id is a signed blob carrying the redirect URIs and name — nothing
// is stored.
func (s *loginServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if corsOAuth(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		registerError(w, "invalid_client_metadata", "body must be a JSON object under 16KiB")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		registerError(w, "invalid_redirect_uri", "redirect_uris must contain 1-10 entries")
		return
	}
	for _, ru := range req.RedirectURIs {
		if reason := badRedirectURI(ru); reason != "" {
			registerError(w, "invalid_redirect_uri", reason)
			return
		}
	}
	name := sanitizeClientName(req.ClientName)
	now := time.Now().Unix()
	clientID := sso.SignClient(sso.ClientReg{RedirectURIs: req.RedirectURIs, Name: name, IssuedAt: now}, s.secret)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        now,
		"redirect_uris":              req.RedirectURIs,
		"client_name":                name,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

func registerError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// badRedirectURI returns a rejection reason, "" if the URI is acceptable:
// absolute, no fragment, https:// anywhere or http:// only on loopback
// (Claude Code's local callback listener).
func badRedirectURI(raw string) string {
	if len(raw) > 512 {
		return "redirect_uri exceeds 512 characters"
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return "redirect_uri must be an absolute URL"
	}
	if u.Fragment != "" {
		return "redirect_uri must not contain a fragment"
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
			return "http redirect_uri is only allowed for localhost/127.0.0.1"
		}
	default:
		return "redirect_uri scheme must be https (or http on loopback)"
	}
	return ""
}

func sanitizeClientName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	for len(name) > 128 || (len(name) > 0 && !utf8.ValidString(name)) {
		name = name[:len(name)-1]
	}
	return name
}

// redirectURIMatches reports whether presented matches one of the registered
// redirect URIs. Exact match, except loopback URIs (http://localhost or
// http://127.0.0.1) compare scheme+host+path with ANY port (RFC 8252 §7.3 —
// Claude Code binds a random local port per run).
func redirectURIMatches(registered []string, presented string) bool {
	pu, err := url.Parse(presented)
	if err != nil {
		return false
	}
	for _, reg := range registered {
		if reg == presented {
			return true
		}
		ru, err := url.Parse(reg)
		if err != nil || ru.Scheme != "http" || pu.Scheme != "http" {
			continue
		}
		h := ru.Hostname()
		if h != "localhost" && h != "127.0.0.1" {
			continue
		}
		if pu.Hostname() == h && pu.Path == ru.Path {
			return true
		}
	}
	return false
}

// authzParams is the validated slice of an authorize request. Re-derived on
// both GET (query) and POST (hidden fields) — the fields are client-
// controlled either way.
type authzParams struct {
	clientID      string
	clientName    string
	redirectURI   string
	state         string
	scope         string
	codeChallenge string
	audience      string // resource host, or "*" when no resource was given
}

// validateAuthzParams applies authorize steps 1-2. hardErr non-empty → the
// caller must render a 400 page and NEVER redirect (client_id/redirect_uri
// are unverified, RFC 6749 §4.1.2.1). redirectErr non-empty → 302 back to
// the now-verified redirect_uri with that error code.
func (s *loginServer) validateAuthzParams(v url.Values) (p authzParams, hardErr, redirectErr string) {
	p.clientID = v.Get("client_id")
	reg, ok := sso.VerifyClient(p.clientID, s.secret)
	if !ok {
		return p, "Unknown or invalid client_id.", ""
	}
	p.clientName = reg.Name
	p.redirectURI = v.Get("redirect_uri")
	if !redirectURIMatches(reg.RedirectURIs, p.redirectURI) {
		return p, "redirect_uri is not registered for this client.", ""
	}
	p.state = v.Get("state")
	p.scope = v.Get("scope")
	p.codeChallenge = v.Get("code_challenge")
	if v.Get("response_type") != "code" {
		return p, "", "unsupported_response_type"
	}
	if p.codeChallenge == "" || v.Get("code_challenge_method") != "S256" {
		return p, "", "invalid_request"
	}
	aud, ok := s.validateResource(v.Get("resource"))
	if !ok {
		return p, "", "invalid_target"
	}
	p.audience = aud
	return p, "", ""
}

// validateResource maps the RFC 8707 resource parameter to a token audience:
// absent → "*" (all protected hosts); present → must be https, under a
// cookie domain, and routed by the proxy (soft-skipped when /routes is
// unreachable, like validRedirect).
func (s *loginServer) validateResource(raw string) (audience string, ok bool) {
	if raw == "" {
		return "*", true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if s.matchDomain(host) == "" {
		return "", false
	}
	if hosts, ok := s.routedHosts(); ok && !hosts[host] {
		return "", false
	}
	return host, true
}

func (s *loginServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		s.authorizeGET(w, r)
	case "POST":
		s.authorizePOST(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *loginServer) authorizeGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, hardErr, redirectErr := s.validateAuthzParams(q)
	if hardErr != "" {
		renderOAuthError(w, hardErr)
		return
	}
	if redirectErr != "" {
		redirectBack(w, r, p.redirectURI, url.Values{"error": {redirectErr}}, p.state)
		return
	}
	user := s.ssoUser(r)
	if user == "" {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(target), http.StatusFound)
		return
	}
	s.renderConsent(w, p, q, user)
}

func (s *loginServer) ssoUser(r *http.Request) string {
	c, err := r.Cookie(sso.CookieName)
	if err != nil {
		return ""
	}
	user, _ := sso.Verify(c.Value, s.secret)
	return user
}

// renderConsent issues a fresh CSRF token scoped to /oauth/authorize (NOT
// the /login-scoped cookie — same name, different Path, so the browser keeps
// them separate) and echoes every authorize parameter as a hidden field.
func (s *loginServer) renderConsent(w http.ResponseWriter, p authzParams, params url.Values, user string) {
	token := newCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/oauth/authorize",
		MaxAge:   int(csrfMaxAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	var fields strings.Builder
	fields.WriteString(hiddenField("csrf", token))
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "code_challenge", "code_challenge_method", "state", "scope", "resource"} {
		if v := params.Get(k); v != "" {
			fields.WriteString(hiddenField(k, v))
		}
	}
	name := p.clientName
	if name == "" {
		name = "An application"
	}
	audText := p.audience
	if audText == "*" {
		audText = "all protected hosts"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, consentPage,
		html.EscapeString(name), html.EscapeString(audText), html.EscapeString(user), fields.String())
}

func hiddenField(name, value string) string {
	return "<input type=hidden name=" + name + " value=\"" + html.EscapeString(value) + "\">\n    "
}

func renderOAuthError(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, oauthErrorPage, html.EscapeString(reason))
}

// redirectBack 302s to the client's verified redirect_uri with the given
// extra params, propagating state when present.
func redirectBack(w http.ResponseWriter, r *http.Request, redirectURI string, extra url.Values, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		renderOAuthError(w, "Invalid redirect_uri.")
		return
	}
	q := u.Query()
	for k, vs := range extra {
		q.Set(k, vs[0])
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *loginServer) authorizePOST(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOK(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !csrfOK(r) {
		http.Error(w, "csrf token mismatch", http.StatusForbidden)
		return
	}
	user := s.ssoUser(r)
	if user == "" {
		http.Error(w, "not signed in", http.StatusForbidden)
		return
	}
	// Hidden fields are client-controlled: re-run the full validation.
	p, hardErr, redirectErr := s.validateAuthzParams(r.PostForm)
	if hardErr != "" {
		renderOAuthError(w, hardErr)
		return
	}
	if redirectErr != "" {
		redirectBack(w, r, p.redirectURI, url.Values{"error": {redirectErr}}, p.state)
		return
	}
	if r.PostFormValue("decision") != "approve" {
		redirectBack(w, r, p.redirectURI, url.Values{"error": {"access_denied"}}, p.state)
		return
	}
	code := sso.SignCode(sso.CodeClaims{
		Username:      user,
		ClientID:      p.clientID,
		Audience:      p.audience,
		Scope:         p.scope,
		RedirectURI:   p.redirectURI,
		CodeChallenge: p.codeChallenge,
		JTI:           newJTI(),
		Exp:           time.Now().Add(authCodeTTL).Unix(),
	}, s.secret)
	redirectBack(w, r, p.redirectURI, url.Values{"code": {code}}, p.state)
}

func newJTI() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *loginServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if corsOAuth(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if r.Method != "POST" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		tokenError(w, "invalid_request")
		return
	}
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.tokenAuthCode(w, r)
	case "refresh_token":
		s.tokenRefresh(w, r)
	default:
		tokenError(w, "unsupported_grant_type")
	}
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (s *loginServer) tokenAuthCode(w http.ResponseWriter, r *http.Request) {
	claims, ok := sso.VerifyCode(r.PostFormValue("code"), s.secret)
	if !ok {
		tokenError(w, "invalid_grant")
		return
	}
	// Burn the code BEFORE the client/redirect/PKCE checks: any failed
	// attempt spends it, so an intercepted code can't be replayed while
	// brute-forcing verifiers (single-use per RFC 6749 §4.1.2).
	if !s.markJTIUsed(claims.JTI, time.Unix(claims.Exp, 0)) {
		tokenError(w, "invalid_grant")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("client_id")), []byte(claims.ClientID)) != 1 {
		tokenError(w, "invalid_grant")
		return
	}
	if !redirectURIMatches([]string{claims.RedirectURI}, r.PostFormValue("redirect_uri")) {
		tokenError(w, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(computed), []byte(claims.CodeChallenge)) != 1 {
		tokenError(w, "invalid_grant")
		return
	}
	s.issueTokens(w, claims.Username, claims.ClientID, claims.Audience, claims.Scope)
}

func (s *loginServer) tokenRefresh(w http.ResponseWriter, r *http.Request) {
	claims, ok := sso.VerifyRefresh(r.PostFormValue("refresh_token"), s.secret)
	if !ok {
		tokenError(w, "invalid_grant")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("client_id")), []byte(claims.ClientID)) != 1 {
		tokenError(w, "invalid_grant")
		return
	}
	// Rotation: the old refresh token is burned and a fresh pair is issued.
	if !s.markJTIUsed(claims.JTI, time.Unix(claims.Exp, 0)) {
		tokenError(w, "invalid_grant")
		return
	}
	s.issueTokens(w, claims.Username, claims.ClientID, claims.Audience, claims.Scope)
}

func (s *loginServer) issueTokens(w http.ResponseWriter, username, clientID, audience, scope string) {
	now := time.Now()
	access := sso.SignAccess(sso.AccessClaims{
		Username: username, ClientID: clientID, Audience: audience, Scope: scope,
		Exp: now.Add(s.accessTTL).Unix(),
	}, s.secret)
	refresh := sso.SignRefresh(sso.RefreshClaims{
		Username: username, ClientID: clientID, Audience: audience, Scope: scope,
		JTI: newJTI(), Exp: now.Add(s.refreshTTL).Unix(),
	}, s.secret)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(s.accessTTL / time.Second),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// markJTIUsed records a code/refresh JTI as consumed until exp (after which
// the blob itself no longer verifies, so the entry is dead weight). Returns
// false when the JTI was already used. Eviction mirrors authGate.bearers in
// the proxy: at capacity sweep expired entries, and only if that frees
// nothing wipe the map — the cost is re-allowing still-valid tokens, never
// unbounded growth.
func (s *loginServer) markJTIUsed(jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	now := time.Now()
	s.jtiMu.Lock()
	defer s.jtiMu.Unlock()
	if until, ok := s.usedJTI[jti]; ok && now.Before(until) {
		return false
	}
	if len(s.usedJTI) >= maxUsedJTI {
		for k, until := range s.usedJTI {
			if now.After(until) {
				delete(s.usedJTI, k)
			}
		}
		if len(s.usedJTI) >= maxUsedJTI {
			s.usedJTI = map[string]time.Time{}
		}
	}
	s.usedJTI[jti] = exp
	return true
}

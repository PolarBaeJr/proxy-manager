package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// csrfCookie holds the double-submit CSRF token for POST /login. Host-only
// (no Domain attribute) so sibling subdomains can't plant it, and short-lived
// — a fresh token is issued every time the form is rendered.
const (
	csrfCookie = "pmgr_csrf"
	csrfMaxAge = 10 * time.Minute
)

type loginServer struct {
	secret       []byte
	domains      []string // lowercased parent domains
	dashboardURL string
	routesURL    string
	lifetime     time.Duration
	accessTTL    time.Duration // OAuth access token lifetime
	refreshTTL   time.Duration // OAuth refresh token lifetime
	client       *http.Client // dashboard login calls
	routesClient *http.Client // proxy /routes fetches

	mu       sync.Mutex
	hosts    map[string]bool // routed hosts from the proxy, lowercased
	hostsAt  time.Time

	jtiMu   sync.Mutex
	usedJTI map[string]time.Time // consumed code/refresh JTIs → their expiry
}

// loginPage is rendered via Fprintf: %s = message HTML, %s = CSRF token (hex),
// %s = escaped redirect.
// Styled after the proxy's serveUnavailable page — self-contained, no JS, no
// external assets.
const loginPage = `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Sign in</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{width:100%%;max-width:340px}
  .code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.12em;color:#6a6a6a;margin-bottom:14px;text-transform:uppercase}
  h1{margin:0 0 18px;font-size:20px;font-weight:600;letter-spacing:-.015em;color:#fafafa}
  label{display:block;margin:0 0 6px;color:#8a8a8a;font-size:13px}
  label .hint{color:#5f5f5f}
  input{width:100%%;box-sizing:border-box;margin-bottom:14px;padding:9px 11px;border-radius:8px;border:1px solid #262626;background:#141414;color:#e6e6e6;font:inherit}
  input:focus{outline:none;border-color:#3f3f3f}
  button{width:100%%;padding:10px;border:0;border-radius:8px;background:#fafafa;color:#0a0a0a;font-weight:600;font-size:14px;font-family:inherit;cursor:pointer}
  button:hover{background:#e2e2e2}
  .msg{margin:0 0 14px;font-size:13px;color:#f87171}
  .msg.ok{color:#4ade80}
</style>
<div class=box>
  <div class=code>proxy-manager</div>
  <h1>Sign in</h1>
  %s<form method=post action=/login>
    <label for=u>Username</label>
    <input id=u name=username autocomplete=username autofocus required>
    <label for=p>Password</label>
    <input id=p name=password type=password autocomplete=current-password required>
    <label for=c>2FA code <span class=hint>(if enabled)</span></label>
    <input id=c name=code inputmode=numeric pattern="[0-9]*" maxlength=6 autocomplete=one-time-code>
    <input type=hidden name=csrf value="%s">
    <input type=hidden name=redirect value="%s">
    <button type=submit>Sign in</button>
  </form>
</div>
`

const logoutPage = `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Signed out</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{max-width:420px;text-align:center}
  .code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.12em;color:#6a6a6a;margin-bottom:14px;text-transform:uppercase}
  h1{margin:0 0 10px;font-size:20px;font-weight:600;letter-spacing:-.015em;color:#fafafa}
  p{margin:0;color:#8a8a8a;font-size:14px}
  a{color:#e6e6e6}
</style>
<div class=box>
  <div class=code>proxy-manager</div>
  <h1>Signed out</h1>
  <p><a href="/login">Sign in again</a></p>
</div>
`

func hostOnly(s string) string {
	if i := strings.IndexByte(s, ':'); i != -1 {
		return s[:i]
	}
	return s
}

func (s *loginServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		msg := ""
		switch {
		case r.URL.Query().Get("err") == "1":
			msg = "<p class=msg>Invalid credentials.</p>"
		case r.URL.Query().Get("ok") == "1":
			msg = "<p class=\"msg ok\">Logged in. You can close this tab.</p>"
		}
		s.renderLogin(w, msg, r.URL.Query().Get("redirect"))
	case "POST":
		s.doLogin(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// renderLogin issues a fresh CSRF token on every render: set as a host-only
// cookie and embedded in the form (double-submit pattern, no server state).
func (s *loginServer) renderLogin(w http.ResponseWriter, msgHTML, redirect string) {
	token := newCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/login",
		MaxAge:   int(csrfMaxAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, loginPage, msgHTML, token, html.EscapeString(redirect))
}

func newCSRFToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sameOriginOK rejects browser-originated cross-site POSTs. Origin, when
// present, must be exactly this host over https; otherwise Referer (if
// present) must match the same way. Requests carrying neither header
// (curl, health checks) pass — the double-submit token still applies.
func sameOriginOK(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == "https://"+r.Host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Scheme == "https" && u.Host == r.Host
	}
	return true
}

// csrfOK verifies the double-submit pair: pmgr_csrf cookie == hidden form
// field, non-empty, constant-time.
func csrfOK(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) == 1
}

func (s *loginServer) doLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOK(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	code := r.PostFormValue("code")
	redirect := r.PostFormValue("redirect")

	// Missing/stale/mismatched CSRF token: bounce back to GET /login, which
	// issues a fresh token, rather than dead-ending with a 403.
	if !csrfOK(r) {
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(redirect), http.StatusFound)
		return
	}

	domain := s.matchDomain(hostOnly(r.Host))
	if domain == "" {
		http.Error(w, "unrecognized host", http.StatusBadRequest)
		return
	}

	body, _ := json.Marshal(map[string]string{"username": username, "password": password, "code": code})
	resp, err := s.client.Post(s.dashboardURL+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("login: dashboard unreachable: %v", err)
		http.Redirect(w, r, "/login?err=1&redirect="+url.QueryEscape(redirect), http.StatusFound)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		s.renderLogin(w, "<p class=msg>Too many attempts, try again shortly.</p>", redirect)
		return
	default:
		http.Redirect(w, r, "/login?err=1&redirect="+url.QueryEscape(redirect), http.StatusFound)
		return
	}

	// The dashboard returns 200 on password success alone; 2FA is enforced
	// here. Fail closed: if the response doesn't carry the enrollment signal
	// (parse error, older dashboard), reject rather than mint a session.
	var out struct {
		TOTPEnrolled *bool `json:"totp_enrolled"`
		CodeValid    bool  `json:"code_valid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.TOTPEnrolled == nil {
		log.Printf("login: dashboard response missing 2FA signal (err=%v)", err)
		http.Redirect(w, r, "/login?err=1&redirect="+url.QueryEscape(redirect), http.StatusFound)
		return
	}
	// Enrolled users must supply a valid TOTP code. The generic err=1 page
	// deliberately doesn't reveal that the password itself was correct.
	if *out.TOTPEnrolled && !out.CodeValid {
		http.Redirect(w, r, "/login?err=1&redirect="+url.QueryEscape(redirect), http.StatusFound)
		return
	}

	exp := time.Now().Add(s.lifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     sso.CookieName,
		Value:    sso.Sign(username, exp, s.secret),
		Domain:   domain,
		Path:     "/",
		MaxAge:   int(s.lifetime / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	target := "/login?ok=1"
	if s.validRedirect(redirect, domain) {
		target = redirect
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *loginServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	domain := s.matchDomain(hostOnly(r.Host))
	if domain == "" {
		http.Error(w, "unrecognized host", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sso.CookieName, Value: "", Domain: domain, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, logoutPage)
}

// matchDomain returns the -cookie-domains entry that the request host belongs
// to, "" if none.
func (s *loginServer) matchDomain(host string) string {
	host = strings.ToLower(host)
	for _, d := range s.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return d
		}
	}
	return ""
}

// validRedirect accepts only absolute https URLs whose host (a) belongs to
// the cookie domain being set, and (b) is actually routed by the proxy —
// otherwise the login page would be an open redirector. When the proxy's
// /routes endpoint is unreachable, (b) is skipped and (a) alone decides.
func (s *loginServer) validRedirect(raw, domain string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != domain && !strings.HasSuffix(host, "."+domain) {
		return false
	}
	if hosts, ok := s.routedHosts(); ok && !hosts[host] {
		return false
	}
	return true
}

// routedHosts fetches (and caches for 30s) the set of hosts the proxy
// currently routes. ok=false means the fetch failed and no cache exists.
func (s *loginServer) routedHosts() (map[string]bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hosts != nil && time.Since(s.hostsAt) < 30*time.Second {
		return s.hosts, true
	}
	resp, err := s.routesClient.Get(s.routesURL)
	if err != nil {
		log.Printf("routes fetch: %v", err)
		return s.hosts, s.hosts != nil
	}
	defer resp.Body.Close()
	var out struct {
		Hosts []string `json:"hosts"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		log.Printf("routes fetch: bad response (status %d)", resp.StatusCode)
		return s.hosts, s.hosts != nil
	}
	set := make(map[string]bool, len(out.Hosts))
	for _, h := range out.Hosts {
		set[strings.ToLower(h)] = true
	}
	s.hosts = set
	s.hostsAt = time.Now()
	return set, true
}

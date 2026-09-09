package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// newTestLoginServer builds a portal wired to `dashboardURL`. routesURL points
// at a dead port on purpose: routedHosts then fails, so validRedirect falls
// through to the cookie-domain check alone (the documented soft-skip). Both
// clients must be non-nil — routedHosts calls routesClient.Get unconditionally.
func newTestLoginServer(t *testing.T, dashboardURL string) *loginServer {
	t.Helper()
	return &loginServer{
		secret:       []byte("0123456789abcdef0123456789abcdef"),
		domains:      []string{"polardev.org"},
		dashboardURL: dashboardURL,
		routesURL:    "http://127.0.0.1:1/routes",
		lifetime:     time.Hour,
		client:       &http.Client{Timeout: 2 * time.Second},
		routesClient: &http.Client{Timeout: 100 * time.Millisecond},
		usedJTI:      map[string]time.Time{},
	}
}

// stubDashboard records what the portal actually sent, so a URL typo can't
// masquerade as a fail-closed rejection.
type stubDashboard struct {
	*httptest.Server
	mu    sync.Mutex
	calls int
	path  string
	body  string
}

func newStubDashboard(t *testing.T, status int, respBody string) *stubDashboard {
	t.Helper()
	s := &stubDashboard{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls++
		s.path = r.URL.Path
		s.body = string(b)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubDashboard) seen() (calls int, path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.path, s.body
}

func tokenLoginRequest(host, token, redirect string) *http.Request {
	form := url.Values{"token": {token}, "redirect": {redirect}}
	req := httptest.NewRequest("POST", "https://"+host+"/login/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func ssoCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sso.CookieName {
			return c
		}
	}
	return nil
}

func TestTokenLoginMintsSessionForElevatedToken(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/login?ok=1" {
		t.Errorf("Location = %q, want %q", got, "/login?ok=1")
	}
	c := ssoCookie(rec)
	if c == nil {
		t.Fatal("no SSO cookie set")
	}
	user, ok := sso.Verify(c.Value, s.secret)
	if !ok || user != "alice" {
		t.Fatalf("sso.Verify = (%q, %v), want (%q, true)", user, ok, "alice")
	}
}

// The cookie must be indistinguishable from one doLogin mints — the passkey
// endpoints gate on nothing but sso.Verify of this cookie.
func TestTokenLoginCookieShapeMatchesPasswordLogin(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))

	c := ssoCookie(rec)
	if c == nil {
		t.Fatal("no SSO cookie set")
	}
	if c.Domain != "polardev.org" {
		t.Errorf("Domain = %q, want %q", c.Domain, "polardev.org")
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want %q", c.Path, "/")
	}
	if !c.HttpOnly {
		t.Error("HttpOnly = false")
	}
	if !c.Secure {
		t.Error("Secure = false")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if want := int(time.Hour / time.Second); c.MaxAge != want {
		t.Errorf("MaxAge = %d, want %d", c.MaxAge, want)
	}
}

func TestTokenLoginRejectsNonElevatedToken(t *testing.T) {
	// statusbot's auto-provisioned credential resolves to an identity but is
	// not elevated; it must never mint a session.
	stub := newStubDashboard(t, http.StatusOK, `{"username":"statusbot","elevated":false}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_service", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("service token minted an SSO cookie")
	}
}

// An older dashboard image omits "elevated" entirely — fail closed rather than
// treating the absent field as permission.
func TestTokenLoginRejectsMissingElevationField(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice"}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted without an elevation signal")
	}
}

func TestTokenLoginRejectsDashboardError(t *testing.T) {
	stub := newStubDashboard(t, http.StatusInternalServerError, `{}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted on a dashboard error")
	}
}

func TestTokenLoginRejectsUnreachableDashboard(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	dashURL := stub.URL
	stub.Close()
	s := newTestLoginServer(t, dashURL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted with the dashboard unreachable")
	}
}

func TestTokenLoginRejectsEmptyUsername(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted for an empty username")
	}
}

func TestTokenLoginRejectsNonTokenInputWithoutCallingDashboard(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "hunter2", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if calls, _, _ := stub.seen(); calls != 0 {
		t.Errorf("dashboard called %d times for input that isn't an API token", calls)
	}
}

func TestTokenLoginRejectsGET(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, httptest.NewRequest("GET", "https://auth.polardev.org/login/token", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestTokenLoginRejectsCrossOrigin(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	req := tokenLoginRequest("auth.polardev.org", "pmt_good", "")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted for a cross-origin POST")
	}
}

func TestTokenLoginRejectsUnknownHost(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.example.net", "pmt_good", ""))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted for an unrecognized host")
	}
}

func TestTokenLoginRedirectHonoursValidTarget(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	target := "https://auth.polardev.org/oauth/authorize?x=1"
	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", target))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != target {
		t.Errorf("Location = %q, want %q", got, target)
	}
}

func TestTokenLoginRefusesOffDomainRedirect(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", "https://evil.com/"))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/login?ok=1" {
		t.Errorf("Location = %q, want %q — the portal must not be an open redirector", got, "/login?ok=1")
	}
}

// The CSRF check is conditional: enforced whenever the browser sent the
// login page's cookie, skipped for the cookie-less curl recovery path.
func TestTokenLoginEnforcesCSRFWhenCookiePresent(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	req := tokenLoginRequest("auth.polardev.org", "pmt_good", "")
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "expected"})
	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/login?redirect=" {
		t.Errorf("Location = %q, want a bounce back to a freshly-tokened login page", got)
	}
	if c := ssoCookie(rec); c != nil {
		t.Fatal("cookie minted despite a CSRF mismatch")
	}
	if calls, _, _ := stub.seen(); calls != 0 {
		t.Errorf("dashboard called %d times on a CSRF mismatch", calls)
	}
}

// The browser path: the login page's form submits the cookie AND a matching
// hidden field. If the two ever diverged the form would bounce back to /login
// forever, and every other test here sends no cookie at all.
func TestTokenLoginAcceptsMatchingCSRF(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	form := url.Values{"token": {"pmt_good"}, "redirect": {""}, "csrf": {"tok123"}}
	req := httptest.NewRequest("POST", "https://auth.polardev.org/login/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok123"})
	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ssoCookie(rec) == nil {
		t.Fatal("no SSO cookie set on the browser path")
	}
}

// A wrong verification URL would still fail closed with a 401, which looks like
// a correct rejection. Pin the outbound call instead.
func TestTokenLoginCallsVerifyTokenEndpoint(t *testing.T) {
	stub := newStubDashboard(t, http.StatusOK, `{"username":"alice","elevated":true}`)
	s := newTestLoginServer(t, stub.URL)

	rec := httptest.NewRecorder()
	s.handleTokenLogin(rec, tokenLoginRequest("auth.polardev.org", "pmt_good", ""))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	calls, path, body := stub.seen()
	if calls != 1 {
		t.Fatalf("dashboard called %d times, want 1", calls)
	}
	if path != "/api/auth/verify-token" {
		t.Errorf("path = %q, want %q", path, "/api/auth/verify-token")
	}
	if !strings.Contains(body, "pmt_good") {
		t.Errorf("request body %q does not carry the token", body)
	}
}

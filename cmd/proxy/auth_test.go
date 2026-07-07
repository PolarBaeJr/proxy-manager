package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

const authTestHost = "mcp.example.org"

var authTestSecret = []byte("0123456789abcdef0123456789abcdef")

func authFuture() time.Time { return time.Now().Add(time.Hour) }

// authorize() resolves pmt_ tokens over HTTP against the dashboard verifyURL;
// this stub stands in for it, echoing a fixed username on 200.
func newBearerVerifyServer(username string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"username":"` + username + `"}`))
	}))
}

func TestAuthorize(t *testing.T) {
	verifySrv := newBearerVerifyServer("alice")
	defer verifySrv.Close()

	cookieAlice := sso.Sign("alice", authFuture(), authTestSecret)
	cookieBob := sso.Sign("bob", authFuture(), authTestSecret)
	pmgaGood := sso.SignAccess(sso.AccessClaims{Username: "alice", Audience: authTestHost, Exp: authFuture().Unix()}, authTestSecret)
	pmgaWrongAud := sso.SignAccess(sso.AccessClaims{Username: "alice", Audience: "other.example.org", Exp: authFuture().Unix()}, authTestSecret)

	cases := []struct {
		name string

		mode       string
		authUsers  []string
		trustedCSV string

		cookie     string // raw pmgr_sso cookie value, "" to omit
		bearer     string // Authorization token after "Bearer ", "" to omit
		remoteAddr string

		wantAuthorized bool
		wantStatus     int  // asserted only when !wantAuthorized
		wantWWWAuth    bool // WWW-Authenticate header must be present
	}{
		{
			name:           "oauth no credentials denied",
			mode:           "oauth",
			wantAuthorized: false,
			wantStatus:     http.StatusUnauthorized,
			wantWWWAuth:    true,
		},
		{
			name:           "oauth valid cookie does not grant access",
			mode:           "oauth",
			cookie:         cookieAlice,
			wantAuthorized: false,
			wantStatus:     http.StatusUnauthorized,
			wantWWWAuth:    true,
		},
		{
			name:           "oauth valid pmga token allowed",
			mode:           "oauth",
			bearer:         pmgaGood,
			wantAuthorized: true,
		},
		{
			name:           "oauth pmga wrong audience denied",
			mode:           "oauth",
			bearer:         pmgaWrongAud,
			wantAuthorized: false,
			wantStatus:     http.StatusUnauthorized,
			wantWWWAuth:    true,
		},
		{
			// RemoteAddr 10.1.2.3 sits inside the trusted CIDR and is not a
			// trusted XFF hop, so realClientIP returns it directly — the LAN
			// bypass would fire in sso mode but must not in oauth mode.
			name:           "oauth trusted CIDR does not bypass",
			mode:           "oauth",
			trustedCSV:     "10.0.0.0/8",
			remoteAddr:     "10.1.2.3:5555",
			wantAuthorized: false,
			wantStatus:     http.StatusUnauthorized,
			wantWWWAuth:    true,
		},
		{
			name:           "sso valid cookie allowed",
			mode:           "",
			cookie:         cookieAlice,
			wantAuthorized: true,
		},
		{
			name:           "sso trusted CIDR bypass allowed",
			mode:           "",
			trustedCSV:     "10.0.0.0/8",
			remoteAddr:     "10.1.2.3:5555",
			wantAuthorized: true,
		},
		{
			name:           "sso allowlist rejects other user",
			mode:           "",
			authUsers:      []string{"alice"},
			cookie:         cookieBob,
			wantAuthorized: false,
			wantStatus:     http.StatusForbidden,
		},
		{
			name:           "sso valid pmt token allowed",
			mode:           "",
			bearer:         "pmt_sometoken",
			wantAuthorized: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gate := newAuthGate(authTestSecret, "", c.trustedCSV, "", verifySrv.URL)
			group := &RouteGroup{Host: authTestHost, AuthRequired: true, AuthMode: c.mode, AuthUsers: c.authUsers}

			req := httptest.NewRequest(http.MethodGet, "http://"+authTestHost+"/", nil)
			if c.remoteAddr != "" {
				req.RemoteAddr = c.remoteAddr
			}
			if c.cookie != "" {
				req.AddCookie(&http.Cookie{Name: sso.CookieName, Value: c.cookie})
			}
			if c.bearer != "" {
				req.Header.Set("Authorization", bearerPrefix+c.bearer)
			}

			rec := httptest.NewRecorder()
			got := gate.authorize(rec, req, group)
			if got != c.wantAuthorized {
				t.Fatalf("authorize() = %v, want %v", got, c.wantAuthorized)
			}
			if !c.wantAuthorized {
				if rec.Code != c.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
				}
				if c.wantWWWAuth && rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("missing WWW-Authenticate header")
				}
			}
		})
	}
}

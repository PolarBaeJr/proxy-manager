// auth: SSO login portal for proxy.auth=true hosts.
// Verifies credentials against the dashboard's /api/auth/login and issues a
// domain-wide HMAC-signed cookie (pmgr_sso) that the proxy verifies with the
// same shared PMGR_AUTH_SECRET. Stateless — sessions live in the cookie, so
// no volume is needed.
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":8096", "listen address")
	cookieDomains := flag.String("cookie-domains", "polardev.org,the-aquarium.com", "comma-separated parent domains cookies may be set for")
	dashboardURL := flag.String("dashboard-url", "http://dashboard:8093", "dashboard base URL for credential verification")
	routesURL := flag.String("routes-url", "http://proxy:8094/routes", "proxy endpoint listing routed hosts (for redirect validation)")
	sessionLifetime := flag.Duration("session-lifetime", 720*time.Hour, "SSO cookie lifetime")
	accessTTL := flag.Duration("access-token-ttl", time.Hour, "OAuth access token lifetime")
	refreshTTL := flag.Duration("refresh-token-ttl", 720*time.Hour, "OAuth refresh token lifetime")
	passkeyDomains := flag.String("passkey-rp-domains", "", "comma-separated cookie domains to enable WebAuthn passkeys for (empty = disabled)")
	flag.Parse()

	envHex := strings.TrimSpace(os.Getenv("PMGR_AUTH_SECRET"))
	if envHex == "" {
		log.Fatal("PMGR_AUTH_SECRET is required (generate: openssl rand -hex 32)")
	}
	secret, err := hex.DecodeString(envHex)
	if err != nil || len(secret) == 0 {
		log.Fatalf("PMGR_AUTH_SECRET is not valid hex: %v", err)
	}

	s := &loginServer{
		secret:       secret,
		domains:      splitAndTrim(*cookieDomains),
		dashboardURL: strings.TrimRight(*dashboardURL, "/"),
		routesURL:    *routesURL,
		lifetime:     *sessionLifetime,
		accessTTL:    *accessTTL,
		refreshTTL:   *refreshTTL,
		client:       &http.Client{Timeout: 5 * time.Second},
		routesClient: &http.Client{Timeout: 2 * time.Second},
		usedJTI:      map[string]time.Time{},
	}
	if len(s.domains) == 0 {
		log.Fatal("-cookie-domains must list at least one parent domain")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	// Trailing-slash registrations catch the path-suffixed metadata variants
	// (issuer URLs with a path component).
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server/", s.handleASMetadata)
	mux.HandleFunc("/.well-known/openid-configuration", s.handleASMetadata)
	mux.HandleFunc("/.well-known/openid-configuration/", s.handleASMetadata)
	mux.HandleFunc("/oauth/register", s.handleRegister)
	mux.HandleFunc("/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/token", s.handleToken)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	// Bare root is the catch-all: send someone who typed just the login host
	// straight to the login page (carrying any ?redirect= through). Genuinely
	// unknown paths still get an honest 404 rather than a surprise redirect.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		target := "/login"
		if q := r.URL.RawQuery; q != "" {
			target += "?" + q
		}
		http.Redirect(w, r, target, http.StatusFound)
	})

	// Opt-in WebAuthn passkeys. Empty -passkey-rp-domains ⇒ no store opened, no
	// routes registered, portal stays byte-for-byte stateless.
	if pkDomains := splitAndTrim(*passkeyDomains); len(pkDomains) > 0 {
		store, err := loadPasskeyStore("/data/passkeys.json")
		if err != nil {
			log.Fatalf("passkey store: %v", err)
		}
		managers := buildPasskeyManagers(pkDomains, os.Getenv("PASSKEY_RP_ORIGINS"))
		if len(managers) > 0 {
			s.passkeyEnabled = true
			registerPasskeyRoutes(mux, s, managers, store, newRateLimiter(), s.dashboardURL)
			enabled := make([]string, 0, len(managers))
			for d := range managers {
				enabled = append(enabled, d)
			}
			log.Printf("passkey support enabled (rp domains: %s)", strings.Join(enabled, ", "))
		} else {
			log.Print("passkey support disabled (no usable rp domains)")
		}
	} else {
		log.Print("passkey support disabled")
	}

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("auth on %s (cookie domains: %s)", *addr, strings.Join(s.domains, ", "))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func splitAndTrim(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

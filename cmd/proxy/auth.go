// Auth gate for proxy.auth=true hosts. Verifies the domain-wide SSO cookie
// (signed by the auth binary with the shared PMGR_AUTH_SECRET) or a bearer
// API token (verified against the dashboard), with a trusted-CIDR LAN bypass.
// Enforcement is opt-in per host via labels/routes.json; with no protected
// hosts the gate is never consulted.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

const (
	bearerPrefix      = "Bearer "
	bearerPositiveTTL = 5 * time.Minute
	bearerNegativeTTL = 30 * time.Second
	maxBearerCache    = 4096 // scanners spraying random tokens can't grow the map unbounded
)

type bearerEntry struct {
	username string // "" = known-bad token (negative cache)
	expires  time.Time
}

type authGate struct {
	secret     []byte       // shared HMAC secret; nil = fail closed on protected hosts
	domains    []string     // parent domains that have an auth.<domain> login host
	trusted    []*net.IPNet // client IPs that bypass auth entirely (LAN)
	xffTrusted []*net.IPNet // peers whose X-Forwarded-For we believe (edge, loopback)
	verifyURL  string       // dashboard endpoint for bearer-token verification
	client     *http.Client

	warnNoSecret sync.Once
	warnAuthHost sync.Once

	mu      sync.Mutex
	bearers map[[32]byte]bearerEntry
}

func newAuthGate(secret []byte, domainsCSV, trustedCSV, xffTrustedCSV, verifyURL string) *authGate {
	var domains []string
	for _, d := range strings.Split(domainsCSV, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			domains = append(domains, d)
		}
	}
	return &authGate{
		secret:     secret,
		domains:    domains,
		trusted:    parseCIDRList(trustedCSV),
		xffTrusted: parseCIDRList(xffTrustedCSV),
		verifyURL:  verifyURL,
		client:     &http.Client{Timeout: 2 * time.Second},
		bearers:    map[[32]byte]bearerEntry{},
	}
}

func parseCIDRList(csv string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			log.Printf("auth: skip bad CIDR %q: %v", s, err)
			continue
		}
		out = append(out, n)
	}
	return out
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// realClientIP returns the IP auth decisions are based on. Unlike clientIP()
// in accesslog.go (first XFF hop, spoofable, logging-only), this trusts
// X-Forwarded-For only when the TCP peer is a known proxy hop, and then takes
// the RIGHTMOST entry — the one that trusted hop appended; everything left of
// it is client-controlled.
func realClientIP(r *http.Request, xffTrusted []*net.IPNet) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipInAny(peer, xffTrusted) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
		return ip
	}
	return peer
}

// authorize enforces auth for a protected group. Returns true to let the
// request through; on false the response has already been written.
func (a *authGate) authorize(w http.ResponseWriter, req *http.Request, group *RouteGroup) bool {
	reqHost := hostOnly(req.Host)

	// Never gate the login host itself or every redirect would loop.
	for _, d := range a.domains {
		if strings.EqualFold(reqHost, "auth."+d) {
			a.warnAuthHost.Do(func() {
				log.Printf("auth: host %s is the auth login host but carries proxy.auth=true — ignoring", reqHost)
			})
			return true
		}
	}

	if ip := realClientIP(req, a.xffTrusted); ip != nil && ipInAny(ip, a.trusted) {
		return true
	}

	if len(a.secret) == 0 {
		a.warnNoSecret.Do(func() {
			log.Printf("auth: PMGR_AUTH_SECRET unset — protected hosts fail closed with 503")
		})
		serveUnavailable(w, http.StatusServiceUnavailable, reqHost, "Service unavailable at this time, try again later.")
		return false
	}

	if group.AuthMode == "oauth" {
		return a.authorizeOAuth(w, req, group, reqHost)
	}

	if c, err := req.Cookie(sso.CookieName); err == nil {
		if user, ok := sso.Verify(c.Value, a.secret); ok {
			if userAllowed(group, user) {
				return true
			}
			// Valid session, wrong user: 403, not a login redirect — the
			// browser would come straight back with the same cookie and loop.
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	}

	if authz := req.Header.Get("Authorization"); strings.HasPrefix(authz, bearerPrefix+"pmt_") {
		if user := a.verifyBearer(strings.TrimPrefix(authz, bearerPrefix)); user != "" {
			if userAllowed(group, user) {
				return true
			}
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	}

	a.deny(w, req, reqHost)
	return false
}

func userAllowed(g *RouteGroup, user string) bool {
	if len(g.AuthUsers) == 0 {
		return true
	}
	for _, u := range g.AuthUsers {
		if strings.EqualFold(u, user) {
			return true
		}
	}
	return false
}

// deny sends browsers to the login host for the matching parent domain and
// everything else a 401 JSON.
func (a *authGate) deny(w http.ResponseWriter, req *http.Request, reqHost string) {
	if strings.Contains(req.Header.Get("Accept"), "text/html") {
		for _, d := range a.domains {
			if strings.EqualFold(reqHost, d) || strings.HasSuffix(strings.ToLower(reqHost), "."+d) {
				target := "https://" + req.Host + req.URL.RequestURI()
				http.Redirect(w, req, "https://auth."+d+"/login?redirect="+url.QueryEscape(target), http.StatusFound)
				return
			}
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
}

// verifyBearer resolves a pmt_ token to a username via the dashboard, with a
// small positive/negative cache so hot API clients don't hammer it.
func (a *authGate) verifyBearer(token string) string {
	key := sha256.Sum256([]byte(token))
	now := time.Now()

	a.mu.Lock()
	if e, ok := a.bearers[key]; ok && now.Before(e.expires) {
		a.mu.Unlock()
		return e.username
	}
	a.mu.Unlock()

	user := ""
	body, _ := json.Marshal(map[string]string{"token": token})
	resp, err := a.client.Post(a.verifyURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("auth: verify-token: %v", err)
	} else {
		if resp.StatusCode == http.StatusOK {
			var out struct {
				Username string `json:"username"`
			}
			if json.NewDecoder(resp.Body).Decode(&out) == nil {
				user = out.Username
			}
		}
		resp.Body.Close()
	}

	ttl := bearerNegativeTTL
	if user != "" {
		ttl = bearerPositiveTTL
	}
	a.mu.Lock()
	// At capacity: sweep expired entries first, and only if that frees
	// nothing wipe the whole cache (costs a re-verify per live token).
	if len(a.bearers) >= maxBearerCache {
		for k, e := range a.bearers {
			if now.After(e.expires) {
				delete(a.bearers, k)
			}
		}
		if len(a.bearers) >= maxBearerCache {
			a.bearers = map[[32]byte]bearerEntry{}
		}
	}
	a.bearers[key] = bearerEntry{username: user, expires: now.Add(ttl)}
	a.mu.Unlock()
	return user
}

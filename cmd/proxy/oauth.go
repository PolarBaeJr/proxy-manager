// OAuth resource-server support for proxy.auth.mode=oauth hosts (MCP
// servers behind the proxy). The proxy serves RFC 9728 protected-resource
// metadata pointing at the auth binary's authorization server, verifies
// pmga_ access tokens locally with the shared secret, and challenges with
// WWW-Authenticate instead of the browser login redirect.
package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

const protectedResourcePath = "/.well-known/oauth-protected-resource"

// handleOAuthWellKnown intercepts the OAuth well-known endpoints for hosts
// routed in oauth mode. Returns true when the request was fully handled.
// Called from ServeHTTP before group matching so it wins over any
// path-prefixed route and never touches the auth gate.
func (r *Router) handleOAuthWellKnown(w http.ResponseWriter, req *http.Request, groups []*RouteGroup, reqHost string) bool {
	if r.auth == nil || !strings.HasPrefix(req.URL.Path, "/.well-known/") {
		return false
	}
	isPRM := strings.HasPrefix(req.URL.Path, protectedResourcePath)
	isLegacyAS := strings.HasPrefix(req.URL.Path, "/.well-known/oauth-authorization-server") ||
		strings.HasPrefix(req.URL.Path, "/.well-known/openid-configuration")
	if !isPRM && !isLegacyAS {
		return false
	}
	oauthHost := false
	for _, g := range groups {
		if strings.EqualFold(reqHost, g.Host) && g.AuthMode == "oauth" {
			oauthHost = true
			break
		}
	}
	if !oauthHost {
		return false
	}
	if isPRM {
		r.auth.serveProtectedResourceMetadata(w, req)
	} else {
		r.auth.redirectASMetadata(w, req)
	}
	return true
}

// parentDomain returns the -auth-domains entry the host belongs to, "" if
// none (same suffix logic as deny()).
func (a *authGate) parentDomain(host string) string {
	for _, d := range a.domains {
		if strings.EqualFold(host, d) || strings.HasSuffix(strings.ToLower(host), "."+d) {
			return d
		}
	}
	return ""
}

// serveProtectedResourceMetadata is RFC 9728: tells MCP clients which
// authorization server protects this resource. Any path suffix after the
// well-known prefix identifies a sub-resource and is echoed back.
func (a *authGate) serveProtectedResourceMetadata(w http.ResponseWriter, req *http.Request) {
	host := hostOnly(req.Host)
	parent := a.parentDomain(host)
	if parent == "" {
		http.NotFound(w, req)
		return
	}
	suffix := strings.TrimPrefix(req.URL.Path, protectedResourcePath)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=300")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"resource":                 "https://" + host + suffix,
		"authorization_servers":    []string{"https://auth." + parent},
		"bearer_methods_supported": []string{"header"},
	})
}

// redirectASMetadata is the 2025-03-26 MCP spec fallback: older clients
// fetch authorization-server metadata from the resource host itself, so
// bounce them to the same path on the real AS.
func (a *authGate) redirectASMetadata(w http.ResponseWriter, req *http.Request) {
	host := hostOnly(req.Host)
	parent := a.parentDomain(host)
	if parent == "" {
		http.NotFound(w, req)
		return
	}
	http.Redirect(w, req, "https://auth."+parent+req.URL.Path, http.StatusFound)
}

// verifyOAuthBearer checks a pmga_ access token against the shared secret
// and this host: the audience must be the request host or the "*" wildcard
// minted when no resource parameter was given.
func (a *authGate) verifyOAuthBearer(token, reqHost string) (string, bool) {
	claims, ok := sso.VerifyAccess(token, a.secret)
	if !ok {
		return "", false
	}
	if claims.Audience != "*" && !strings.EqualFold(claims.Audience, reqHost) {
		return "", false
	}
	return claims.Username, true
}

// denyOAuth is the oauth-mode 401: the WWW-Authenticate challenge carries
// the resource-metadata URL that starts the client's discovery flow
// (RFC 9728 §5.1), plus error="invalid_token" when a bearer was presented
// but rejected.
func (a *authGate) denyOAuth(w http.ResponseWriter, reqHost string, hadBearer bool) {
	challenge := fmt.Sprintf("Bearer resource_metadata=%q", "https://"+reqHost+protectedResourcePath)
	if hadBearer {
		challenge += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
}

// authorizeOAuth is the oauth-mode counterpart of the sso branch in
// authorize(): SSO cookie, then pmt_ API token, then pmga_ OAuth access
// token. Never redirects to the login page, even for Accept: text/html —
// MCP clients follow the WWW-Authenticate challenge instead.
func (a *authGate) authorizeOAuth(w http.ResponseWriter, req *http.Request, group *RouteGroup, reqHost string) bool {
	if c, err := req.Cookie(sso.CookieName); err == nil {
		if user, ok := sso.Verify(c.Value, a.secret); ok {
			if userAllowed(group, user) {
				return true
			}
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	}

	authz := req.Header.Get("Authorization")
	hadBearer := strings.HasPrefix(authz, bearerPrefix)
	if strings.HasPrefix(authz, bearerPrefix+"pmt_") {
		if user := a.verifyBearer(strings.TrimPrefix(authz, bearerPrefix)); user != "" {
			if userAllowed(group, user) {
				return true
			}
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	} else if strings.HasPrefix(authz, bearerPrefix+sso.AccessTokenPrefix) {
		if user, ok := a.verifyOAuthBearer(strings.TrimPrefix(authz, bearerPrefix), reqHost); ok {
			if userAllowed(group, user) {
				return true
			}
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	}

	a.denyOAuth(w, reqHost, hadBearer)
	return false
}

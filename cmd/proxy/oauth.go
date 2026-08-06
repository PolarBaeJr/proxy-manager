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

// oauthResource is the RFC 8707 resource identifier for a routed group,
// without the scheme: the host for a host-wide route, host+prefix for a
// path-mounted one. It is what a token's audience must name.
func oauthResource(reqHost, pathPrefix string) string {
	return strings.ToLower(reqHost) + strings.TrimSuffix(pathPrefix, "/")
}

// verifyOAuthBearer checks a pmga_ access token against the shared secret and
// the resource being requested.
//
// For a PATH-MOUNTED route the audience must name that exact resource
// (host+prefix). The "*" wildcard — minted when a client sends no resource
// parameter — is deliberately NOT accepted there: several MCP servers share
// one host under different prefixes, so a wildcard token would be a key to all
// of them, and honouring it would make the prefix decorative. Clients get the
// exact resource to ask for from the challenge and the RFC 9728 metadata, so a
// spec-compliant one always sends it.
//
// Host-wide routes keep the original behaviour, wildcard included, so existing
// single-MCP-per-host setups are unaffected.
func (a *authGate) verifyOAuthBearer(token, reqHost, pathPrefix string) (string, bool) {
	claims, ok := sso.VerifyAccess(token, a.secret)
	if !ok {
		return "", false
	}
	want := oauthResource(reqHost, pathPrefix)
	if pathPrefix != "" {
		if !strings.EqualFold(claims.Audience, want) {
			return "", false
		}
		return claims.Username, true
	}
	if claims.Audience != "*" && !strings.EqualFold(claims.Audience, want) {
		return "", false
	}
	return claims.Username, true
}

// denyOAuth is the oauth-mode 401: the WWW-Authenticate challenge carries
// the resource-metadata URL that starts the client's discovery flow
// (RFC 9728 §5.1), plus error="invalid_token" when a bearer was presented
// but rejected.
// pathPrefix is appended to the metadata URL so a client denied at
// /mcp/dashboard is pointed at that sub-resource's metadata rather than the
// host's. Without it the client would request a host-wide token and then be
// rejected by verifyOAuthBearer for the very resource it was told to ask about.
func (a *authGate) denyOAuth(w http.ResponseWriter, reqHost, pathPrefix string, hadBearer bool) {
	metaURL := "https://" + reqHost + protectedResourcePath + strings.TrimSuffix(pathPrefix, "/")
	challenge := fmt.Sprintf("Bearer resource_metadata=%q", metaURL)
	if hadBearer {
		challenge += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
}

// authorizeOAuth is the oauth-mode counterpart of the sso branch in
// authorize(). It accepts ONLY a bearer token — a pmt_ dashboard token or a
// pmga_ OAuth access token — with no SSO cookie and no LAN bypass, so
// browsers always get the 401 challenge. Never redirects to the login page,
// even for Accept: text/html — MCP clients follow the WWW-Authenticate
// challenge instead.
func (a *authGate) authorizeOAuth(w http.ResponseWriter, req *http.Request, group *RouteGroup, reqHost string) bool {
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
		if user, ok := a.verifyOAuthBearer(strings.TrimPrefix(authz, bearerPrefix), reqHost, group.PathPrefix); ok {
			if userAllowed(group, user) {
				return true
			}
			httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	}

	a.denyOAuth(w, reqHost, group.PathPrefix, hadBearer)
	return false
}

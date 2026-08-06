package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// Several MCP servers share one host under different prefixes, so a token has
// to name the exact one. These pin the boundary that makes that true.

func pathGate(t *testing.T) *authGate {
	t.Helper()
	return oauthGate()
}

func mintAud(t *testing.T, a *authGate, user, aud string) string {
	t.Helper()
	return sso.SignAccess(sso.AccessClaims{Username: user, Audience: aud, Exp: authFuture().Unix()}, a.secret)
}

// The headline guarantee: a token for one sub-path must not open another.
func TestOAuthTokenDoesNotCrossPaths(t *testing.T) {
	a := pathGate(t)
	const host = "mcp.polardev.org"
	obsidian := mintAud(t, a, "alice", host+"/mcp/obsidian")

	if u, ok := a.verifyOAuthBearer(obsidian, host, "/mcp/obsidian"); !ok || u != "alice" {
		t.Fatalf("token rejected on its own resource: user=%q ok=%v", u, ok)
	}
	if _, ok := a.verifyOAuthBearer(obsidian, host, "/mcp/dashboard"); ok {
		t.Fatal("obsidian token accepted on /mcp/dashboard — paths are not isolated")
	}
}

// A wildcard token is minted whenever a client sends no resource parameter.
// Honouring it on a path-mounted route would make the prefix decorative.
func TestOAuthWildcardRejectedOnPathRoute(t *testing.T) {
	a := pathGate(t)
	const host = "mcp.polardev.org"
	wildcard := mintAud(t, a, "alice", "*")

	if _, ok := a.verifyOAuthBearer(wildcard, host, "/mcp/dashboard"); ok {
		t.Fatal("wildcard accepted on a path-mounted route — that is a key to every MCP on the host")
	}
	// Host-wide routes are unchanged: existing single-MCP-per-host setups
	// must keep working exactly as before.
	if u, ok := a.verifyOAuthBearer(wildcard, host, ""); !ok || u != "alice" {
		t.Fatalf("wildcard rejected on a host-wide route: user=%q ok=%v", u, ok)
	}
}

// A host-wide token must not be usable on a path-mounted resource, or moving a
// service under a prefix would silently gain nothing.
func TestOAuthHostTokenRejectedOnPathRoute(t *testing.T) {
	a := pathGate(t)
	const host = "mcp.polardev.org"
	hostTok := mintAud(t, a, "alice", host)

	if _, ok := a.verifyOAuthBearer(hostTok, host, "/mcp/dashboard"); ok {
		t.Fatal("host-wide token accepted on a path-mounted route")
	}
	if u, ok := a.verifyOAuthBearer(hostTok, host, ""); !ok || u != "alice" {
		t.Fatalf("host-wide token rejected on its own host route: user=%q ok=%v", u, ok)
	}
}

// Audience comparison is case-insensitive on the host (DNS is) but the path is
// compared as-is by EqualFold too — pin the behaviour so a future change is a
// deliberate one.
func TestOAuthAudienceHostCaseInsensitive(t *testing.T) {
	a := pathGate(t)
	tok := mintAud(t, a, "alice", "MCP.PolarDev.org/mcp/dashboard")
	if _, ok := a.verifyOAuthBearer(tok, "mcp.polardev.org", "/mcp/dashboard"); !ok {
		t.Fatal("host case difference rejected the token")
	}
}

// A trailing slash on the route prefix must not produce a different resource
// than the same route without one.
func TestOAuthResourceTrailingSlash(t *testing.T) {
	if got, want := oauthResource("mcp.polardev.org", "/mcp/dashboard/"), "mcp.polardev.org/mcp/dashboard"; got != want {
		t.Fatalf("oauthResource = %q, want %q", got, want)
	}
	if got, want := oauthResource("MCP.polardev.org", ""), "mcp.polardev.org"; got != want {
		t.Fatalf("oauthResource = %q, want %q", got, want)
	}
}

// The 401 has to point at the SUB-resource's metadata. Pointing at the host's
// would send the client to fetch a host-wide resource id, request a token for
// it, and then be rejected for the very resource it was told to ask about.
func TestDenyOAuthChallengeCarriesPath(t *testing.T) {
	a := pathGate(t)
	rec := httptest.NewRecorder()
	a.denyOAuth(rec, "mcp.polardev.org", "/mcp/dashboard", false)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := "https://mcp.polardev.org" + protectedResourcePath + "/mcp/dashboard"
	if !strings.Contains(got, want) {
		t.Fatalf("challenge = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, `error="invalid_token"`) {
		t.Error("no bearer was presented; challenge must not claim an invalid token")
	}

	rec = httptest.NewRecorder()
	a.denyOAuth(rec, "mcp.polardev.org", "/mcp/dashboard", true)
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Error("a rejected bearer must be reported as invalid_token")
	}
}

// A host-wide route's challenge must stay exactly as it was.
func TestDenyOAuthHostWideChallengeUnchanged(t *testing.T) {
	a := pathGate(t)
	rec := httptest.NewRecorder()
	a.denyOAuth(rec, "mcp.polardev.org", "", false)
	got := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(got, "https://mcp.polardev.org"+protectedResourcePath) {
		t.Fatalf("challenge = %q", got)
	}
	if strings.Contains(got, protectedResourcePath+"/") {
		t.Fatalf("host-wide challenge grew a path suffix: %q", got)
	}
}

package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

func oauthGate() *authGate {
	return &authGate{secret: authTestSecret, domains: []string{"polardev.org"}}
}

func TestParentDomain(t *testing.T) {
	a := oauthGate()
	if got := a.parentDomain("polardev.org"); got != "polardev.org" {
		t.Fatalf("apex: got %q", got)
	}
	if got := a.parentDomain("mcp.polardev.org"); got != "polardev.org" {
		t.Fatalf("subdomain: got %q", got)
	}
	if got := a.parentDomain("MCP.PolarDev.org"); got != "polardev.org" {
		t.Fatalf("case-insensitive: got %q", got)
	}
	if got := a.parentDomain("example.com"); got != "" {
		t.Fatalf("unrelated: got %q, want empty", got)
	}
}

func TestVerifyOAuthBearer(t *testing.T) {
	a := oauthGate()
	host := "mcp.polardev.org"

	wildcard := sso.SignAccess(sso.AccessClaims{Username: "alice", Audience: "*", Exp: authFuture().Unix()}, authTestSecret)
	if u, ok := a.verifyOAuthBearer(wildcard, host); !ok || u != "alice" {
		t.Fatalf("wildcard aud: got %q ok=%v", u, ok)
	}

	exact := sso.SignAccess(sso.AccessClaims{Username: "bob", Audience: host, Exp: authFuture().Unix()}, authTestSecret)
	if u, ok := a.verifyOAuthBearer(exact, host); !ok || u != "bob" {
		t.Fatalf("exact aud: got %q ok=%v", u, ok)
	}

	mismatch := sso.SignAccess(sso.AccessClaims{Username: "bob", Audience: "other.polardev.org", Exp: authFuture().Unix()}, authTestSecret)
	if _, ok := a.verifyOAuthBearer(mismatch, host); ok {
		t.Fatal("audience mismatch should be rejected")
	}

	if _, ok := a.verifyOAuthBearer("pmga_garbage", host); ok {
		t.Fatal("bad token should be rejected")
	}
}

func TestServeProtectedResourceMetadata(t *testing.T) {
	a := oauthGate()

	req := httptest.NewRequest("GET", "http://mcp.polardev.org"+protectedResourcePath+"/sub", nil)
	rec := httptest.NewRecorder()
	a.serveProtectedResourceMetadata(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Resource               string   `json:"resource"`
		AuthorizationServers   []string `json:"authorization_servers"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Resource != "https://mcp.polardev.org/sub" {
		t.Fatalf("resource = %q (suffix not echoed)", out.Resource)
	}
	if len(out.AuthorizationServers) != 1 || out.AuthorizationServers[0] != "https://auth.polardev.org" {
		t.Fatalf("authorization_servers = %v", out.AuthorizationServers)
	}
	if len(out.BearerMethodsSupported) != 1 || out.BearerMethodsSupported[0] != "header" {
		t.Fatalf("bearer_methods_supported = %v", out.BearerMethodsSupported)
	}

	// Host outside any parent domain → 404.
	req = httptest.NewRequest("GET", "http://mcp.example.com"+protectedResourcePath, nil)
	rec = httptest.NewRecorder()
	a.serveProtectedResourceMetadata(rec, req)
	if rec.Code != 404 {
		t.Fatalf("unknown parent status = %d, want 404", rec.Code)
	}
}

func TestDenyOAuthChallenge(t *testing.T) {
	a := oauthGate()
	host := "mcp.polardev.org"

	rec := httptest.NewRecorder()
	a.denyOAuth(rec, host, false)
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, `resource_metadata="https://mcp.polardev.org`+protectedResourcePath+`"`) {
		t.Fatalf("challenge = %q, missing resource_metadata", wa)
	}
	if strings.Contains(wa, "invalid_token") {
		t.Fatalf("challenge = %q, must not carry error without a bearer", wa)
	}

	rec = httptest.NewRecorder()
	a.denyOAuth(rec, host, true)
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("challenge with bearer = %q, want invalid_token", rec.Header().Get("WWW-Authenticate"))
	}
}

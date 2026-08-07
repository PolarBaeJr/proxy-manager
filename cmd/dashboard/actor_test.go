package main

import (
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

var testActorSecret = []byte("actor-secret-for-tests-0123456789")

// withActorSecret installs a secret for one test and restores the previous one,
// since actorSecret is package state.
func withActorSecret(t *testing.T, secret []byte) {
	t.Helper()
	prev := actorSecret
	actorSecret = secret
	t.Cleanup(func() { actorSecret = prev })
}

func signedActor(t *testing.T, user, ip string, exp time.Time) string {
	t.Helper()
	return sso.SignActor(sso.ActorClaims{Username: user, IP: ip, Exp: exp.Unix()}, testActorSecret)
}

// The feature working: a verified assertion names the end user, while still
// showing which credential carried the call.
func TestAuditUserPrefersVerifiedActor(t *testing.T) {
	withActorSecret(t, testActorSecret)
	req := httptest.NewRequest("POST", "/api/services/app/stop", nil)
	req.Header.Set(actorHeader, signedActor(t, "alice", "100.64.0.9", time.Now().Add(time.Minute)))

	got := auditUser(req, "mcp-bot")
	if got != "alice (via mcp-bot)" {
		t.Fatalf("auditUser = %q, want %q", got, "alice (via mcp-bot)")
	}
	if ip := actorIP(req); ip != "100.64.0.9" {
		t.Errorf("actorIP = %q, want the real client IP", ip)
	}
}

// A forged assertion must not be believed. This is the boundary: without the
// signature check, anything on the docker network could claim to be anyone.
func TestAuditUserRejectsForgedAssertion(t *testing.T) {
	withActorSecret(t, testActorSecret)

	forged := sso.SignActor(sso.ActorClaims{Username: "root", Exp: time.Now().Add(time.Minute).Unix()},
		[]byte("a-different-secret-entirely-xxxxx"))
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set(actorHeader, forged)

	if got := auditUser(req, "mcp-bot"); got != "mcp-bot" {
		t.Fatalf("auditUser = %q — a forged assertion was believed", got)
	}
	if ip := actorIP(req); ip != "" {
		t.Errorf("actorIP = %q, want empty for a forged assertion", ip)
	}
}

// An expired assertion degrades attribution; it must never fail the operation,
// so auditUser simply falls back.
func TestAuditUserExpiredFallsBack(t *testing.T) {
	withActorSecret(t, testActorSecret)
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set(actorHeader, signedActor(t, "alice", "1.2.3.4", time.Now().Add(-time.Minute)))

	if got := auditUser(req, "mcp-bot"); got != "mcp-bot" {
		t.Fatalf("auditUser = %q, want the fallback for an expired assertion", got)
	}
}

func TestAuditUserGarbageAndMissing(t *testing.T) {
	withActorSecret(t, testActorSecret)
	for _, tc := range []struct{ name, hdr string }{
		{"absent", ""},
		{"garbage", "not-a-token"},
		{"wrong prefix", "pmga_abcdef"},
		{"empty username", sso.SignActor(sso.ActorClaims{Exp: time.Now().Add(time.Minute).Unix()}, testActorSecret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/x", nil)
			if tc.hdr != "" {
				req.Header.Set(actorHeader, tc.hdr)
			}
			if got := auditUser(req, "mcp-bot"); got != "mcp-bot" {
				t.Fatalf("auditUser = %q, want the fallback", got)
			}
		})
	}
}

// With no secret configured the header is ignored entirely, even a
// well-formed one — otherwise enabling the feature would be implicit.
func TestAuditUserIgnoredWithoutSecret(t *testing.T) {
	withActorSecret(t, nil)
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set(actorHeader, signedActor(t, "alice", "1.2.3.4", time.Now().Add(time.Minute)))

	if got := auditUser(req, "mcp-bot"); got != "mcp-bot" {
		t.Fatalf("auditUser = %q — the header was honoured with no secret set", got)
	}
	if ip := actorIP(req); ip != "" {
		t.Errorf("actorIP = %q, want empty with no secret", ip)
	}
}

func TestInitActorSecret(t *testing.T) {
	withActorSecret(t, nil)

	// Unset must warn rather than fail silently.
	msgs := initActorSecret(func(string) string { return "" })
	if len(msgs) == 0 || !strings.Contains(msgs[0], "unset") {
		t.Fatalf("expected a warning for an unset secret, got %v", msgs)
	}
	if actorSecret != nil {
		t.Error("secret set despite empty env")
	}

	// Non-hex must warn and stay disabled, not panic.
	msgs = initActorSecret(func(string) string { return "zzzz-not-hex" })
	if len(msgs) == 0 || !strings.Contains(msgs[0], "not valid hex") {
		t.Fatalf("expected a hex warning, got %v", msgs)
	}
	if actorSecret != nil {
		t.Error("secret set from invalid hex")
	}

	valid := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	msgs = initActorSecret(func(string) string { return valid })
	if len(msgs) == 0 || !strings.Contains(msgs[0], "attribution enabled") {
		t.Fatalf("expected an enabled message, got %v", msgs)
	}
	if len(actorSecret) == 0 {
		t.Error("secret not set from valid hex")
	}
}

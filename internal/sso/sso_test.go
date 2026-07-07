package sso

import (
	"testing"
	"time"
)

func TestCookieName(t *testing.T) {
	if CookieName != "pmgr_sso" {
		t.Fatalf("CookieName = %q, want %q", CookieName, "pmgr_sso")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	raw := Sign("alice", time.Now().Add(time.Hour), testSecret)
	user, ok := Verify(raw, testSecret)
	if !ok {
		t.Fatal("Verify rejected its own cookie")
	}
	if user != "alice" {
		t.Fatalf("Verify returned user %q, want %q", user, "alice")
	}
}

func TestVerifyRejects(t *testing.T) {
	valid := Sign("alice", time.Now().Add(time.Hour), testSecret)
	cases := []struct {
		name string
		raw  string
	}{
		{"tampered value", flipChar(valid, 0)},
		{"expired", Sign("alice", time.Now().Add(-time.Minute), testSecret)},
		{"junk", "not-a-cookie"},
		{"oauth access blob", SignAccess(AccessClaims{Username: "alice", Audience: "*", Exp: futureExp()}, testSecret)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := Verify(c.raw, testSecret); ok {
				t.Fatalf("Verify accepted %s", c.name)
			}
		})
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	raw := Sign("alice", time.Now().Add(time.Hour), testSecret)
	if _, ok := Verify(raw, []byte("wrong-secret")); ok {
		t.Fatal("Verify accepted cookie under wrong secret")
	}
}

// A username containing the '|' delimiter splits into more than three parts,
// so Verify rejects it rather than letting the delimiter be smuggled into the
// signed payload (documented contract in Sign/Verify).
func TestVerifyDelimiterInUsernameRejected(t *testing.T) {
	raw := Sign("al|ice", time.Now().Add(time.Hour), testSecret)
	if _, ok := Verify(raw, testSecret); ok {
		t.Fatal("Verify accepted a username containing the delimiter")
	}
}

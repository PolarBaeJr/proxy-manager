package main

import (
	"strings"
	"testing"
)

// The credential exists so nobody has to manage one. It must be freshly random
// each start and never resemble something an operator could have configured.
func TestMintInternalTokenIsRandomAndPrefixed(t *testing.T) {
	prev := internalToken
	t.Cleanup(func() { internalToken = prev })

	if err := mintInternalToken(); err != nil {
		t.Fatalf("mint: %v", err)
	}
	first := internalToken
	if !strings.HasPrefix(first, apiTokenPrefix) {
		t.Errorf("token %q lacks the API token prefix, so VerifyToken would reject it", first)
	}
	if len(first) < 40 {
		t.Errorf("token is too short to be unguessable: %d chars", len(first))
	}

	if err := mintInternalToken(); err != nil {
		t.Fatal(err)
	}
	if internalToken == first {
		t.Error("two mints produced the same token — it is not random per process")
	}
}

// It must authenticate, and only it. A near-miss must not.
func TestInternalTokenVerifies(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	t.Cleanup(func() { internalToken = prev })
	if err := mintInternalToken(); err != nil {
		t.Fatal(err)
	}

	if got := s.VerifyToken(internalToken); got != internalUser {
		t.Fatalf("VerifyToken(internal) = %q, want %q", got, internalUser)
	}
	if got := s.VerifyToken(internalToken + "x"); got != "" {
		t.Errorf("a near-miss token authenticated as %q", got)
	}
	if got := s.VerifyToken("pmt_internal_guess"); got != "" {
		t.Errorf("a guessed token authenticated as %q", got)
	}

	// Disabled means disabled: an empty internal token must not match "".
	internalToken = ""
	if got := s.VerifyToken(apiTokenPrefix); got != "" {
		t.Errorf("empty internal token matched, giving %q", got)
	}
}

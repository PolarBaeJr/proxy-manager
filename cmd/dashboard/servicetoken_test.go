package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionServiceToken(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	dir := t.TempDir()

	if err := provisionServiceToken(s, dir, "statusbot"); err != nil {
		t.Fatalf("provisionServiceToken: %v", err)
	}
	path := filepath.Join(dir, "statusbot.token")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	raw := string(b)
	if !strings.HasPrefix(raw, apiTokenPrefix) {
		t.Fatalf("token %q missing prefix %q", raw, apiTokenPrefix)
	}
	if got := s.VerifyToken(raw); got != "statusbot" {
		t.Fatalf("VerifyToken(raw) = %q, want %q", got, "statusbot")
	}

	if err := provisionServiceToken(s, dir, "statusbot"); err != nil {
		t.Fatalf("provisionServiceToken (second call): %v", err)
	}
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file (second): %v", err)
	}
	if string(b2) != raw {
		t.Fatal("provisionServiceToken rewrote an already-provisioned token file")
	}
}

func TestProvisionServiceTokenReheatsStaleFile(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	dir := t.TempDir()
	path := filepath.Join(dir, "statusbot.token")

	// Simulate a token file left over from before auth.json was replaced/
	// restored — the hash it points at no longer exists in the store.
	if err := os.WriteFile(path, []byte("pmt_stale-token-nobody-recognizes"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	if err := provisionServiceToken(s, dir, "statusbot"); err != nil {
		t.Fatalf("provisionServiceToken: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	raw := string(b)
	if raw == "pmt_stale-token-nobody-recognizes" {
		t.Fatal("provisionServiceToken left a stale, unverifiable token file in place")
	}
	if got := s.VerifyToken(raw); got != "statusbot" {
		t.Fatalf("VerifyToken(raw) = %q, want %q", got, "statusbot")
	}
}

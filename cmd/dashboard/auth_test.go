package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// codeNow returns the current valid TOTP code for a secret.
func codeNow(secret string) string {
	return totp(secret, time.Now().Unix()/totpPeriodSeconds)
}

// newConfirmedStore builds a store with one confirmed user via the two-phase flow.
func newConfirmedStore(t *testing.T, username, password string) (*AuthStore, string) {
	t.Helper()
	s, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("loadAuthStore: %v", err)
	}
	secret, _, err := s.BeginSetup(username, password)
	if err != nil {
		t.Fatalf("BeginSetup: %v", err)
	}
	if err := s.ConfirmPending(username, codeNow(secret)); err != nil {
		t.Fatalf("ConfirmPending: %v", err)
	}
	return s, secret
}

func TestVerifyPassword(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	if !s.VerifyPassword("alice", "correct horse") {
		t.Fatal("correct password rejected")
	}
	if s.VerifyPassword("alice", "wrong password") {
		t.Fatal("wrong password accepted")
	}
	if s.VerifyPassword("nobody", "whatever") {
		t.Fatal("unknown user accepted")
	}
}

func TestVerifyTOTPSecret(t *testing.T) {
	s, secret := newConfirmedStore(t, "alice", "correct horse")
	counter := time.Now().Unix() / totpPeriodSeconds
	if !s.VerifyTOTP("alice", totp(secret, counter)) {
		t.Fatal("current code rejected")
	}
	if !s.VerifyTOTP("alice", totp(secret, counter+1)) {
		t.Fatal("+1 drift code rejected")
	}
	if !s.VerifyTOTP("alice", totp(secret, counter-1)) {
		t.Fatal("-1 drift code rejected")
	}
	if s.VerifyTOTP("alice", totp(secret, counter+2)) {
		t.Fatal("+2 drift code accepted")
	}
	if s.VerifyTOTP("alice", "000000") && totp(secret, counter) != "000000" {
		t.Fatal("obviously wrong code accepted")
	}
	// Empty secret is always rejected.
	if verifyTOTPSecret("", totp(secret, counter)) {
		t.Fatal("empty secret accepted a code")
	}
}

func TestHasTOTP(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	if !s.HasTOTP("alice") {
		t.Fatal("enrolled user should have TOTP")
	}
	if s.HasTOTP("nobody") {
		t.Fatal("unknown user reported as having TOTP")
	}
}

func TestCookieRoundTrip(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")

	raw := s.newCookie("alice", time.Now().Add(time.Hour))
	info, ok := s.parseCookie(raw)
	if !ok {
		t.Fatal("valid cookie rejected")
	}
	if info.Username != "alice" {
		t.Fatalf("username = %q, want alice", info.Username)
	}

	// Tampered signature.
	bad := raw[:len(raw)-2] + flipHex(raw[len(raw)-2:])
	if _, ok := s.parseCookie(bad); ok {
		t.Fatal("tampered signature accepted")
	}

	// Wrong field count.
	if _, ok := s.parseCookie("alice|123|456"); ok {
		t.Fatal("3-field cookie accepted")
	}

	// Expired: valid signature but issued long ago.
	if _, ok := s.parseCookie(s.signBody(fmt.Sprintf("alice|%d|%d",
		time.Now().Add(-sessionLifetime-time.Hour).Unix(), time.Now().Unix()))); ok {
		t.Fatal("expired cookie accepted")
	}

	// Deleted user: valid signature, but user no longer exists.
	if _, ok := s.parseCookie(s.signBody(fmt.Sprintf("ghost|%d|%d",
		time.Now().Unix(), time.Now().Unix()))); ok {
		t.Fatal("cookie for nonexistent user accepted")
	}
}

// signBody mirrors newCookie's HMAC construction over an explicit body.
func (s *AuthStore) signBody(body string) string {
	mac := hmac.New(sha256.New, s.cookieSecret())
	mac.Write([]byte(body))
	return body + "|" + hex.EncodeToString(mac.Sum(nil))
}

func flipHex(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] == '0' {
			b[i] = '1'
		} else {
			b[i] = '0'
		}
	}
	return string(b)
}

func TestTwoPhaseUserCreation(t *testing.T) {
	s, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("loadAuthStore: %v", err)
	}
	secret, _, err := s.BeginSetup("alice", "correct horse")
	if err != nil {
		t.Fatalf("BeginSetup: %v", err)
	}
	// Wrong code does not confirm.
	if err := s.ConfirmPending("alice", "000000"); err == nil {
		if totp(secret, time.Now().Unix()/totpPeriodSeconds) != "000000" {
			t.Fatal("ConfirmPending accepted a wrong code")
		}
	}
	// Correct code persists the user.
	if err := s.ConfirmPending("alice", codeNow(secret)); err != nil {
		t.Fatalf("ConfirmPending: %v", err)
	}
	if !s.IsSetup() {
		t.Fatal("store should be set up after confirm")
	}
	// A second BeginSetup once a user exists errors.
	if _, _, err := s.BeginSetup("bob", "another pass"); err == nil {
		t.Fatal("second BeginSetup should error")
	}
}

func TestValidUsername(t *testing.T) {
	for _, s := range []string{"ab", "alice", "a.b_c-d", "User1", "abcdefghijklmnopqrstuvwxyz012345"} {
		if !validUsername(s) {
			t.Errorf("validUsername(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "a", "a b", "a/b", "a@b", "abcdefghijklmnopqrstuvwxyz0123456"} {
		if validUsername(s) {
			t.Errorf("validUsername(%q) = true, want false", s)
		}
	}
}

func TestRemintServiceToken(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	raw, err := s.RemintServiceToken("statusbot")
	if err != nil {
		t.Fatalf("RemintServiceToken: %v", err)
	}
	if got := s.VerifyToken(raw); got != "statusbot" {
		t.Fatalf("VerifyToken(raw) = %q, want %q", got, "statusbot")
	}
	s.mu.RLock()
	lastUsed := s.data.ServiceTokens[0].LastUsedAt
	s.mu.RUnlock()
	if lastUsed == 0 {
		t.Fatal("LastUsedAt was not set after VerifyToken")
	}
}

func TestRemintServiceTokenReplacesOld(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	oldRaw, err := s.RemintServiceToken("statusbot")
	if err != nil {
		t.Fatalf("RemintServiceToken (first): %v", err)
	}
	newRaw, err := s.RemintServiceToken("statusbot")
	if err != nil {
		t.Fatalf("RemintServiceToken (second): %v", err)
	}
	s.mu.RLock()
	n := len(s.data.ServiceTokens)
	s.mu.RUnlock()
	if n != 1 {
		t.Fatalf("len(ServiceTokens) = %d, want 1", n)
	}
	if s.VerifyToken(oldRaw) != "" {
		t.Fatal("old service token still verifies after remint")
	}
	if got := s.VerifyToken(newRaw); got != "statusbot" {
		t.Fatalf("VerifyToken(newRaw) = %q, want %q", got, "statusbot")
	}
}

func TestServiceTokenDoesNotElevate(t *testing.T) {
	s, _ := newConfirmedStore(t, "alice", "correct horse")
	raw, err := s.RemintServiceToken("statusbot")
	if err != nil {
		t.Fatalf("RemintServiceToken: %v", err)
	}
	if got := s.VerifyToken(raw); got != "statusbot" {
		t.Fatalf("VerifyToken(raw) = %q, want %q — service tokens must still pass the broad check", got, "statusbot")
	}
	if got := s.VerifyElevatedToken(raw); got != "" {
		t.Fatalf("VerifyElevatedToken(raw) = %q, want \"\" — service tokens must never grant elevated access", got)
	}

	// Contrast: a human-minted token still elevates.
	userRaw, _, err := s.CreateToken("alice", "cli")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if got := s.VerifyElevatedToken(userRaw); got != "alice" {
		t.Fatalf("VerifyElevatedToken(userRaw) = %q, want %q", got, "alice")
	}
}

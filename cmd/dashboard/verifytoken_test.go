package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /api/auth/verify-token reports whether the credential is elevated, which is
// what lets the SSO portal's break-glass token login refuse the
// auto-provisioned service token while accepting a human one.

func verifyToken(t *testing.T, mux http.Handler, token, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/auth/verify-token", strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func verifyTokenMux(t *testing.T) (http.Handler, *AuthStore) {
	t.Helper()
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := newDashboardMux(nil, nil, auth, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	return mux, auth
}

func TestVerifyTokenReportsElevationForUserToken(t *testing.T) {
	mux, auth := verifyTokenMux(t)
	raw, _, err := auth.CreateToken("alice", "cli")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	rec := verifyToken(t, mux, raw, "192.0.2.10:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Username string `json:"username"`
		Elevated bool   `json:"elevated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Username != "alice" {
		t.Errorf("username = %q, want %q", out.Username, "alice")
	}
	if !out.Elevated {
		t.Error("elevated = false for a human-minted token; the portal would refuse the only credential that can recover an account")
	}
}

func TestVerifyTokenServiceTokenNotElevated(t *testing.T) {
	mux, auth := verifyTokenMux(t)
	raw, err := auth.RemintServiceToken("statusbot")
	if err != nil {
		t.Fatalf("RemintServiceToken: %v", err)
	}

	rec := verifyToken(t, mux, raw, "192.0.2.11:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Username string `json:"username"`
		Elevated bool   `json:"elevated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Username != "statusbot" {
		t.Errorf("username = %q, want %q", out.Username, "statusbot")
	}
	if out.Elevated {
		t.Error("elevated = true for an auto-provisioned service token; a mounted container credential must never mint a human SSO session")
	}
}

func TestVerifyTokenRejectsUnknownToken(t *testing.T) {
	mux, _ := verifyTokenMux(t)
	rec := verifyToken(t, mux, "pmt_bogus", "192.0.2.12:1234")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// The proxy's verifyBearer decodes the 200 body into exactly this shape; the
// added "elevated" field must not disturb it.
func TestVerifyTokenBodyStillDecodesForProxy(t *testing.T) {
	mux, auth := verifyTokenMux(t)
	raw, _, err := auth.CreateToken("alice", "cli")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	rec := verifyToken(t, mux, raw, "192.0.2.13:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Username != "alice" {
		t.Errorf("username = %q, want %q", out.Username, "alice")
	}
}

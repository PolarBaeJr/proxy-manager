package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidContainerName(t *testing.T) {
	valid := []string{"a", "app-1", "app_1", "app.1", "ABCabc123", strings.Repeat("a", 64)}
	for _, v := range valid {
		if !validContainerName(v) {
			t.Errorf("validContainerName(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "../x", "/x", "a b", "a/b", strings.Repeat("a", 129)}
	for _, v := range invalid {
		if validContainerName(v) {
			t.Errorf("validContainerName(%q) = true, want false", v)
		}
	}
}

// TestRegisterLogRoutesContainersLocal and TestRegisterLogRoutesLocal are
// baseline coverage for the plain (no ?host=) local paths through
// registerLogRoutes directly — logs.go had zero test coverage before this
// phase, and this phase changes registerLogRoutes's signature materially
// (adds the registry param), so a regression here needs its own guard
// independent of logshost_test.go's cross-host-focused cases.
func TestRegisterLogRoutesContainersLocal(t *testing.T) {
	dc := logsContainersStub(t, nil)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := http.NewServeMux()
	registerLogRoutes(mux, dc, auth, nil)

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	req := httptest.NewRequest("GET", "/api/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var list []containerSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].Name != "app-1" {
		t.Fatalf("list = %+v, want the stub's one container", list)
	}
}

func TestRegisterLogRoutesLocal(t *testing.T) {
	frames := framedLogBody("local line one", "local line two")
	dc := logsContainersStub(t, frames)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := http.NewServeMux()
	registerLogRoutes(mux, dc, auth, nil)

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	req := httptest.NewRequest("GET", "/api/logs/app-1?tail=10", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Container string    `json:"container"`
		Tail      int       `json:"tail"`
		Lines     []logLine `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Container != "app-1" || body.Tail != 10 {
		t.Errorf("body = %+v, want container=app-1 tail=10", body)
	}
	if len(body.Lines) != 2 || body.Lines[0].Text != "local line one" {
		t.Fatalf("Lines = %+v, want the framed stub's lines", body.Lines)
	}
}

func TestRegisterLogRoutesRejectsInvalidContainerName(t *testing.T) {
	dc := logsContainersStub(t, nil)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := http.NewServeMux()
	registerLogRoutes(mux, dc, auth, nil)

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	req := httptest.NewRequest("GET", "/api/logs/evil/name", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

package httpx

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 201, map[string]string{"hello": "world"})
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["hello"] != "world" {
		t.Fatalf("body = %v", out)
	}
}

func TestWriteErr(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErr(rec, errors.New("boom"))
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["error"] != "boom" {
		t.Fatalf("error body = %v", out)
	}
}

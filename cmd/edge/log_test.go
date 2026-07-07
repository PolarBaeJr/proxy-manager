package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForwardedHeadersOverwrite(t *testing.T) {
	var got *http.Request
	h := withForwardedHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
	}))

	// Attacker supplies bogus forwarding headers; peer is 203.0.113.7.
	req := httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 7.7.7.7")
	req.Header.Set("X-Real-IP", "6.6.6.6")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if v := got.Header.Get("X-Forwarded-For"); v != "203.0.113.7" {
		t.Fatalf("X-Forwarded-For = %q, want peer only", v)
	}
	if v := got.Header.Get("X-Real-IP"); v != "203.0.113.7" {
		t.Fatalf("X-Real-IP = %q, want peer only", v)
	}
	if v := got.Header.Get("X-Forwarded-Proto"); v != "http" {
		t.Fatalf("X-Forwarded-Proto = %q, want http", v)
	}

	// With TLS the proto flips to https.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	req.TLS = &tls.ConnectionState{}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if v := got.Header.Get("X-Forwarded-Proto"); v != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", v)
	}
}

func TestRemoteIP(t *testing.T) {
	req := httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "198.51.100.9:5555"
	if got := remoteIP(req); got != "198.51.100.9" {
		t.Fatalf("remoteIP = %q, want 198.51.100.9", got)
	}
	// No port: returned verbatim.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "198.51.100.9"
	if got := remoteIP(req); got != "198.51.100.9" {
		t.Fatalf("remoteIP = %q, want 198.51.100.9", got)
	}
}

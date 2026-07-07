package main

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	// First X-Forwarded-For hop, comma-split and trimmed.
	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Header.Set("X-Forwarded-For", " 1.2.3.4 , 5.6.7.8")
	if got := clientIP(req); got != "1.2.3.4" {
		t.Fatalf("clientIP xff = %q, want 1.2.3.4", got)
	}

	// No XFF: host from RemoteAddr.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	if got := clientIP(req); got != "9.9.9.9" {
		t.Fatalf("clientIP remoteaddr = %q, want 9.9.9.9", got)
	}

	// Empty RemoteAddr and no XFF: "".
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = ""
	if got := clientIP(req); got != "" {
		t.Fatalf("clientIP empty = %q, want empty", got)
	}
}

func TestAccessLogRing(t *testing.T) {
	a := NewAccessLog()
	const n = ringSize + 50
	for i := 1; i <= n; i++ {
		a.Record(AccessEntry{Time: int64(i), Path: "/p"})
	}
	out := a.Snapshot(ringSize+100, 0)
	if len(out) != ringSize {
		t.Fatalf("snapshot len = %d, want %d (capped)", len(out), ringSize)
	}
	// Newest first.
	if out[0].Time != int64(n) {
		t.Fatalf("newest Time = %d, want %d", out[0].Time, n)
	}
	// Oldest surviving entry is the (n-ringSize+1)th; anything older overwritten.
	if out[len(out)-1].Time != int64(n-ringSize+1) {
		t.Fatalf("oldest surviving Time = %d, want %d", out[len(out)-1].Time, n-ringSize+1)
	}
}

func TestAccessLogSince(t *testing.T) {
	a := NewAccessLog()
	for i := 1; i <= 10; i++ {
		a.Record(AccessEntry{Time: int64(i)})
	}
	out := a.Snapshot(100, 7)
	if len(out) != 3 {
		t.Fatalf("since=7 len = %d, want 3", len(out))
	}
	for _, e := range out {
		if e.Time <= 7 {
			t.Fatalf("entry Time %d not strictly newer than 7", e.Time)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("truncate short = %q", got)
	}
	if got := truncate("hello", 3); got != "hel" {
		t.Fatalf("truncate long = %q, want hel", got)
	}
	if got := truncate("hello", 5); got != "hello" {
		t.Fatalf("truncate exact = %q", got)
	}
}

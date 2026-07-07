package main

import (
	"net"
	"net/http/httptest"
	"testing"
)

func mustCIDR(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

func TestRealClientIP(t *testing.T) {
	trusted := mustCIDR(t, "10.0.0.0/8")

	// Untrusted peer with a spoofed XFF: XFF ignored, peer returned.
	req := httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if ip := realClientIP(req, trusted); ip == nil || ip.String() != "203.0.113.5" {
		t.Fatalf("untrusted peer: got %v, want 203.0.113.5", ip)
	}

	// Trusted peer: take the rightmost XFF entry.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 9.9.9.9")
	if ip := realClientIP(req, trusted); ip == nil || ip.String() != "9.9.9.9" {
		t.Fatalf("trusted peer: got %v, want 9.9.9.9", ip)
	}

	// Trusted peer, empty XFF: fall back to peer.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	if ip := realClientIP(req, trusted); ip == nil || ip.String() != "10.1.2.3" {
		t.Fatalf("trusted peer empty xff: got %v, want 10.1.2.3", ip)
	}

	// Malformed RemoteAddr with no parseable host: best-effort yields nil.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "not-an-addr"
	if ip := realClientIP(req, trusted); ip != nil {
		t.Fatalf("malformed remoteaddr: got %v, want nil", ip)
	}
}

func TestParseCIDRList(t *testing.T) {
	got := parseCIDRList("10.0.0.0/8, 192.168.0.0/16 , , bad-cidr")
	if len(got) != 2 {
		t.Fatalf("parseCIDRList len = %d, want 2 (blank and bad skipped)", len(got))
	}
	if got[0].String() != "10.0.0.0/8" || got[1].String() != "192.168.0.0/16" {
		t.Fatalf("parseCIDRList = %v", got)
	}
	if len(parseCIDRList("")) != 0 {
		t.Fatal("empty CSV should yield no nets")
	}
}

func TestIPInAny(t *testing.T) {
	nets := mustCIDR(t, "10.0.0.0/8")
	if !ipInAny(net.ParseIP("10.9.9.9"), nets) {
		t.Fatal("10.9.9.9 should be in 10.0.0.0/8")
	}
	if ipInAny(net.ParseIP("11.0.0.1"), nets) {
		t.Fatal("11.0.0.1 should not be in 10.0.0.0/8")
	}
	if ipInAny(net.ParseIP("1.2.3.4"), nil) {
		t.Fatal("nil net list should contain nothing")
	}
}

func TestUserAllowed(t *testing.T) {
	if !userAllowed(&RouteGroup{}, "anyone") {
		t.Fatal("empty AuthUsers should allow any user")
	}
	g := &RouteGroup{AuthUsers: []string{"Alice", "bob"}}
	if !userAllowed(g, "alice") {
		t.Fatal("membership should be case-insensitive")
	}
	if !userAllowed(g, "BOB") {
		t.Fatal("membership should be case-insensitive")
	}
	if userAllowed(g, "carol") {
		t.Fatal("carol is not in the allowlist")
	}
}

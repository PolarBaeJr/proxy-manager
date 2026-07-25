package main

import (
	"strings"
	"testing"
)

func TestValidServiceName(t *testing.T) {
	good := []string{"myapp", "app_1", "a.b-c", "A", "z9", strings.Repeat("a", 63)}
	for _, s := range good {
		if !validServiceName(s) {
			t.Errorf("validServiceName(%q) = false, want true", s)
		}
	}
	bad := []string{"", ".app", "-app", "_app", "<script>", `a"b`, "a'b", "a b", "a/b", strings.Repeat("a", 64)}
	for _, s := range bad {
		if validServiceName(s) {
			t.Errorf("validServiceName(%q) = true, want false", s)
		}
	}
}

func TestValidHostname(t *testing.T) {
	good := []string{"a", "myapp.polardev.org", "sub.the-aquarium.com", "1.2.3.4"}
	for _, s := range good {
		if !validHostname(s) {
			t.Errorf("validHostname(%q) = false, want true", s)
		}
	}
	bad := []string{"", "a b", "a/b", "http://a", "a:80", "a?b", "<x>", `a"b`, strings.Repeat("a", 254)}
	for _, s := range bad {
		if validHostname(s) {
			t.Errorf("validHostname(%q) = true, want false", s)
		}
	}
}

func TestValidMaintHost(t *testing.T) {
	good := []string{"a", "myapp.polardev.org", "sub.the-aquarium.com", "1.2.3.4", strings.Repeat("a", 253)}
	for _, s := range good {
		if !validMaintHost(s) {
			t.Errorf("validMaintHost(%q) = false, want true", s)
		}
	}
	bad := []string{
		"", ".", "..", "...", "-x", "x-", ".x", "x.", "a/b", `a\b`, "/etc/passwd",
		// Embedded "..": matches maintHostRE, so only the Contains check stops it.
		"a..b", "x..y.polardev.org",
		"MyApp.polardev.org", "a b", "a:80", "<x>", "a\x00b", strings.Repeat("a", 254),
	}
	for _, s := range bad {
		if validMaintHost(s) {
			t.Errorf("validMaintHost(%q) = true, want false", s)
		}
	}
	// Why the second validator exists: hostnameRE admits values that escape a
	// directory once joined, so validHostname must never gate a filename.
	if !validHostname("..") {
		t.Errorf(`validHostname("..") = false; the maintenance validator exists because it is true`)
	}
	if validMaintHost("..") {
		t.Errorf(`validMaintHost("..") = true, want false`)
	}
}

func TestValidProxyPath(t *testing.T) {
	good := []string{"/", "/api", "/a/b-c_d.e", "/" + strings.Repeat("a", 511)}
	for _, s := range good {
		if !validProxyPath(s) {
			t.Errorf("validProxyPath(%q) = false, want true", s)
		}
	}
	bad := []string{"", "api", "/a b", "/a?b", "/a#b", "/<x>", "/" + strings.Repeat("a", 512)}
	for _, s := range bad {
		if validProxyPath(s) {
			t.Errorf("validProxyPath(%q) = true, want false", s)
		}
	}
}

func TestValidRoutePath(t *testing.T) {
	good := []string{"", "/", "/admin", "/a/b-c_d.e"}
	for _, s := range good {
		if !validRoutePath(s) {
			t.Errorf("validRoutePath(%q) = false, want true", s)
		}
	}
	bad := []string{"admin", "/a b", "/a?b", "/<x>", "/" + strings.Repeat("a", 512)}
	for _, s := range bad {
		if validRoutePath(s) {
			t.Errorf("validRoutePath(%q) = true, want false", s)
		}
	}
}

func TestValidPort(t *testing.T) {
	for _, p := range []int{0, -1, 65536, 100000} {
		if validPort(p) {
			t.Errorf("validPort(%d) = true, want false", p)
		}
	}
	for _, p := range []int{1, 8094, 65535} {
		if !validPort(p) {
			t.Errorf("validPort(%d) = false, want true", p)
		}
	}
}

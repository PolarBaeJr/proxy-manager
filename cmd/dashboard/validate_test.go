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

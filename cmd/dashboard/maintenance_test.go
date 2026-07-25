package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintPathRejects(t *testing.T) {
	m := &maintStore{dir: t.TempDir()}
	bad := []string{
		".", "..", "...", "../x", "x/..", "/etc/passwd", "a/b", `a\b`, "a..b",
		"", "   ", "..%2fx", "%2e%2e%2f", "\x00", "a.b\x00.c",
		"-a", "a-", ".a", "a.", "．．", strings.Repeat("a", 254),
	}
	for _, s := range bad {
		p, err := m.path(s)
		if err == nil {
			t.Errorf("path(%q) = %q, want error", s, p)
		}
		if p != "" {
			t.Errorf("path(%q) returned %q on error, want empty path", s, p)
		}
	}
}

func TestMaintPathAccepts(t *testing.T) {
	dir := t.TempDir()
	m := &maintStore{dir: dir}
	cases := []struct{ in, want string }{
		{"a", "a"},
		{"myapp.polardev.org", "myapp.polardev.org"},
		{"sub.the-aquarium.com", "sub.the-aquarium.com"},
		// nginx lowercases $host, so the normalization has to be load-bearing.
		{"MyApp.PolarDev.org", "myapp.polardev.org"},
		{strings.Repeat("a", 253), strings.Repeat("a", 253)},
	}
	for _, c := range cases {
		p, err := m.path(c.in)
		if err != nil {
			t.Errorf("path(%q) = error %v, want ok", c.in, err)
			continue
		}
		if p != filepath.Join(dir, c.want) {
			t.Errorf("path(%q) = %q, want %q", c.in, p, filepath.Join(dir, c.want))
		}
	}
}

func TestMaintStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &maintStore{dir: dir}
	const host = "myapp.polardev.org"

	if err := m.On(host); err != nil {
		t.Fatalf("On: %v", err)
	}
	if err := m.On(host); err != nil {
		t.Fatalf("On (second call must be idempotent): %v", err)
	}
	hosts, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != host {
		t.Fatalf("List = %v, want [%s]", hosts, host)
	}
	on, err := m.IsOn(host)
	if err != nil || !on {
		t.Fatalf("IsOn = %v, %v; want true, nil", on, err)
	}
	// The flag must sit DIRECTLY in the dir — nginx only looks there.
	if _, err := os.Stat(filepath.Join(dir, host)); err != nil {
		t.Fatalf("stat flag file: %v", err)
	}

	if err := m.Off(host); err != nil {
		t.Fatalf("Off: %v", err)
	}
	if err := m.Off(host); err != nil {
		t.Fatalf("Off (second call must be idempotent): %v", err)
	}
	on, err = m.IsOn(host)
	if err != nil || on {
		t.Fatalf("IsOn after Off = %v, %v; want false, nil", on, err)
	}
	hosts, err = m.List()
	if err != nil {
		t.Fatalf("List after Off: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("List after Off = %v, want empty", hosts)
	}
}

func TestMaintOffRejectsUnknown(t *testing.T) {
	dir := t.TempDir()
	m := &maintStore{dir: dir}
	if err := m.Off("gone.polardev.org"); err != nil {
		t.Fatalf("Off on empty dir = %v, want nil", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("dir contains %d entries after Off, want 0", len(ents))
	}
}

func TestMaintUnconfigured(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	m, msgs := newMaintFromEnv(func(k string) string {
		if k == "MAINT_DIR" {
			return missing
		}
		return ""
	})
	if m != nil {
		t.Fatalf("newMaintFromEnv = %v, want nil for a missing dir", m)
	}
	if len(msgs) == 0 {
		t.Errorf("newMaintFromEnv returned no log message for a missing dir")
	}
	// Creating the dir would make the feature claim to be configured while
	// writing flags nginx never reads.
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("newMaintFromEnv created %q (stat err = %v), want it left alone", missing, err)
	}
	// nil receiver: the mux registers the routes unconditionally.
	if p, err := m.path("myapp.polardev.org"); err == nil || p != "" {
		t.Errorf("nil path = %q, %v; want empty path + error", p, err)
	}
	if _, err := m.List(); err == nil {
		t.Errorf("nil List = nil error, want error")
	}
	if _, err := m.IsOn("myapp.polardev.org"); err == nil {
		t.Errorf("nil IsOn = nil error, want error")
	}
	if err := m.On("myapp.polardev.org"); err == nil {
		t.Errorf("nil On = nil error, want error")
	}
	if err := m.Off("myapp.polardev.org"); err == nil {
		t.Errorf("nil Off = nil error, want error")
	}
}

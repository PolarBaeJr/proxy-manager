package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestReadWriteRoutesFilePreservesAllFields is the regression test for the
// data-loss bug: routesEntry was missing several fields staticRoute (the
// proxy's own read of the same file) carries, so every dashboard
// read-modify-write of routes.json silently dropped them. The fixture is
// raw JSON on disk (not a struct literal) so the test actually exercises
// readRoutesFile's decode, not just writeRoutesFile's encode.
func TestReadWriteRoutesFilePreservesAllFields(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "routes.json")
	raw := `{
  "routes": [
    {
      "name": "curated",
      "host": "svc.example.com",
      "path": "/admin",
      "strip": true,
      "backends": ["http://10.0.0.1:8080"],
      "service": "auth",
      "health": "/healthz",
      "auth": true,
      "auth_users": ["alice", "bob"],
      "auth_mode": "basic",
      "ratelimit": true,
      "ratelimit_rpm": 120,
      "onboarded": "auth"
    }
  ]
}`
	if err := os.WriteFile(src, []byte(raw), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := readRoutesFile(src)
	if err != nil {
		t.Fatalf("readRoutesFile: %v", err)
	}
	if len(f.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(f.Routes))
	}
	want := routesEntry{
		Name:      "curated",
		Host:      "svc.example.com",
		Path:      "/admin",
		Strip:     true,
		Backends:  []string{"http://10.0.0.1:8080"},
		Service:   "auth",
		Health:    "/healthz",
		Auth:      true,
		AuthUsers: []string{"alice", "bob"},
		AuthMode:  "basic",
		RateLimit: true,
		RateRPM:   120,
		Onboarded: "auth",
	}
	if !reflect.DeepEqual(f.Routes[0], want) {
		t.Fatalf("decoded entry = %+v, want %+v", f.Routes[0], want)
	}

	dst := filepath.Join(dir, "routes-out.json")
	if err := writeRoutesFile(dst, f); err != nil {
		t.Fatalf("writeRoutesFile: %v", err)
	}
	out, err := readRoutesFile(dst)
	if err != nil {
		t.Fatalf("re-read after write: %v", err)
	}
	if len(out.Routes) != 1 {
		t.Fatalf("re-read routes = %d, want 1", len(out.Routes))
	}
	if !reflect.DeepEqual(out.Routes[0], want) {
		t.Fatalf("round-tripped entry = %+v, want %+v", out.Routes[0], want)
	}
}

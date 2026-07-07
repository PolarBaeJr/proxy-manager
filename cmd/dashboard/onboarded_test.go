package main

import (
	"path/filepath"
	"testing"
)

func TestNextCloneIndex(t *testing.T) {
	clones := []dockerContainer{
		{Names: []string{"/goproxy-onb-app-1"}},
		{Names: []string{"/goproxy-onb-app-3"}},
		{Names: []string{"/unrelated"}},
	}
	if got := nextCloneIndex(clones, "app"); got != 4 {
		t.Fatalf("nextCloneIndex = %d, want 4", got)
	}
	if got := nextCloneIndex(nil, "app"); got != 1 {
		t.Fatalf("nextCloneIndex(empty) = %d, want 1", got)
	}
}

func TestSortByNameDesc(t *testing.T) {
	in := []dockerContainer{
		{Names: []string{"/goproxy-onb-app-1"}},
		{Names: []string{"/goproxy-onb-app-3"}},
		{Names: []string{"/goproxy-onb-app-2"}},
	}
	sortByNameDesc(in)
	got := []string{in[0].name(), in[1].name(), in[2].name()}
	want := []string{"goproxy-onb-app-3", "goproxy-onb-app-2", "goproxy-onb-app-1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortByNameDesc = %v, want %v", got, want)
		}
	}
}

func TestUpsertOnboardedRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	// Seed a user-curated entry that must survive upserts.
	if err := writeRoutesFile(path, routesFile{Routes: []routesEntry{
		{Name: "curated", Host: "curated.example.org", Backends: []string{"http://c:80"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// Insert.
	if err := upsertOnboardedRoute(path, "myapp", "myapp.example.org", []string{"http://myapp:3000"}); err != nil {
		t.Fatal(err)
	}
	f, err := readRoutesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Routes) != 2 {
		t.Fatalf("after insert routes = %d, want 2", len(f.Routes))
	}

	// Update in place — count unchanged, host updated, curated preserved.
	if err := upsertOnboardedRoute(path, "myapp", "new.example.org", []string{"http://myapp:4000"}); err != nil {
		t.Fatal(err)
	}
	f, _ = readRoutesFile(path)
	if len(f.Routes) != 2 {
		t.Fatalf("after update routes = %d, want 2 (in-place)", len(f.Routes))
	}
	var curatedOK, onboardedOK bool
	for _, r := range f.Routes {
		if r.Onboarded == "" && r.Host == "curated.example.org" {
			curatedOK = true
		}
		if r.Onboarded == "myapp" && r.Host == "new.example.org" && r.Backends[0] == "http://myapp:4000" {
			onboardedOK = true
		}
	}
	if !curatedOK {
		t.Fatal("curated entry was clobbered")
	}
	if !onboardedOK {
		t.Fatal("onboarded entry not updated in place")
	}
}

func TestRemoveOnboardedRoute(t *testing.T) {
	// Missing file → no-op (no error).
	missing := filepath.Join(t.TempDir(), "routes.json")
	if err := removeOnboardedRoute(missing, "myapp"); err != nil {
		t.Fatalf("remove on missing file = %v, want nil", err)
	}

	// Existing: onboarded entry removed, curated kept.
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := writeRoutesFile(path, routesFile{Routes: []routesEntry{
		{Name: "curated", Host: "curated.example.org", Backends: []string{"http://c:80"}},
		{Name: "onboarded: myapp", Host: "myapp.example.org", Backends: []string{"http://myapp:3000"}, Onboarded: "myapp"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := removeOnboardedRoute(path, "myapp"); err != nil {
		t.Fatal(err)
	}
	f, _ := readRoutesFile(path)
	if len(f.Routes) != 1 || f.Routes[0].Host != "curated.example.org" {
		t.Fatalf("after remove routes = %+v, want only curated", f.Routes)
	}
}

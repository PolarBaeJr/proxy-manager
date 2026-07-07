package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagedOnlyGuards proves the lifecycle actions reject a managed-only
// record (empty Host) BEFORE any Docker IO — the guard sits ahead of listAll,
// so a zero-value *dockerClient (no socket) never gets called.
func TestManagedOnlyGuards(t *testing.T) {
	store, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(OnboardedService{Name: "mo", Host: "", Image: "img", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	dc := &dockerClient{} // zero value: any Docker call would fail/panic
	ctx := context.Background()
	routes := filepath.Join(t.TempDir(), "routes.json")

	check := func(label string, err error) {
		if err == nil || !strings.Contains(err.Error(), "managed-only") {
			t.Fatalf("%s: err = %v, want a managed-only error", label, err)
		}
	}
	check("scale", dc.scaleOnboarded(ctx, "mo", 2, store, routes))
	check("stage", dc.stageOnboarded(ctx, "mo", ReplaceServiceRequest{Image: "x"}, store, routes))
	check("replace", dc.replaceOnboarded(ctx, "mo", ReplaceServiceRequest{Image: "x"}, store, routes))
	check("promote", dc.promoteOnboarded(ctx, "mo", store, routes))
}

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

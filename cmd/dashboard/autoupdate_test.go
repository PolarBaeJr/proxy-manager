package main

import (
	"path/filepath"
	"testing"
)

func TestShouldAutoUpdate(t *testing.T) {
	base := Service{Name: "app", Image: "img:latest", AutoUpdate: true}
	avail := &imageStatus{Image: "img:latest", UpdateAvailable: true}
	cases := []struct {
		name  string
		svc   Service
		st    *imageStatus
		fails int
		want  bool
	}{
		{"opted in + update available", base, avail, 0, true},
		{"not opted in", Service{Name: "app", Image: "img:latest"}, avail, 0, false},
		{"nil status", base, nil, 0, false},
		{"no update available", base, &imageStatus{Image: "img:latest"}, 0, false},
		{"checker error", base, &imageStatus{Image: "img:latest", UpdateAvailable: true, Err: "boom"}, 0, false},
		{"canary in flight", func() Service { s := base; s.CanaryImage = "img:new"; return s }(), avail, 0, false},
		{"all replicas stopped", func() Service { s := base; s.AllStopped = true; return s }(), avail, 0, false},
		{"empty image", func() Service { s := base; s.Image = ""; return s }(), avail, 0, false},
		{"failures at cap", base, avail, autoUpdateMaxFailures, false},
		{"failures below cap", base, avail, autoUpdateMaxFailures - 1, true},
	}
	for _, tc := range cases {
		if got := shouldAutoUpdate(tc.svc, tc.st, tc.fails); got != tc.want {
			t.Errorf("%s: shouldAutoUpdate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSetAutoUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarded.json")
	store, err := loadOnboardedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(OnboardedService{Name: "app", Host: "app.example.org", Image: "img", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAutoUpdate("app", true); err != nil {
		t.Fatal(err)
	}
	// Persists through a reload round-trip.
	store2, err := loadOnboardedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store2.Get("app")
	if !ok || !got.AutoUpdate {
		t.Fatalf("after reload AutoUpdate = %v (found=%v), want true", got.AutoUpdate, ok)
	}
	if err := store2.SetAutoUpdate("app", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := store2.Get("app"); got.AutoUpdate {
		t.Fatal("SetAutoUpdate(false) did not clear the flag")
	}
	if err := store2.SetAutoUpdate("nope", true); err == nil {
		t.Fatal("SetAutoUpdate on unknown name should error")
	}
}

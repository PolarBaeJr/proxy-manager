package main

import (
	"strings"
	"testing"
)

func TestBatchOnboardTargets(t *testing.T) {
	longName := "svc_" + strings.Repeat("x", 70) // > 63 chars, invalid
	items := []discoveryItem{
		{Name: "supabase_db_badminton_dev", Project: "supabase_badminton_dev"},
		{Name: "realtime-dev.supabase_realtime_badminton_dev", Project: "supabase_badminton_dev"},
		{Name: "supabase_studio_badminton_dev", Project: "supabase_badminton_dev"},
		{Name: longName, Project: "supabase_badminton_dev"},
		{Name: "market_db_prod", Project: "market_tracker_prod"},
	}

	onboarded := map[string]bool{"supabase_studio_badminton_dev": true}
	already := func(n string) bool { return onboarded[n] }

	targets, skipped := batchOnboardTargets(items, "supabase_badminton_dev", already)

	// Only members of the target project are considered — market_db_prod excluded.
	wantTargets := []string{
		"realtime-dev.supabase_realtime_badminton_dev",
		"supabase_db_badminton_dev",
	}
	if len(targets) != len(wantTargets) {
		t.Fatalf("targets = %v, want %v", targets, wantTargets)
	}
	for i := range wantTargets {
		if targets[i] != wantTargets[i] {
			t.Fatalf("targets = %v, want %v (sorted)", targets, wantTargets)
		}
	}

	// Two skips: already-onboarded studio, and the over-long invalid name.
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Name] = s.Reason
	}
	if reasons["supabase_studio_badminton_dev"] != "already onboarded" {
		t.Fatalf("studio skip reason = %q, want %q", reasons["supabase_studio_badminton_dev"], "already onboarded")
	}
	if reasons[longName] != "invalid container name" {
		t.Fatalf("long-name skip reason = %q, want %q", reasons[longName], "invalid container name")
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v, want 2 entries", skipped)
	}
}

func TestBatchOnboardTargetsEmptyProject(t *testing.T) {
	items := []discoveryItem{{Name: "a", Project: "p"}}
	targets, skipped := batchOnboardTargets(items, "", func(string) bool { return false })
	if len(targets) != 0 {
		t.Fatalf("targets = %v, want empty", targets)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want empty", skipped)
	}
	// Non-nil so JSON marshals to [] not null.
	if targets == nil || skipped == nil {
		t.Fatalf("targets/skipped must be non-nil slices")
	}
}

func TestValidServiceNameRealCompose(t *testing.T) {
	valid := []string{
		"supabase_db_badminton_dev",
		"realtime-dev.supabase_realtime_badminton_dev",
	}
	for _, n := range valid {
		if !validServiceName(n) {
			t.Errorf("validServiceName(%q) = false, want true", n)
		}
	}
	name64 := strings.Repeat("a", 64)
	if validServiceName(name64) {
		t.Errorf("validServiceName(64-char) = true, want false")
	}
}

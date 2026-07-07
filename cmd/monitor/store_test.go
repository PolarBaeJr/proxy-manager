package main

import (
	"testing"
	"time"
)

func hostsTarget(byHost map[string]any) *Store {
	s := NewStore(time.Hour, 5*time.Second)
	s.state["t"] = &TargetState{Name: "t", Latest: &Sample{OK: true, Data: map[string]any{"by_host": byHost}}}
	return s
}

func TestTargetHostsTieBreak(t *testing.T) {
	s := hostsTarget(map[string]any{"b.com": 5.0, "a.com": 5.0, "c.com": 10.0})
	out := s.TargetHosts("t")
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	// total desc, then host ascending on ties.
	order := []string{out[0]["host"].(string), out[1]["host"].(string), out[2]["host"].(string)}
	if order[0] != "c.com" || order[1] != "a.com" || order[2] != "b.com" {
		t.Fatalf("order = %v, want [c.com a.com b.com]", order)
	}
}

func TestOverviewSortStable(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	s.state["zeta"] = &TargetState{Name: "zeta", Health: "up", EverReached: true}
	s.state["alpha"] = &TargetState{Name: "alpha", Health: "up", EverReached: true}
	s.state["mid"] = &TargetState{Name: "mid", Health: "down", EverReached: true}

	ov := s.Overview()
	targets := ov["targets"].([]map[string]any)
	got := []string{targets[0]["name"].(string), targets[1]["name"].(string), targets[2]["name"].(string)}
	if got[0] != "alpha" || got[1] != "mid" || got[2] != "zeta" {
		t.Fatalf("order = %v, want alphabetical", got)
	}
	if ov["health"].(string) != "degraded" {
		t.Fatalf("health = %v, want degraded (one down)", ov["health"])
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		ever bool
		fail int
		want string
	}{
		{false, 3, "absent"},
		{true, 3, "down"},
		{true, 1, "flaky"},
		{true, 0, "up"},
		{false, 0, "up"},
	}
	for _, c := range cases {
		got := classify(&TargetState{EverReached: c.ever, FailCount: c.fail})
		if got != c.want {
			t.Errorf("classify(ever=%v fail=%d) = %q, want %q", c.ever, c.fail, got, c.want)
		}
	}
}

func TestRecordEviction(t *testing.T) {
	s := NewStore(time.Minute, 5*time.Second)
	s.Record("t", "http://x", map[string]any{"total": 1.0}, nil)
	// Age the existing point out of the window.
	s.series["t"][0].At = time.Now().Add(-2 * time.Minute)
	s.Record("t", "http://x", map[string]any{"total": 2.0}, nil)
	if len(s.series["t"]) != 1 {
		t.Fatalf("series len = %d, want 1 (old point evicted)", len(s.series["t"]))
	}
}

func TestExportRestoreEviction(t *testing.T) {
	s := NewStore(time.Minute, 5*time.Second)
	s.state["t"] = &TargetState{Name: "t", URL: "http://x", EverReached: true}
	s.series["t"] = []*Sample{
		{At: time.Now().Add(-2 * time.Minute), OK: true, Total: 1, Data: map[string]any{"total": 1.0}},
		{At: time.Now().Add(-10 * time.Second), OK: true, Total: 2, Data: map[string]any{"total": 2.0}},
	}
	st := s.exportState()

	s2 := NewStore(time.Minute, 5*time.Second)
	s2.restoreState(st)
	series := s2.series["t"]
	if len(series) != 1 {
		t.Fatalf("restored series len = %d, want 1 (old evicted)", len(series))
	}
	if series[0].Data != nil {
		t.Fatal("restored sample should have Data stripped")
	}
}

func TestRate(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	s.series["t"] = []*Sample{
		{At: time.Now().Add(-10 * time.Second), OK: true, Delta: 5},
		{At: time.Now().Add(-5 * time.Second), OK: true, Delta: 10},
	}
	got := s.rate("t", time.Minute)
	if got <= 1.0 || got >= 2.0 {
		t.Fatalf("rate = %v, want ~1.5/s", got)
	}
	if s.rate("nobody", time.Minute) != 0 {
		t.Fatal("unknown target rate should be 0")
	}
}

func TestErrorPctRecent(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	s.state["t"] = &TargetState{Name: "t", Latest: &Sample{OK: true, Data: map[string]any{
		"by_status": map[string]any{"200": 90.0, "404": 5.0, "500": 5.0},
	}}}
	if got := s.errorPctRecent("t"); got != 10 {
		t.Fatalf("errorPctRecent = %v, want 10", got)
	}
	if got := s.errorPctRecent("nobody"); got != 0 {
		t.Fatalf("unknown target errorPctRecent = %v, want 0", got)
	}
}

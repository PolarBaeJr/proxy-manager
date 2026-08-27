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

// The live incident this guards: a dashboard card showing "Requests (5m): 5"
// right next to "Errors: 55%" computed from lifetime-cumulative counters
// spanning hours of history — reads as an active outage when there isn't
// one. TargetHosts must report the same trailing window as "Requests (5m)".
func TestTargetHostsWindowedDelta(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	now := time.Now()
	old := &Sample{
		At: now.Add(-10 * time.Minute), OK: true,
		Data: map[string]any{
			"by_host":        map[string]any{"a.com": 100.0},
			"by_host_status": map[string]any{"a.com": map[string]any{"200": 90.0, "500": 10.0}},
		},
	}
	latest := &Sample{
		At: now, OK: true,
		Data: map[string]any{
			"by_host":        map[string]any{"a.com": 150.0},
			"by_host_status": map[string]any{"a.com": map[string]any{"200": 130.0, "500": 20.0}},
		},
	}
	s.series["t"] = []*Sample{old, latest}
	s.state["t"] = &TargetState{Name: "t", Latest: latest}

	out := s.TargetHosts("t")
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	got := out[0]
	if got["total"].(float64) != 50 {
		t.Errorf("total = %v, want 50 (150-100 windowed delta, not the 150 lifetime total)", got["total"])
	}
	if got["errors"].(float64) != 10 {
		t.Errorf("errors = %v, want 10 (20-10 windowed delta, not the 20 lifetime total)", got["errors"])
	}
	if pct, _ := got["error_pct"].(float64); pct < 19.9 || pct > 20.1 {
		t.Errorf("error_pct = %v, want ~20 (10/50 windowed), not the ~15%% lifetime figure (30/150)", pct)
	}
}

// A process restart mid-window resets the proxy's cumulative counters lower
// than the baseline sample — the delta must clamp to the current value
// instead of going negative.
func TestTargetHostsCounterResetClampsToCurrentValue(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	now := time.Now()
	old := &Sample{
		At: now.Add(-10 * time.Minute), OK: true,
		Data: map[string]any{
			"by_host":        map[string]any{"a.com": 500.0},
			"by_host_status": map[string]any{"a.com": map[string]any{"200": 500.0}},
		},
	}
	latest := &Sample{
		At: now, OK: true,
		Data: map[string]any{
			"by_host":        map[string]any{"a.com": 20.0},
			"by_host_status": map[string]any{"a.com": map[string]any{"200": 20.0}},
		},
	}
	s.series["t"] = []*Sample{old, latest}
	s.state["t"] = &TargetState{Name: "t", Latest: latest}

	out := s.TargetHosts("t")
	if len(out) != 1 || out[0]["total"].(float64) != 20 {
		t.Fatalf("total = %v, want 20 (clamped to current value, not -480)", out[0]["total"])
	}
}

// With less than hostStatsWindow of history (monitor just started watching
// this target, or — as in TestTargetHostsTieBreak above — a test fixture
// with no series at all), there's no real 5m-ago baseline to diff against.
// Falling back to the raw cumulative value is closer to correct than
// reporting zero traffic.
func TestTargetHostsNoBaselineFallsBackToCumulative(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	latest := &Sample{At: time.Now(), OK: true, Data: map[string]any{"by_host": map[string]any{"a.com": 42.0}}}
	s.series["t"] = []*Sample{latest}
	s.state["t"] = &TargetState{Name: "t", Latest: latest}

	out := s.TargetHosts("t")
	if len(out) != 1 || out[0]["total"].(float64) != 42 {
		t.Fatalf("out = %v, want total=42", out)
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

func TestOverviewWindowedPassthrough(t *testing.T) {
	s := NewStore(time.Hour, 5*time.Second)
	s.state["reporting"] = &TargetState{Name: "reporting", Health: "up", EverReached: true, Latest: &Sample{OK: true, Data: map[string]any{
		"total": 10.0,
		"windowed": map[string]any{
			"last_1h":  map[string]any{"requests": 7.0, "bytes_out": 100.0, "errors": 1.0},
			"last_24h": map[string]any{"requests": 9.0, "bytes_out": 200.0, "errors": 2.0},
		},
	}}}
	s.state["stale"] = &TargetState{Name: "stale", Health: "up", EverReached: true, Latest: &Sample{OK: true, Data: map[string]any{
		"total": 5.0,
	}}}

	ov := s.Overview()
	if ov["windowed_requests_1h"].(uint64) != 7 {
		t.Fatalf("windowed_requests_1h = %v, want 7", ov["windowed_requests_1h"])
	}
	if ov["windowed_requests_24h"].(uint64) != 9 {
		t.Fatalf("windowed_requests_24h = %v, want 9", ov["windowed_requests_24h"])
	}

	targets := ov["targets"].([]map[string]any)
	for _, entry := range targets {
		if entry["name"] == "reporting" {
			if entry["windowed"] == nil {
				t.Fatal("reporting target should have windowed passthrough")
			}
		}
		if entry["name"] == "stale" {
			if entry["windowed"] != nil {
				t.Fatalf("stale target windowed = %v, want nil", entry["windowed"])
			}
		}
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

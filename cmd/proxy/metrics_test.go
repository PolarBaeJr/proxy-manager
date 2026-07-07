package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecordAndSnapshot(t *testing.T) {
	m := NewMetrics()
	m.Record("a.example.org", "GET", 200, 100, 5*time.Millisecond)
	m.Record("a.example.org", "POST", 500, 0, 7*time.Millisecond)
	m.Record("b.example.org", "GET", 200, 50, 3*time.Millisecond)

	snap := m.Snapshot()
	if snap["total"].(uint64) != 3 {
		t.Fatalf("total = %v, want 3", snap["total"])
	}
	if snap["bytes_out"].(uint64) != 150 {
		t.Fatalf("bytes_out = %v, want 150", snap["bytes_out"])
	}
	byHost := snap["by_host"].(map[string]uint64)
	if byHost["a.example.org"] != 2 || byHost["b.example.org"] != 1 {
		t.Fatalf("by_host = %v", byHost)
	}
	if snap["sample_size"].(int) != 3 {
		t.Fatalf("sample_size = %v, want 3", snap["sample_size"])
	}
}

func TestSnapshotPercentiles(t *testing.T) {
	m := NewMetrics()
	// Deterministic spread 1..100 ms.
	for i := 1; i <= 100; i++ {
		m.latencyMs = append(m.latencyMs, float64(i))
	}
	lat := m.Snapshot()["latency_ms"].(map[string]float64)
	// pct formula: index = int(len*p); sorted[index].
	if lat["p50"] != 51 || lat["p90"] != 91 || lat["p99"] != 100 || lat["max"] != 100 {
		t.Fatalf("percentiles = %v", lat)
	}

	// Empty reservoir → all zeros.
	empty := NewMetrics().Snapshot()["latency_ms"].(map[string]float64)
	for k, v := range empty {
		if v != 0 {
			t.Fatalf("empty %s = %v, want 0", k, v)
		}
	}
}

func TestExportRestoreRoundTrip(t *testing.T) {
	m := NewMetrics()
	m.Record("a.example.org", "GET", 200, 100, 5*time.Millisecond)
	m.Record("b.example.org", "POST", 404, 10, 8*time.Millisecond)
	st := m.exportState()

	m2 := NewMetrics()
	m2.restoreState(st)
	snap := m2.Snapshot()
	if snap["total"].(uint64) != 2 {
		t.Fatalf("restored total = %v, want 2", snap["total"])
	}
	if snap["bytes_out"].(uint64) != 110 {
		t.Fatalf("restored bytes_out = %v, want 110", snap["bytes_out"])
	}
	byHost := snap["by_host"].(map[string]uint64)
	if byHost["a.example.org"] != 1 || byHost["b.example.org"] != 1 {
		t.Fatalf("restored by_host = %v", byHost)
	}
	if snap["sample_size"].(int) != 2 {
		t.Fatalf("restored sample_size = %v, want 2", snap["sample_size"])
	}
}

func TestRestoreClampsLatency(t *testing.T) {
	st := metricsState{Version: metricsStateVersion}
	for i := 0; i < 1500; i++ {
		st.LatencyMs = append(st.LatencyMs, float64(i))
	}
	st.LatencyHead = 99999 // out of range for the clamped slice

	m := NewMetrics()
	m.restoreState(st)
	if len(m.latencyMs) != latencyWindow {
		t.Fatalf("latencyMs len = %d, want %d", len(m.latencyMs), latencyWindow)
	}
	if m.latencyHead != 0 {
		t.Fatalf("latencyHead = %d, want 0 (bad head ignored)", m.latencyHead)
	}
}

func TestUnroutedBucketing(t *testing.T) {
	m := NewMetrics()
	h := withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mu, ok := w.(interface{ MarkUnrouted() }); ok {
			mu.MarkUnrouted()
		}
		w.WriteHeader(404)
	}), m)

	req := httptest.NewRequest("GET", "http://scanner.evil.example/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	byHost := m.Snapshot()["by_host"].(map[string]uint64)
	if byHost[unroutedHost] != 1 {
		t.Fatalf("(unrouted) count = %d, want 1", byHost[unroutedHost])
	}
	if _, ok := byHost["scanner.evil.example"]; ok {
		t.Fatal("attacker host should not appear in by_host")
	}
}

package main

import (
	"testing"
	"time"
)

func TestWindowedRollover(t *testing.T) {
	m := NewMetrics()
	fake := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fake }
	m.Record("a.example.org", "GET", 200, 0, time.Millisecond)

	fake = fake.Add(6 * time.Minute)
	m.now = func() time.Time { return fake }
	win := m.Snapshot()["windowed"].(map[string]any)
	last5m := win["last_5m"].(map[string]any)
	if last5m["requests"].(uint64) != 0 {
		t.Fatalf("last_5m requests = %v, want 0 (request should have dropped out)", last5m["requests"])
	}
	last1h := win["last_1h"].(map[string]any)
	if last1h["requests"].(uint64) != 1 {
		t.Fatalf("last_1h requests = %v, want 1 (request should still be in window)", last1h["requests"])
	}
}

func TestWindowedBucketReuseResets(t *testing.T) {
	m := NewMetrics()
	fake := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fake }
	m.Record("a.example.org", "GET", 200, 0, time.Millisecond)

	fake = fake.Add(numBuckets * time.Minute)
	m.now = func() time.Time { return fake }
	m.Record("a.example.org", "GET", 200, 0, time.Millisecond)

	win := m.Snapshot()["windowed"].(map[string]any)
	last5m := win["last_5m"].(map[string]any)
	if last5m["requests"].(uint64) != 1 {
		t.Fatalf("last_5m requests = %v, want 1 (stale bucket reuse should reset, not accumulate)", last5m["requests"])
	}
}

func TestWindowedEmpty(t *testing.T) {
	win := NewMetrics().Snapshot()["windowed"].(map[string]any)
	for _, key := range []string{"last_5m", "last_1h", "last_24h"} {
		w := win[key].(map[string]any)
		if w["requests"].(uint64) != 0 || w["bytes_out"].(uint64) != 0 || w["errors"].(uint64) != 0 {
			t.Fatalf("%s = %v, want all zero", key, w)
		}
	}
}

func TestWindowedErrorPredicate(t *testing.T) {
	m := NewMetrics()
	m.Record("a.example.org", "GET", 200, 0, time.Millisecond)
	m.Record("a.example.org", "GET", 404, 0, time.Millisecond)
	m.Record("a.example.org", "GET", 599, 0, time.Millisecond)

	last5m := m.Snapshot()["windowed"].(map[string]any)["last_5m"].(map[string]any)
	if last5m["errors"].(uint64) != 2 {
		t.Fatalf("last_5m errors = %v, want 2", last5m["errors"])
	}
}

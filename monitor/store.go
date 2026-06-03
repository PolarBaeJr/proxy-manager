package main

import (
	"sync"
	"time"
)

// Store keeps per-target rolling time series + the latest snapshot.
//
// Each Record() call appends a point. We retain points for `window` duration
// (older ones evicted on read). For the dashboard, this gives ~hourly
// sparklines without external storage.

type Sample struct {
	At    time.Time      `json:"at"`
	OK    bool           `json:"ok"`
	Err   string         `json:"err,omitempty"`
	Total uint64         `json:"total"`            // running total at this point
	Delta uint64         `json:"delta"`            // requests since last sample
	Data  map[string]any `json:"data,omitempty"`   // full /metrics payload for the latest sample only
}

type TargetState struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Health    string    `json:"health"`     // up | flaky | down
	Latest    *Sample   `json:"latest,omitempty"`
	LastOK    time.Time `json:"last_ok"`
	FailCount int       `json:"fail_count"` // consecutive scrape failures
}

type Store struct {
	mu       sync.RWMutex
	window   time.Duration
	interval time.Duration
	series   map[string][]*Sample
	state    map[string]*TargetState
}

func NewStore(window, interval time.Duration) *Store {
	return &Store{
		window:   window,
		interval: interval,
		series:   map[string][]*Sample{},
		state:    map[string]*TargetState{},
	}
}

func (s *Store) Record(name, url string, data map[string]any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	st, ok := s.state[name]
	if !ok {
		st = &TargetState{Name: name, URL: url}
		s.state[name] = st
	}

	sample := &Sample{At: now, Data: data}
	if err != nil {
		sample.OK = false
		sample.Err = err.Error()
		st.FailCount++
	} else {
		sample.OK = true
		st.FailCount = 0
		st.LastOK = now
		if t, ok := data["total"].(float64); ok {
			sample.Total = uint64(t)
			// delta = current total - previous OK sample's total
			prev := s.series[name]
			for i := len(prev) - 1; i >= 0; i-- {
				if prev[i].OK {
					if sample.Total >= prev[i].Total {
						sample.Delta = sample.Total - prev[i].Total
					}
					break
				}
			}
		}
	}
	st.Latest = sample
	st.Health = classify(st)

	s.series[name] = append(s.series[name], sample)
	// Evict points older than window.
	cutoff := now.Add(-s.window)
	in := s.series[name]
	keep := in[:0]
	for _, p := range in {
		if p.At.After(cutoff) {
			keep = append(keep, p)
		}
	}
	s.series[name] = keep
}

func classify(st *TargetState) string {
	switch {
	case st.FailCount >= 3:
		return "down"
	case st.FailCount > 0:
		return "flaky"
	default:
		return "up"
	}
}

// Snapshot returns the current state of every target.
func (s *Store) Snapshot() map[string]*TargetState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*TargetState, len(s.state))
	for k, v := range s.state {
		copy := *v
		out[k] = &copy
	}
	return out
}

// Series returns the recent time-series for one target. `field` selects which
// scalar to extract per sample (currently supports: "total", "delta", "in_flight").
func (s *Store) Series(target, field string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	in := s.series[target]
	if field == "" {
		field = "delta"
	}
	out := make([]map[string]any, 0, len(in))
	for _, p := range in {
		var v float64
		switch field {
		case "delta":
			v = float64(p.Delta)
		case "total":
			v = float64(p.Total)
		case "in_flight":
			if p.Data != nil {
				if x, ok := p.Data["in_flight"].(float64); ok {
					v = x
				}
			}
		}
		out = append(out, map[string]any{"at": p.At.UTC().Format(time.RFC3339), "v": v, "ok": p.OK})
	}
	return out
}

// Overview aggregates the latest scrapes into a single dashboard-friendly view.
func (s *Store) Overview() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalReqs := uint64(0)
	allUp := true
	targets := []map[string]any{}
	for name, st := range s.state {
		entry := map[string]any{"name": name, "url": st.URL, "health": st.Health}
		if st.Health != "up" {
			allUp = false
		}
		if st.Latest != nil && st.Latest.Data != nil {
			d := st.Latest.Data
			entry["total"] = d["total"]
			entry["in_flight"] = d["in_flight"]
			entry["uptime_seconds"] = d["uptime_seconds"]
			entry["by_status"] = d["by_status"]
			entry["by_method"] = d["by_method"]
			entry["by_host"] = d["by_host"]
			entry["latency_ms"] = d["latency_ms"]
			if t, ok := d["total"].(float64); ok {
				totalReqs += uint64(t)
			}
		}
		targets = append(targets, entry)
	}

	healthy := "up"
	if !allUp {
		healthy = "degraded"
	}
	return map[string]any{
		"health":       healthy,
		"total_requests": totalReqs,
		"targets":      targets,
		"scraped_at":   time.Now().UTC().Format(time.RFC3339),
	}
}

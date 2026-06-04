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
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Health      string    `json:"health"`        // up | flaky | down | absent
	EverReached bool      `json:"ever_reached"`  // true once we've gotten ANY successful scrape
	Latest      *Sample   `json:"latest,omitempty"`
	LastOK      time.Time `json:"last_ok"`
	FailCount   int       `json:"fail_count"`    // consecutive scrape failures
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
		st.EverReached = true
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
	// "absent" — we've been told to scrape this target but never reached it.
	// Treated separately from "down" so we don't flag the stack degraded
	// because a target the user simply hasn't deployed (e.g. edge with the
	// profile off) is missing.
	if !st.EverReached && st.FailCount >= 3 {
		return "absent"
	}
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

// Target returns a single target's full latest state — health, raw /metrics
// payload, and derived rates (req/s averaged over the last minute and 5 min).
// Returns nil if the target is unknown.
func (s *Store) Target(name string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.state[name]
	if !ok {
		return nil
	}
	out := map[string]any{
		"name":         st.Name,
		"url":          st.URL,
		"health":       st.Health,
		"ever_reached": st.EverReached,
		"last_ok":      st.LastOK.UTC().Format(time.RFC3339),
		"fail_count":   st.FailCount,
	}
	if st.Latest != nil && st.Latest.Data != nil {
		out["metrics"] = st.Latest.Data
		out["latest_err"] = st.Latest.Err
	}
	out["rate_per_sec_1m"] = s.rate(name, 1*time.Minute)
	out["rate_per_sec_5m"] = s.rate(name, 5*time.Minute)
	out["error_pct_recent"] = s.errorPctRecent(name)
	return out
}

// rate computes requests-per-second over the trailing `window`, by summing
// the deltas of OK samples in that window.
func (s *Store) rate(name string, window time.Duration) float64 {
	cutoff := time.Now().Add(-window)
	var sum uint64
	var span time.Duration
	prev := s.series[name]
	earliestKept := time.Time{}
	for _, p := range prev {
		if p.At.Before(cutoff) || !p.OK {
			continue
		}
		if earliestKept.IsZero() {
			earliestKept = p.At
		}
		sum += p.Delta
	}
	if earliestKept.IsZero() {
		return 0
	}
	span = time.Since(earliestKept)
	if span <= 0 {
		return 0
	}
	return float64(sum) / span.Seconds()
}

// errorPctRecent is the share of recent (last 5m) requests with 4xx/5xx
// status, computed from the latest sample's by_status map. Approximate.
func (s *Store) errorPctRecent(name string) float64 {
	st, ok := s.state[name]
	if !ok || st.Latest == nil || st.Latest.Data == nil {
		return 0
	}
	byStatus, ok := st.Latest.Data["by_status"].(map[string]any)
	if !ok {
		return 0
	}
	var total, errs float64
	for k, v := range byStatus {
		n, ok := v.(float64)
		if !ok || len(k) == 0 {
			continue
		}
		total += n
		if k[0] == '4' || k[0] == '5' {
			errs += n
		}
	}
	if total == 0 {
		return 0
	}
	return 100 * errs / total
}

// TargetHosts returns the per-host traffic + error breakdown for one target.
// Sorted by total requests descending.
func (s *Store) TargetHosts(name string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.state[name]
	if !ok || st.Latest == nil || st.Latest.Data == nil {
		return nil
	}
	byHost, _ := st.Latest.Data["by_host"].(map[string]any)
	byHostStatus, _ := st.Latest.Data["by_host_status"].(map[string]any)
	out := make([]map[string]any, 0, len(byHost))
	for host, total := range byHost {
		totalN, _ := total.(float64)
		entry := map[string]any{"host": host, "total": totalN}
		var errs float64
		if statuses, ok := byHostStatus[host].(map[string]any); ok {
			entry["by_status"] = statuses
			for k, v := range statuses {
				n, _ := v.(float64)
				if len(k) > 0 && (k[0] == '4' || k[0] == '5') {
					errs += n
				}
			}
		}
		if totalN > 0 {
			entry["error_pct"] = 100 * errs / totalN
		}
		entry["errors"] = errs
		out = append(out, entry)
	}
	// Sort by total descending.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j]["total"].(float64) > out[i]["total"].(float64) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TargetErrors returns just the failing requests, grouped by status and host.
func (s *Store) TargetErrors(name string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.state[name]
	if !ok || st.Latest == nil || st.Latest.Data == nil {
		return nil
	}
	byStatus, _ := st.Latest.Data["by_status"].(map[string]any)
	byHostStatus, _ := st.Latest.Data["by_host_status"].(map[string]any)
	errsByStatus := map[string]float64{}
	errsByHost := map[string]float64{}
	totalErrors := 0.0
	for k, v := range byStatus {
		n, _ := v.(float64)
		if len(k) > 0 && (k[0] == '4' || k[0] == '5') {
			errsByStatus[k] = n
			totalErrors += n
		}
	}
	for host, statuses := range byHostStatus {
		m, _ := statuses.(map[string]any)
		for k, v := range m {
			n, _ := v.(float64)
			if len(k) > 0 && (k[0] == '4' || k[0] == '5') {
				errsByHost[host] += n
			}
		}
	}
	return map[string]any{
		"total_errors":   totalErrors,
		"by_status":      errsByStatus,
		"by_host":        errsByHost,
		"by_host_status": byHostStatus,
	}
}

// Overview aggregates the latest scrapes into a single dashboard-friendly view.
func (s *Store) Overview() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalReqs := uint64(0)
	allUp := true
	targets := []map[string]any{}
	for name, st := range s.state {
		entry := map[string]any{"name": name, "url": st.URL, "health": st.Health, "ever_reached": st.EverReached}
		// "absent" targets are excluded from the overall health calculation —
		// they're listed (UI can render them as "not deployed") but don't make
		// the stack look degraded.
		if st.Health != "up" && st.Health != "absent" {
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

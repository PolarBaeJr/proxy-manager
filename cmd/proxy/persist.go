// Metrics persistence: periodic JSON snapshots so counters survive restarts.
// Best-effort by design — a failed save never affects the request path.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// loadMetricsState reads a saved snapshot. Missing file is the normal first
// boot — silent false. Corruption or a version mismatch logs and starts
// fresh; never fatal.
func loadMetricsState(path string) (metricsState, bool) {
	var st metricsState
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("metrics state: read %s: %v", path, err)
		}
		return metricsState{}, false
	}
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("metrics state: parse %s: %v (starting fresh)", path, err)
		return metricsState{}, false
	}
	if st.Version != metricsStateVersion {
		log.Printf("metrics state: %s has version %d, want %d (starting fresh)", path, st.Version, metricsStateVersion)
		return metricsState{}, false
	}
	return st, true
}

func saveMetricsState(path string, m *Metrics) error {
	st := m.exportState()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write to a sibling temp file, then rename — a same-directory rename is
	// atomic, so readers never see a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func persistLoop(ctx context.Context, path string, interval time.Duration, m *Metrics) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := saveMetricsState(path, m); err != nil {
				log.Printf("metrics state: save: %v", err)
			}
		}
	}
}

// saveOnShutdown flushes one final snapshot on SIGTERM/interrupt, then exits.
func saveOnShutdown(path string, m *Metrics) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-ch
		if err := saveMetricsState(path, m); err != nil {
			log.Printf("metrics state: shutdown save: %v", err)
		}
		os.Exit(0)
	}()
}

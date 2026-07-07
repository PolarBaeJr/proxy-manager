package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "metrics.json")
	m := NewMetrics()
	m.Record("a.example.org", "GET", 200, 100, 5*time.Millisecond)
	m.Record("b.example.org", "POST", 500, 0, 9*time.Millisecond)

	if err := saveMetricsState(path, m); err != nil {
		t.Fatalf("saveMetricsState: %v", err)
	}
	st, ok := loadMetricsState(path)
	if !ok {
		t.Fatal("loadMetricsState returned ok=false")
	}
	if st.Total != 2 || st.BytesOut != 100 {
		t.Fatalf("loaded state total=%d bytes=%d", st.Total, st.BytesOut)
	}
	if st.ByHost["a.example.org"] != 1 || st.ByHost["b.example.org"] != 1 {
		t.Fatalf("loaded by_host = %v", st.ByHost)
	}
}

func TestLoadMissingFile(t *testing.T) {
	st, ok := loadMetricsState(filepath.Join(t.TempDir(), "nope.json"))
	if ok {
		t.Fatal("missing file should return ok=false")
	}
	if st.Total != 0 {
		t.Fatal("missing file should return zero state")
	}
}

func TestLoadVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	b, _ := json.Marshal(metricsState{Version: metricsStateVersion + 99, Total: 5})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadMetricsState(path); ok {
		t.Fatal("version mismatch should return ok=false")
	}
}

func TestLoadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, ok := loadMetricsState(path)
	if ok || st.Total != 0 {
		t.Fatal("corrupt file should return zero state, ok=false")
	}
}

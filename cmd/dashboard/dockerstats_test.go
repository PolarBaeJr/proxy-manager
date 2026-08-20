package main

import "testing"

func TestCPUPercent(t *testing.T) {
	prev := cpuStatsJSON{SystemUsage: 1000, OnlineCPUs: 2}
	prev.CPUUsage.TotalUsage = 100
	cur := cpuStatsJSON{SystemUsage: 2000, OnlineCPUs: 2}
	cur.CPUUsage.TotalUsage = 300

	// cpu_delta=200, sys_delta=1000 → (200/1000)*2*100 = 40%
	got := cpuPercent(cur, prev)
	if got != 40 {
		t.Fatalf("cpuPercent = %v, want 40", got)
	}
}

func TestCPUPercentFallsBackToPercpuLen(t *testing.T) {
	prev := cpuStatsJSON{SystemUsage: 1000}
	prev.CPUUsage.TotalUsage = 100
	cur := cpuStatsJSON{SystemUsage: 2000}
	cur.CPUUsage.TotalUsage = 300
	cur.CPUUsage.PercpuUsage = []uint64{1, 2, 3, 4} // OnlineCPUs absent → falls back to len == 4

	got := cpuPercent(cur, prev)
	want := (200.0 / 1000.0) * 4 * 100
	if got != want {
		t.Fatalf("cpuPercent = %v, want %v", got, want)
	}
}

func TestCPUPercentNoPreviousSample(t *testing.T) {
	// precpu_stats is zero-valued on a container's first sample — must not
	// divide by zero or report a bogus percentage.
	var prev cpuStatsJSON
	cur := cpuStatsJSON{SystemUsage: 2000}
	cur.CPUUsage.TotalUsage = 300
	if got := cpuPercent(cur, prev); got != 0 {
		t.Fatalf("cpuPercent = %v, want 0", got)
	}
}

func TestMemUsedCgroupV1Cache(t *testing.T) {
	m := memoryStatsJSON{Usage: 1000, Limit: 5000, Stats: map[string]uint64{"cache": 300}}
	if got := memUsed(m); got != 700 {
		t.Fatalf("memUsed = %v, want 700", got)
	}
}

func TestMemUsedCgroupV2InactiveFile(t *testing.T) {
	m := memoryStatsJSON{Usage: 1000, Limit: 5000, Stats: map[string]uint64{"inactive_file": 400}}
	if got := memUsed(m); got != 600 {
		t.Fatalf("memUsed = %v, want 600", got)
	}
}

func TestMemUsedNoCacheField(t *testing.T) {
	m := memoryStatsJSON{Usage: 1000, Limit: 5000, Stats: map[string]uint64{}}
	if got := memUsed(m); got != 1000 {
		t.Fatalf("memUsed = %v, want 1000 (no cache/inactive_file field to subtract)", got)
	}
}

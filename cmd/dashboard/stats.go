package main

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Lightweight host system stats — CPU %, memory, disk free — surfaced in the
// dashboard header. Reads from a /host-proc + /host-root bind mount so we get
// HOST numbers (not the dashboard container's view).

type SysStats struct {
	CPUPercent float64 `json:"cpu_pct"`
	MemTotal   uint64  `json:"mem_total"`
	MemFree    uint64  `json:"mem_free"`
	MemUsed    uint64  `json:"mem_used"`
	DiskTotal  uint64  `json:"disk_total"`
	DiskFree   uint64  `json:"disk_free"`
	DiskUsed   uint64  `json:"disk_used"`
}

const (
	procStatPath    = "/host-proc/stat"
	procMeminfoPath = "/host-proc/meminfo"
	hostRootPath    = "/host-root" // statfs target
)

var (
	cpuMu       sync.Mutex
	prevIdle    uint64
	prevTotal   uint64
	cpuFraction float64 // 0.0 – 1.0, updated by sampleCPU
)

func sampleCPU() {
	f, err := os.Open(procStatPath)
	if err != nil {
		// Try in-container /proc as a fallback (works on Linux without user-ns).
		f, err = os.Open("/proc/stat")
		if err != nil {
			return
		}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return
	}
	line := scanner.Text()
	// "cpu  3357 0 4313 1362393 ..." — user nice system idle iowait irq softirq steal
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return
		}
		total += v
		// idle = field[4] (idle) + field[5] (iowait, if present)
		if i == 4 || i == 5 {
			idle += v
		}
	}

	cpuMu.Lock()
	defer cpuMu.Unlock()
	if prevTotal > 0 && total > prevTotal {
		dTotal := total - prevTotal
		dIdle := idle - prevIdle
		if dTotal > 0 {
			cpuFraction = 1 - float64(dIdle)/float64(dTotal)
		}
	}
	prevTotal = total
	prevIdle = idle
}

func memStats() (total, free uint64) {
	path := procMeminfoPath
	if _, err := os.Stat(path); err != nil {
		path = "/proc/meminfo"
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var v uint64
		v, err = strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		v *= 1024 // /proc/meminfo is in kB
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			free = v
		}
		if total > 0 && free > 0 {
			break
		}
	}
	return
}

func diskStats(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total = st.Blocks * uint64(st.Bsize)
	free = st.Bavail * uint64(st.Bsize)
	return
}

func GetStats() SysStats {
	cpuMu.Lock()
	cpu := cpuFraction * 100
	cpuMu.Unlock()

	memT, memF := memStats()
	dpath := hostRootPath
	if _, err := os.Stat(dpath); err != nil {
		dpath = "/"
	}
	dT, dF := diskStats(dpath)
	return SysStats{
		CPUPercent: cpu,
		MemTotal:   memT,
		MemFree:    memF,
		MemUsed:    memT - memF,
		DiskTotal:  dT,
		DiskFree:   dF,
		DiskUsed:   dT - dF,
	}
}

func statsLoop(ctx context.Context) {
	sampleCPU()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sampleCPU()
		}
	}
}

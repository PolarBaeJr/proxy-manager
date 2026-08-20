package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Bounded-concurrency CPU/mem poller over every running container backing a
// proxy.service-labeled service (routed or not — a DB-only container gets
// polled too). Package-level state mirrors stats.go's cpuMu/cpuFraction +
// statsLoop/GetStats shape rather than threading a cache object through
// newDashboardMux — that constructor already has a dozen params and five
// call sites (several in tests), and this cache has exactly one writer
// (dockerStatsLoop) and any number of readers (the /api/service-status
// handler), same as the host stats.
const (
	dockerStatsInterval = 10 * time.Second
	dockerStatsWorkers  = 6
)

type containerStats struct {
	CPUPercent    float64
	MemUsedBytes  uint64
	MemLimitBytes uint64
}

var (
	dockerStatsMu        sync.RWMutex
	dockerStatsByID      = map[string]containerStats{}
	dockerStatsSampledAt time.Time
	dockerStatsInFlight  bool
)

// GetDockerStats returns a container's last-sampled CPU/mem, or false if it
// hasn't been sampled yet (container just started, or isn't running so the
// poller skips it).
func GetDockerStats(id string) (containerStats, bool) {
	dockerStatsMu.RLock()
	defer dockerStatsMu.RUnlock()
	s, ok := dockerStatsByID[id]
	return s, ok
}

// DockerStatsSampledAt is the timestamp of the last completed poll pass —
// separate from the request-time "sampled_at" the /api/service-status
// response also carries, since stats are cached and can legitimately be a
// few seconds stale.
func DockerStatsSampledAt() time.Time {
	dockerStatsMu.RLock()
	defer dockerStatsMu.RUnlock()
	return dockerStatsSampledAt
}

// dockerStatsLoop mirrors statsLoop's ticker/context-cancellation shape.
func dockerStatsLoop(ctx context.Context, dc *dockerClient) {
	t := time.NewTicker(dockerStatsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pollDockerStats(ctx, dc)
		}
	}
}

// pollDockerStats gathers every running container across all services and
// samples CPU/mem for each with a small worker pool. Skips the tick entirely
// if the previous pass is still running — a full pass over 10-15 containers
// can take several seconds, and overlapping polls would just pile up
// in-flight Docker requests.
func pollDockerStats(ctx context.Context, dc *dockerClient) {
	dockerStatsMu.Lock()
	if dockerStatsInFlight {
		dockerStatsMu.Unlock()
		return
	}
	dockerStatsInFlight = true
	dockerStatsMu.Unlock()
	defer func() {
		dockerStatsMu.Lock()
		dockerStatsInFlight = false
		dockerStatsMu.Unlock()
	}()

	svcs, err := dc.listServices(ctx)
	if err != nil {
		log.Printf("docker stats: listServices: %v", err)
		return
	}
	seen := map[string]bool{}
	var ids []string
	for _, s := range svcs {
		for _, m := range s.Members {
			// Stats on a stopped container return zeros or error every tick —
			// pointless load and log noise for any service with a stopped replica.
			if m.State != "running" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}

	type result struct {
		id    string
		stats containerStats
	}
	jobs := make(chan string)
	results := make(chan result, len(ids))
	var wg sync.WaitGroup
	for i := 0; i < dockerStatsWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				st, err := fetchContainerStats(ctx, dc, id)
				if err != nil {
					continue
				}
				results <- result{id: id, stats: st}
			}
		}()
	}
	go func() {
		for _, id := range ids {
			jobs <- id
		}
		close(jobs)
	}()
	wg.Wait()
	close(results)

	fresh := make(map[string]containerStats, len(ids))
	for r := range results {
		fresh[r.id] = r.stats
	}

	dockerStatsMu.Lock()
	dockerStatsByID = fresh
	dockerStatsSampledAt = time.Now()
	dockerStatsMu.Unlock()
}

// dockerStatsResponse is the subset of Docker's
// GET /containers/{id}/stats?stream=false response this needs.
type dockerStatsResponse struct {
	CPUStats    cpuStatsJSON    `json:"cpu_stats"`
	PreCPUStats cpuStatsJSON    `json:"precpu_stats"`
	MemoryStats memoryStatsJSON `json:"memory_stats"`
}

type cpuStatsJSON struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint64 `json:"online_cpus"`
}

type memoryStatsJSON struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

func fetchContainerStats(ctx context.Context, dc *dockerClient, id string) (containerStats, error) {
	body, err := dc.get(ctx, "/containers/"+id+"/stats?stream=false")
	if err != nil {
		return containerStats{}, err
	}
	defer body.Close()
	var resp dockerStatsResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return containerStats{}, err
	}
	return containerStats{
		CPUPercent:    cpuPercent(resp.CPUStats, resp.PreCPUStats),
		MemUsedBytes:  memUsed(resp.MemoryStats),
		MemLimitBytes: resp.MemoryStats.Limit,
	}, nil
}

// cpuPercent computes CPU% the same way `docker stats` does: the container's
// share of system-wide CPU time consumed since the previous sample, times
// the number of CPUs. cpu_usage.total_usage alone is a monotonic counter,
// not a percentage — it must be diffed against precpu_stats from the same
// response.
func cpuPercent(cur, prev cpuStatsJSON) float64 {
	if prev.SystemUsage == 0 {
		// precpu_stats comes back all-zero on a container's first sample —
		// there's no real previous point to diff against, so treating the
		// zero baseline as a genuine delta would report a bogus percentage.
		return 0
	}
	cpuDelta := float64(cur.CPUUsage.TotalUsage) - float64(prev.CPUUsage.TotalUsage)
	sysDelta := float64(cur.SystemUsage) - float64(prev.SystemUsage)
	if sysDelta <= 0 || cpuDelta < 0 {
		// No previous sample yet (container just started) or a decode/read
		// race — 0 is the honest answer, not a divide-by-zero guess.
		return 0
	}
	onlineCPUs := float64(cur.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(cur.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}
	return (cpuDelta / sysDelta) * onlineCPUs * 100
}

// memUsed subtracts page cache out of the raw usage figure — cgroup v1
// reports it under stats.cache, cgroup v2 under stats.inactive_file. Naive
// usage/limit overcounts page cache as "used", which is misleading for a
// long-running container that's just read a lot of files.
func memUsed(m memoryStatsJSON) uint64 {
	var cache uint64
	var ok bool
	if cache, ok = m.Stats["cache"]; !ok {
		cache, ok = m.Stats["inactive_file"]
	}
	if !ok || cache > m.Usage {
		return m.Usage
	}
	return m.Usage - cache
}

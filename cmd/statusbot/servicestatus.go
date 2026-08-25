package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// serviceStatusEntry/Group/Resp mirror cmd/dashboard/servicestatus.go's
// ServiceStatusEntry/Group/Resp JSON shape. statusbot can't import
// cmd/dashboard (both are package main), so these are duplicated on
// purpose — keep in sync by hand if the dashboard's response shape changes.
type serviceStatusEntry struct {
	Name            string  `json:"name"`
	Routed          bool    `json:"routed"`
	Host            string  `json:"host,omitempty"`
	HealthyReplicas int     `json:"healthy_replicas"`
	TotalReplicas   int     `json:"total_replicas"`
	State           string  `json:"state"`
	Requests5m      *int    `json:"requests_5m"`
	RateTruncated   bool    `json:"rate_truncated"`
	CPUPercent      float64 `json:"cpu_pct"`
	MemUsedBytes    uint64  `json:"mem_used_bytes"`
	MemLimitBytes   uint64  `json:"mem_limit_bytes"`
}

type serviceStatusGroup struct {
	Group    string               `json:"group"`
	Services []serviceStatusEntry `json:"services"`
	// Machine is the peer identity (DASHBOARD_HOST) whose dashboard instance
	// this group came from — "" for the polled dashboard's own host, set on
	// groups it merged in from a peer via /peer/service-status (see
	// cmd/dashboard/peers.go's fetchPeerServiceStatus). statusbot never
	// fetches peers itself — the dashboard it polls does the merging — this
	// just carries the label through so statuspager.go can show it.
	Machine string `json:"machine,omitempty"`
}

// hostHealth mirrors cmd/dashboard/servicestatus.go's HostHealth JSON shape.
// Reuses healthTarget (health.go) for Targets — identical shape, no need to
// duplicate it a second time.
type hostHealth struct {
	Machine   string         `json:"machine"`
	Reachable bool           `json:"reachable"`
	Status    string         `json:"status,omitempty"`
	Targets   []healthTarget `json:"targets,omitempty"`
	CheckedAt string         `json:"checked_at,omitempty"`
}

type serviceStatusResp struct {
	SampledAt      time.Time            `json:"sampled_at"`
	StatsSampledAt time.Time            `json:"stats_sampled_at"`
	WindowSeconds  int                  `json:"window_seconds"`
	Groups         []serviceStatusGroup `json:"groups"`
	Hosts          []hostHealth         `json:"hosts,omitempty"`
}

// errNoDashboardToken is returned by fetchServiceStatus without attempting a
// request when DASHBOARD_API_TOKEN isn't set — a distinct, easily-matched
// error so main.go's poll loop can dedupe its "not configured" log line the
// same way it dedupes any other repeated fetch error, instead of spamming it
// every tick.
var errNoDashboardToken = fmt.Errorf("no dashboard API token available (set DASHBOARD_API_TOKEN, or wait for dashboard auto-provisioning)")

// fetchServiceStatus calls the dashboard's authenticated GET
// /api/service-status endpoint — see cmd/dashboard/api.go and
// cmd/dashboard/servicestatus.go. Unlike /api/health, this endpoint is
// gated by requireAuth, so every request needs a bearer pmt_-prefixed API
// token minted in the dashboard's UI.
func fetchServiceStatus(ctx context.Context, url, token string, client *http.Client) (serviceStatusResp, error) {
	if token == "" {
		return serviceStatusResp{}, errNoDashboardToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return serviceStatusResp{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return serviceStatusResp{}, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return serviceStatusResp{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var out serviceStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return serviceStatusResp{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

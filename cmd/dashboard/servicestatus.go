package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Server-side per-service/per-group health+usage aggregation for the
// dashboard's Status sub-tab and (later) statusbot's Discord embed.
//
// This deliberately duplicates the IDEA behind ui.go's recentForPanel/
// fillServiceStatsPanels (bucket access-log entries by backend, attribute to
// a service) — but that pair renders a different artifact (the per-service
// table inside an expanded Services card, client-side) and is left alone on
// purpose. Don't assume the two are unified; if you're touching request
// attribution, you may need to touch both.

const (
	serviceStatusWindow = 5 * time.Minute
	accessSnapshotLimit = 2000
)

type serviceRateResult struct {
	Requests5m *int
	Truncated  bool
}

// accessSnapshotResp is the subset of the proxy's GET /access response this
// needs — see cmd/proxy/accesslog.go's accessHandler for the full shape.
type accessSnapshotResp struct {
	Count   int `json:"count"`
	Entries []struct {
		Backend string `json:"backend"`
	} `json:"entries"`
}

// serviceRates buckets the proxy's recent access log by backend and sums per
// service's Backends set, over the actual serviceStatusWindow. Returns one
// result per service in svcs, keyed by name.
//
// proxyURL == "" (no PROXY_URL configured) means the whole rate path is
// absent: every service gets Requests5m == nil, which the caller must
// serialize as JSON null (not 0) — 0 reads as "confirmed idle", null reads
// as "not applicable/unknown". Services with no backends (grouped-only,
// unrouted, e.g. a DB) get the same nil treatment regardless of proxy
// availability — there is nothing to bucket for them.
func serviceRates(ctx context.Context, proxyURL string, svcs []Service) map[string]serviceRateResult {
	out := make(map[string]serviceRateResult, len(svcs))
	if proxyURL == "" {
		for _, s := range svcs {
			out[s.Name] = serviceRateResult{}
		}
		return out
	}

	since := time.Now().Add(-serviceStatusWindow).UnixMilli()
	reqURL := proxyURL + "/access?limit=" + strconv.Itoa(accessSnapshotLimit) + "&since=" + strconv.FormatInt(since, 10)
	client := http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		for _, s := range svcs {
			out[s.Name] = serviceRateResult{}
		}
		return out
	}
	resp, err := client.Do(req)
	if err != nil {
		for _, s := range svcs {
			out[s.Name] = serviceRateResult{}
		}
		return out
	}
	defer resp.Body.Close()

	var body accessSnapshotResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		for _, s := range svcs {
			out[s.Name] = serviceRateResult{}
		}
		return out
	}

	// The ring is shared across every host/service. If it saturated, we
	// cannot tell which service(s) lost entries — so every ROUTED service's
	// count is potentially under-reported, not just ones that happen to
	// still have entries in this snapshot.
	truncated := body.Count >= accessSnapshotLimit

	byBackend := map[string]int{}
	for _, e := range body.Entries {
		byBackend[e.Backend]++
	}

	for _, s := range svcs {
		if len(s.Backends) == 0 {
			out[s.Name] = serviceRateResult{}
			continue
		}
		n := 0
		for _, b := range s.Backends {
			n += byBackend[b]
		}
		out[s.Name] = serviceRateResult{Requests5m: &n, Truncated: truncated}
	}
	return out
}

type ServiceStatusEntry struct {
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
	// Machines lists every peer identity this entry's replicas were counted
	// on, sorted/deduplicated — populated by mergeServiceStatusGroups when
	// this host's own entry and a peer's matching entry (same Name+Host,
	// i.e. a Spread service — see spread.go) get folded into one. A single
	// element (or none, before any peer is configured) is the common case.
	Machines []string `json:"machines,omitempty"`
}

type ServiceStatusGroup struct {
	Group    string               `json:"group"`
	Services []ServiceStatusEntry `json:"services"`
	// Machine is the peer identity (DASHBOARD_HOST, see peers.go) whose
	// dashboard instance this group's data came from — "" for this host's
	// own groups (the common case, and the only case before any peers are
	// configured), set only on groups merged in from a peer's
	// /peer/service-status. Lets a multi-host caller (statusbot) label which
	// host each group belongs to without a second lookup.
	//
	// mergeServiceStatusGroups combines every group sharing the same Group
	// name (regardless of Machine) into one, so this ends up "" whenever
	// that group's services actually came from more than one machine —
	// which entry.Machines then says explicitly, entry by entry.
	Machine string `json:"machine,omitempty"`
}

// HealthTarget mirrors one entry of the monitor's per-target health list —
// see fetchMonitorHealth (api.go) and /api/health's response shape.
type HealthTarget struct {
	Name   string `json:"name"`
	Health string `json:"health"`
}

// HostHealth is one host's monitor-derived reachability summary, carried
// alongside ServiceStatusResp.Groups so a dead peer shows up explicitly as
// "unreachable" instead of silently contributing zero groups. Machine
// matches ServiceStatusGroup.Machine's convention — this host's own
// identity for the local entry, the peer's identity (or its raw peer URL,
// if never successfully handshaked) for a merged-in one.
type HostHealth struct {
	Machine   string         `json:"machine"`
	Reachable bool           `json:"reachable"`
	Status    string         `json:"status,omitempty"`
	Targets   []HealthTarget `json:"targets,omitempty"`
	CheckedAt string         `json:"checked_at,omitempty"`
}

type ServiceStatusResp struct {
	SampledAt      time.Time            `json:"sampled_at"`
	StatsSampledAt time.Time            `json:"stats_sampled_at"`
	WindowSeconds  int                  `json:"window_seconds"`
	Groups         []ServiceStatusGroup `json:"groups"`
	Hosts          []HostHealth         `json:"hosts,omitempty"`
}

// buildServiceStatus combines listServices, the request-rate aggregator
// above, and the docker-stats cache (dockerstats.go) into the grouped
// response shape statusbot will also consume.
//
// A single proxy.service can be backed by MANY containers sharing that
// label — not just HTTP replicas of one app (e.g. a Postgres/Supabase-style
// "DB" service is realistically 5-10 containers, none individually routed).
// CPU/mem below are summed across every one of a service's Members, except
// mem_limit_bytes: a container with no explicit memory limit reports the
// HOST'S total RAM as its limit, so summing that across members would wildly
// overstate the denominator — take the max instead.
func buildServiceStatus(ctx context.Context, dc *dockerClient, proxyURL string) (ServiceStatusResp, error) {
	svcs, err := dc.listServices(ctx)
	if err != nil {
		return ServiceStatusResp{}, err
	}
	rates := serviceRates(ctx, proxyURL, svcs)
	statsSampledAt := DockerStatsSampledAt()

	byGroup := map[string][]ServiceStatusEntry{}
	for _, s := range svcs {
		var cpu float64
		var memUsed uint64
		var memLimit uint64
		for _, m := range s.Members {
			st, ok := GetDockerStats(m.ID)
			if !ok {
				continue
			}
			cpu += st.CPUPercent
			memUsed += st.MemUsedBytes
			if st.MemLimitBytes > memLimit {
				memLimit = st.MemLimitBytes
			}
		}

		healthy := 0
		for _, m := range s.MemberSummaries {
			if m.IsCanary {
				continue
			}
			// No healthcheck defined ("") counts as healthy — most
			// containers have none, and treating that as unhealthy would
			// report 0/N for the common case.
			if m.State == "running" && m.Health != "unhealthy" && m.Health != "starting" {
				healthy++
			}
		}
		state := "up"
		switch {
		case s.AllStopped:
			state = "down"
		case healthy < s.Replicas:
			state = "degraded"
		}

		rate := rates[s.Name]
		entry := ServiceStatusEntry{
			Name:            s.Name,
			Routed:          len(s.Backends) > 0,
			Host:            s.Host,
			HealthyReplicas: healthy,
			TotalReplicas:   s.Replicas,
			State:           state,
			Requests5m:      rate.Requests5m,
			RateTruncated:   rate.Truncated,
			CPUPercent:      cpu,
			MemUsedBytes:    memUsed,
			MemLimitBytes:   memLimit,
		}
		byGroup[s.Group] = append(byGroup[s.Group], entry)
	}

	groupNames := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)

	out := ServiceStatusResp{
		SampledAt:      time.Now(),
		StatsSampledAt: statsSampledAt,
		WindowSeconds:  int(serviceStatusWindow.Seconds()),
		Groups:         make([]ServiceStatusGroup, 0, len(groupNames)),
	}
	for _, g := range groupNames {
		entries := byGroup[g]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		out.Groups = append(out.Groups, ServiceStatusGroup{Group: g, Services: entries})
	}
	return out, nil
}

// mergeServiceStatusGroups combines groups that share a Group name —
// which happens once a peer's groups are merged in (api.go) and, for a
// spread service, once spread.go forwards proxy.group so the origin and its
// peer replicas agree on the same group name — into one group. Within that
// combined group, entries that are really the same routed service running on
// multiple hosts (matched on Name+Host, Host required non-empty: an unrouted
// "DB"-style entry describes containers this host runs, never a shared
// identity a peer could also be running) are folded into one, summing
// replica/usage counts and recording every contributing machine so a caller
// can render e.g. "5/5 replicas (pi + mac)" instead of two disconnected
// entries.
//
// Safe to call unconditionally, including a single-host response with no
// peers configured: every group name is already unique in that case, so
// nothing merges and the input comes back unchanged (aside from sort order).
func mergeServiceStatusGroups(groups []ServiceStatusGroup) []ServiceStatusGroup {
	order := make([]string, 0, len(groups))
	byName := map[string]*ServiceStatusGroup{}
	machinesOf := map[string]map[string]bool{}

	for _, g := range groups {
		mg, ok := byName[g.Group]
		if !ok {
			gc := ServiceStatusGroup{Group: g.Group}
			byName[g.Group] = &gc
			machinesOf[g.Group] = map[string]bool{}
			order = append(order, g.Group)
			mg = &gc
		}
		if g.Machine != "" {
			machinesOf[g.Group][g.Machine] = true
		}
		for _, e := range g.Services {
			if g.Machine != "" {
				e.Machines = appendUniqueSorted(e.Machines, g.Machine)
			}
			if e.Host == "" {
				mg.Services = append(mg.Services, e)
				continue
			}
			idx := -1
			for i := range mg.Services {
				if mg.Services[i].Name == e.Name && mg.Services[i].Host == e.Host {
					idx = i
					break
				}
			}
			if idx == -1 {
				mg.Services = append(mg.Services, e)
				continue
			}
			mg.Services[idx] = combineServiceStatusEntries(mg.Services[idx], e)
		}
	}

	sort.Strings(order)
	out := make([]ServiceStatusGroup, 0, len(order))
	for _, name := range order {
		g := byName[name]
		if ms := machinesOf[name]; len(ms) == 1 {
			for m := range ms {
				g.Machine = m
			}
		}
		sort.Slice(g.Services, func(i, j int) bool { return g.Services[i].Name < g.Services[j].Name })
		out = append(out, *g)
	}
	return out
}

// combineServiceStatusEntries folds b (a same-identity entry counted on
// another machine) into a.
func combineServiceStatusEntries(a, b ServiceStatusEntry) ServiceStatusEntry {
	a.HealthyReplicas += b.HealthyReplicas
	a.TotalReplicas += b.TotalReplicas
	a.CPUPercent += b.CPUPercent
	a.MemUsedBytes += b.MemUsedBytes
	if b.MemLimitBytes > a.MemLimitBytes {
		a.MemLimitBytes = b.MemLimitBytes
	}
	// down only when every contributing host agrees it's down — one host
	// still serving traffic is a real degradation, not a full outage.
	switch {
	case a.State == "up" && b.State == "up":
		a.State = "up"
	case a.State == "down" && b.State == "down":
		a.State = "down"
	default:
		a.State = "degraded"
	}
	switch {
	case a.Requests5m == nil:
		a.Requests5m = b.Requests5m
	case b.Requests5m != nil:
		sum := *a.Requests5m + *b.Requests5m
		a.Requests5m = &sum
	}
	a.RateTruncated = a.RateTruncated || b.RateTruncated
	for _, m := range b.Machines {
		a.Machines = appendUniqueSorted(a.Machines, m)
	}
	return a
}

func appendUniqueSorted(machines []string, m string) []string {
	for _, existing := range machines {
		if existing == m {
			return machines
		}
	}
	machines = append(machines, m)
	sort.Strings(machines)
	return machines
}

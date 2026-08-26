package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
	"github.com/redis/go-redis/v9"
)

// PeerRegistry discovers and authenticates with sibling dashboard instances
// running on other hosts (e.g. Pi + Mac mini over Tailscale). Modeled on
// cmd/proxy/peers.go's PeerRegistry, but for this phase it only proves
// connectivity + auth — no service data crosses the wire yet.
//
// Every `interval`, each dashboard POSTs a placeholder handshake to every
// configured peer's /peer/handshake endpoint on the dedicated peer-handshake
// port, authenticated with a shared bearer secret, and records
// success/failure + last-contact time per peer. A peer being unreachable is
// expected during restarts/network blips, not an error condition.

// dashboardVersionsRedisKey is the shared hash storing each dashboard host's
// own ratcheted last-stable build version, keyed by identity. Matches the
// existing "pmgr:" prefix convention (see auth.go's authRedisKey).
const dashboardVersionsRedisKey = "pmgr:dashboard:versions"

// peerStatus is the last-known bookkeeping for one configured peer.
type peerStatus struct {
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success"`
	OK          bool      `json:"ok"`
	Identity    string    `json:"identity,omitempty"`
	Version     string    `json:"version,omitempty"`
	// Writes reports the peer's own -peer-writes setting, as of its last
	// successful handshake — advisory display only (ui.go's peerWritable),
	// never trusted for authorization. The peer's own /peer/images/*
	// handlers re-check their own writesEnabled regardless of what this
	// says.
	Writes bool `json:"writes,omitempty"`
}

type PeerRegistry struct {
	peers    []string // full URLs, e.g. http://100.83.62.68:8098
	secret   string
	identity string
	version  string // this host's own buildVersion
	interval time.Duration
	client   *http.Client
	rdb      *redis.Client // nil disables all redis-backed behavior

	mu             sync.Mutex
	status         map[string]peerStatus
	meshFloor      int
	meshFloorKnown bool
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func newPeerRegistry(peers []string, secret, identity, version string, interval time.Duration, rdb *redis.Client) *PeerRegistry {
	return &PeerRegistry{
		peers:    peers,
		secret:   secret,
		identity: identity,
		version:  version,
		interval: interval,
		rdb:      rdb,
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        len(peers) * 2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		status: map[string]peerStatus{},
	}
}

// Identity returns this host's own peer identity.
func (p *PeerRegistry) Identity() string { return p.identity }

// Peers returns a copy of the configured peer-handshake base URLs — used by
// fetchPeerServiceStatus to know which peers' /peer/service-status to poll.
func (p *PeerRegistry) Peers() []string { return append([]string(nil), p.peers...) }

func (p *PeerRegistry) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *PeerRegistry) tick(ctx context.Context) {
	p.refreshMeshFloor(ctx)
	for _, peer := range p.peers {
		go p.send(ctx, peer)
	}
}

// send POSTs a placeholder handshake payload to peer's /peer/handshake and
// records the outcome, including the peer's own identity/version from the
// response body.
func (p *PeerRegistry) send(ctx context.Context, peer string) {
	url := strings.TrimRight(peer, "/") + "/peer/handshake"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		// Peer unreachable — expected during restarts / network blips. Silent.
		p.recordResult(peer, false, "", "", false)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Auth failure or malformed request: worth surfacing so misconfig is visible.
		log.Printf("dashboard peer handshake: %s → %s", url, resp.Status)
		p.recordResult(peer, false, "", "", false)
		return
	}
	var body struct {
		Peer    string `json:"peer"`
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Writes  bool   `json:"writes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	p.recordResult(peer, true, body.Peer, body.Version, body.Writes)
}

// recordResult only overwrites Identity/Version/Writes on a successful
// handshake — a transient failure preserves the last-known values instead of
// blanking them. Identity/Version are additionally guarded on non-empty
// (their own zero value means "unknown", not "changed to empty"); Writes has
// no such ambiguity — false is a meaningful, intentional value — so it's
// simply copied whenever ok.
func (p *PeerRegistry) recordResult(peer string, ok bool, identity, version string, writes bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status[peer]
	st.LastAttempt = time.Now()
	if ok {
		st.LastSuccess = time.Now()
	}
	st.OK = ok
	if ok && version != "" {
		st.Version = version
	}
	if ok && identity != "" {
		st.Identity = identity
	}
	if ok {
		st.Writes = writes
	}
	p.status[peer] = st
}

// Status returns a copy of the current per-peer bookkeeping.
func (p *PeerRegistry) Status() map[string]peerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]peerStatus, len(p.status))
	for k, v := range p.status {
		out[k] = v
	}
	return out
}

// URLForIdentity scans the current per-peer bookkeeping for a peer whose
// last-known handshake identity matches identity, returning its base URL.
// Not wired into anything yet — colocated here for a later phase that needs
// to resolve a peer's URL from a Machine tag.
func (p *PeerRegistry) URLForIdentity(identity string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for url, st := range p.status {
		if st.Identity == identity {
			return url, true
		}
	}
	return "", false
}

// MeshFloor returns the minimum ratcheted version currently known across the
// mesh, and whether it's known yet.
func (p *PeerRegistry) MeshFloor() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meshFloor, p.meshFloorKnown
}

// ratchetOwnVersion writes this host's own buildVersion into the shared
// Redis hash, never decreasing it — even across a restart to an older build.
//
// Only this process ever writes this specific hash field (its own identity)
// — a plain read-then-conditionally-write has no concurrent-writer race, no
// WATCH/transaction needed. Note: if DASHBOARD_HOST/hostname are both unset
// on two peer instances, both fall back to the literal "dashboard" identity
// and would race on this same field — pre-existing ambiguity, not new to
// this write path.
func (p *PeerRegistry) ratchetOwnVersion(ctx context.Context) {
	if p.rdb == nil || p.version == "dev" {
		return
	}
	v, err := strconv.Atoi(p.version)
	if err != nil {
		log.Printf("dashboard peers: buildVersion %q is not numeric, skipping ratchet: %v", p.version, err)
		return
	}
	cur, err := p.rdb.HGet(ctx, dashboardVersionsRedisKey, p.identity).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("dashboard peers: redis read for version ratchet failed: %v", err)
		return
	}
	if err == nil && cur >= v {
		return
	}
	if err := p.rdb.HSet(ctx, dashboardVersionsRedisKey, p.identity, v).Err(); err != nil {
		log.Printf("dashboard peers: redis write for version ratchet failed: %v", err)
	}
}

// parseVersions extracts the parseable-int subset of a raw
// identity→version-string map (as HGetAll returns), dropping "dev" and
// any garbage entry.
func parseVersions(raw map[string]string) []int {
	var out []int
	for _, s := range raw {
		if v, err := strconv.Atoi(s); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// meshFloorFrom returns the minimum of vals, or (0, false) when empty.
func meshFloorFrom(vals []int) (int, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	min := vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
	}
	return min, true
}

// refreshMeshFloor recomputes the mesh floor from Redis's current per-host
// version hash — the minimum across all currently-known hosts.
func (p *PeerRegistry) refreshMeshFloor(ctx context.Context) {
	if p.rdb == nil {
		return
	}
	raw, err := p.rdb.HGetAll(ctx, dashboardVersionsRedisKey).Result()
	if err != nil {
		log.Printf("dashboard peers: redis read for mesh floor failed: %v", err)
		return
	}
	floor, ok := meshFloorFrom(parseVersions(raw))
	if !ok {
		return
	}
	p.mu.Lock()
	p.meshFloor = floor
	p.meshFloorKnown = true
	p.mu.Unlock()
}

// peerHandshakeHandler returns the HTTP handler for POST /peer/handshake on
// the dedicated peer-handshake port. Bearer-authenticated with the shared
// secret, constant-time compare — same approach as cmd/proxy/peers.go's
// peerHandshakeHandler. Empty secret disables the endpoint entirely (404) so
// an unconfigured dashboard can't accept handshakes. A valid request gets a
// minimal ok:true body with this host's identity/version/writesEnabled —
// writesEnabled lets a peer's UI show write controls only where the target
// actually accepts them (peerStatus.Writes / ui.go's peerWritable);
// purely advisory, the /peer/images/* handlers re-check it themselves.
func peerHandshakeHandler(secret, identity, version string, writesEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"peer": identity, "ok": true, "version": version, "writes": writesEnabled})
	})
}

// peerServiceStatusResp is the wire shape for GET /peer/service-status —
// this peer's own local service-status (never further merged, so a mesh
// can't loop) plus its identity, since the fetcher needs both to label the
// groups it merges in.
type peerServiceStatusResp struct {
	Identity string            `json:"identity"`
	Status   ServiceStatusResp `json:"status"`
	// Health is this peer's own monitor-derived reachability summary —
	// Machine/Reachable are left zero here (same convention as Machine on
	// Status.Groups) and filled in by the caller (fetchPeerServiceStatus)
	// after decoding.
	Health HostHealth `json:"health"`
}

// peerServiceStatusHandler returns the HTTP handler for GET
// /peer/service-status on the dedicated peer-handshake port — same
// bearer-auth shape as peerHandshakeHandler. Always returns THIS host's own
// local status only (never re-merges groups already tagged with another
// peer's Machine) — the caller (api.go's /api/service-status) is the one
// that fans out to every configured peer, so a peer answering this request
// never needs to know about the mesh beyond itself.
func peerServiceStatusHandler(secret, identity string, dc *dockerClient, proxyURL, monitorURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		status, err := buildServiceStatus(r.Context(), dc, proxyURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		healthStatus, healthTargets := fetchMonitorHealth(monitorURL)
		health := HostHealth{Status: healthStatus, Targets: healthTargets, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerServiceStatusResp{Identity: identity, Status: status, Health: health})
	})
}

// peerGET performs one bearer-authenticated GET against peerBaseURL+path,
// bounded by a 2s timeout, and decodes the JSON response body into out. The
// shared plumbing behind every /peer/<resource> fetcher (fetchPeerServiceStatus,
// fetchPeerServices, …) — callers only differ in the response shape and what
// they do with a failure.
func peerGET(ctx context.Context, client *http.Client, peerBaseURL, secret, path string, out any) error {
	url := strings.TrimRight(peerBaseURL, "/") + path
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s → %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s bad response: %w", url, err)
	}
	return nil
}

// fetchPeerServiceStatus polls every configured peer's /peer/service-status
// in parallel (bounded by a short per-peer timeout, so one unreachable peer
// can't stall /api/service-status past a fraction of a second) and returns
// their groups, each tagged with that peer's own identity as Machine, plus
// one HostHealth per configured peer — including a Reachable:false entry for
// a peer that's down, slow, or misconfigured, so it shows up as explicitly
// unreachable in the caller's response instead of silently contributing zero
// groups. Groups remain advisory display only (a failed peer contributes
// none), but the host roster is not: every configured peer gets exactly one
// entry, success or failure.
func fetchPeerServiceStatus(ctx context.Context, registry *PeerRegistry, secret string) ([]ServiceStatusGroup, []HostHealth) {
	peers := registry.Peers()
	if len(peers) == 0 || secret == "" {
		return nil, nil
	}
	// Snapshot of last-known handshake identity per peer URL, used only as
	// the failure-path label fallback below — a peer that's never
	// successfully handshaked has no better name than its raw configured URL.
	statuses := registry.Status()
	client := &http.Client{Timeout: 2 * time.Second}
	type result struct {
		groups []ServiceStatusGroup
		host   HostHealth
	}
	results := make(chan result, len(peers))
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			var body peerServiceStatusResp
			if err := peerGET(ctx, client, peer, secret, "/peer/service-status", &body); err != nil {
				log.Printf("dashboard peer service-status: %v", err)
				machine := statuses[peer].Identity
				if machine == "" {
					machine = peer // fallback label so a peer with no prior successful handshake still shows up as *something*
				}
				results <- result{host: HostHealth{Machine: machine, Reachable: false}}
				return
			}
			machine := body.Identity
			if machine == "" {
				machine = peer // fallback label so a peer that forgot to set DASHBOARD_HOST still shows up as *something*
			}
			groups := body.Status.Groups
			for i := range groups {
				groups[i].Machine = machine
			}
			health := body.Health
			health.Machine = machine
			health.Reachable = true
			results <- result{groups: groups, host: health}
		}(peer)
	}
	wg.Wait()
	close(results)
	var groups []ServiceStatusGroup
	var hosts []HostHealth
	for r := range results {
		groups = append(groups, r.groups...)
		hosts = append(hosts, r.host)
	}
	return groups, hosts
}

// peerServicesResp is the wire shape for GET /peer/services — this peer's
// own local managed services (self already excluded by buildManagedServices)
// plus its identity, since the fetcher needs both to label the services it
// merges in.
type peerServicesResp struct {
	Identity string    `json:"identity"`
	Services []Service `json:"services"`
}

// peerServicesHandler returns the HTTP handler for GET /peer/services on the
// dedicated peer-handshake port — same bearer-auth shape as
// peerServiceStatusHandler. Always returns THIS host's own managed services
// (self-excluded via buildManagedServices) — the caller (api.go's
// /api/services) is the one that fans out to every configured peer.
func peerServicesHandler(secret, identity string, dc *dockerClient, onb *OnboardedStore, ic *imageChecker, blocks *autoUpdateBlockStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		svcs, err := buildManagedServices(r.Context(), dc, onb, ic, blocks)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerServicesResp{Identity: identity, Services: svcs})
	})
}

// peerServicesMutateHandler returns the HTTP handler for POST
// /peer/services/{name}/{scale,stop,start,replicas/{member}/{stop,start},
// autoupdate,singleton,check} on the dedicated peer-handshake port — the write-side
// counterpart of peerServicesHandler. This establishes the copyable pattern
// later write-mesh phases (4-5) should reuse: gate on secret+writesEnabled,
// constant-time bearer compare, parse the subpath, self-guard, then dispatch
// against THIS host's own live Docker state (never the requester's claims),
// auditing every branch as peer-mesh.
func peerServicesMutateHandler(secret, identity string, dc *dockerClient, onb *OnboardedStore, ic *imageChecker, registry *PeerRegistry, routesConfigPath, proxyURL string, writesEnabled bool, rom *rollingOpManager) http.Handler {
	if rom == nil {
		rom = newRollingOpManager(dc)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || !writesEnabled {
			http.NotFound(w, r)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sub := strings.TrimPrefix(r.URL.Path, "/peer/services/")
		parts := strings.SplitN(sub, "/", 2)
		name := parts[0]
		if name == "" {
			http.NotFound(w, r)
			return
		}
		// Fails OPEN on a Docker error (err == nil && self): every branch
		// below still requires a subsequent successful live-state read
		// (findService / MemberSummaries lookup) before it can act, so an
		// error here can't be exploited into a blind mutation. A FUTURE
		// phase (3-5) adding a peer-write branch that can mutate WITHOUT
		// first re-reading live state must fail CLOSED instead of copying
		// this assumption blindly.
		//
		// The autoupdate branch (phase 3) is exactly that case: enabling
		// auto-update writes straight to the onboarded store with no
		// live-state read afterward, so it cannot rely on this outer check.
		// runServiceAutoUpdateSet does its own independent self-check and
		// fails CLOSED (403) on a Docker error there too, not just on self==true.
		if self, err := dc.serviceContainsSelfByName(r.Context(), name); err == nil && self {
			http.Error(w, "refusing to manage the dashboard's own service from within itself — use docker compose on the host", http.StatusForbidden)
			return
		}
		if len(parts) == 2 && parts[1] == "scale" && r.Method == http.MethodPost {
			var body struct{ Replicas int }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := runServiceScale(r.Context(), dc, onb, routesConfigPath, proxyURL, name, body.Replicas); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(r, "peer-mesh", "service.scale", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "scaled", "replicas": body.Replicas})
			return
		}
		if len(parts) == 2 && (parts[1] == "stop" || parts[1] == "start") && r.Method == http.MethodPost {
			svc, ok, err := findService(r.Context(), dc, name)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if !ok {
				http.Error(w, "service not found", http.StatusNotFound)
				return
			}
			act := parts[1]
			acted, err := runServiceLifecycle(r.Context(), dc, proxyURL, svc, act)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(r, "peer-mesh", "service."+act, name)
			msg := act + "ped"
			if acted == 0 {
				msg = "already-" + act + "ped"
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": msg, "members_acted": acted})
			return
		}
		if len(parts) == 2 && strings.HasPrefix(parts[1], "replicas/") && r.Method == http.MethodPost {
			sub := strings.TrimPrefix(parts[1], "replicas/")
			memberParts := strings.SplitN(sub, "/", 2)
			if len(memberParts) != 2 || (memberParts[1] != "stop" && memberParts[1] != "start") {
				http.NotFound(w, r)
				return
			}
			member, act := memberParts[0], memberParts[1]
			svc, ok, err := findService(r.Context(), dc, name)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if !ok {
				http.Error(w, "service not found", http.StatusNotFound)
				return
			}
			var targetID string
			var targetIsCanary bool
			for _, m := range svc.MemberSummaries {
				if m.Name == member {
					targetID = m.ID
					targetIsCanary = m.IsCanary
					break
				}
			}
			if targetID == "" {
				http.Error(w, "replica not found", http.StatusNotFound)
				return
			}
			if targetIsCanary {
				http.Error(w, "canary replicas can't be stopped here — use Discard or Promote", http.StatusConflict)
				return
			}
			if act == "stop" {
				err = dc.stopContainer(r.Context(), targetID)
			} else {
				err = dc.startContainer(r.Context(), targetID)
			}
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			proxyRefresh(proxyURL)
			audit(r, "peer-mesh", "service.replica_"+act, name+"/"+member)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": act + "ped", "member": member})
			return
		}
		if len(parts) == 2 && parts[1] == "autoupdate" && r.Method == http.MethodPost {
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := runServiceAutoUpdateSet(r.Context(), dc, onb, name, body.Enabled); err != nil {
				writeAutoUpdateErr(w, err)
				return
			}
			audit(r, "peer-mesh", "service.autoupdate_set", name+" => "+strconv.FormatBool(body.Enabled))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": body.Enabled})
			return
		}
		if len(parts) == 2 && parts[1] == "singleton" && r.Method == http.MethodPost {
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if _, ok := onb.Get(name); ok {
				http.Error(w, "singleton toggle is only supported for label-managed services", http.StatusBadRequest)
				return
			}
			if err := dc.setUnscalableLabel(r.Context(), name, body.Enabled); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(r, "peer-mesh", "service.singleton_set", name+" => "+strconv.FormatBool(body.Enabled))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": body.Enabled})
			return
		}
		if len(parts) == 2 && parts[1] == "weight" && r.Method == http.MethodPost {
			var body struct {
				Weight int `json:"weight"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if !validWeight(body.Weight) {
				http.Error(w, fmt.Sprintf("weight must be between 1 and %d", maxServiceWeight), http.StatusBadRequest)
				return
			}
			if _, ok := onb.Get(name); ok {
				http.Error(w, "weight is only supported for label-managed services", http.StatusBadRequest)
				return
			}
			if err := dc.setWeightLabel(r.Context(), name, body.Weight); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(r, "peer-mesh", "service.weight_set", name+" => "+strconv.Itoa(body.Weight))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "weight": body.Weight})
			return
		}
		if len(parts) == 2 && parts[1] == "check" && r.Method == http.MethodPost {
			payload, status, err := runServiceCheckImage(r.Context(), dc, ic, onb, name)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if status != http.StatusOK {
				if status == http.StatusNotFound {
					http.Error(w, "service not found", http.StatusNotFound)
					return
				}
				msg, _ := payload.(string)
				if msg == "" {
					msg = "check failed"
				}
				http.Error(w, msg, status)
				return
			}
			// Unlike the local handler (which audits unconditionally once the
			// service is found, including the eventual-502 path), this only
			// audits on success — there's no operator watching this response
			// synchronously the way the local UI is, so a failed check isn't
			// an action worth recording in the peer-mesh audit trail.
			audit(r, "peer-mesh", "service.check_image", name)
			httpx.WriteJSON(w, status, payload)
			return
		}
		if len(parts) == 2 && parts[1] == "replace" && r.Method == http.MethodPost {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// No additional fail-closed self-check beyond the outer guard
			// above (unlike Phase 3's autoupdate case): the residual risk on
			// the onboarded path here is route-mutation on the dashboard's
			// own onboarded entry (a traffic split), not container
			// destruction — a materially smaller blast radius that doesn't
			// warrant its own guard.
			if _, ok := onb.Get(name); ok {
				if err := dc.replaceOnboarded(r.Context(), name, body, onb, routesConfigPath); err != nil {
					writeServiceErr(w, err)
					return
				}
				proxyRefresh(proxyURL)
			} else if err := dc.replaceService(r.Context(), name, body); err != nil {
				writeServiceErr(w, err)
				return
			}
			audit(r, "peer-mesh", "service.replace", name+" => "+body.Image)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "replaced", "image": body.Image})
			return
		}
		// rolling-replace, unlike replace above, is label-managed-only (see
		// rollingop.go/api.go's doc comments) — an onboarded name here is
		// rejected rather than silently falling back to replaceOnboarded.
		if len(parts) == 2 && parts[1] == "rolling-replace" && r.Method == http.MethodPost {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if body.Image == "" {
				http.Error(w, "image is required", http.StatusBadRequest)
				return
			}
			if _, ok := onb.Get(name); ok {
				http.Error(w, fmt.Sprintf("%q is an onboarded service — rolling replace only supports label-managed services", name), http.StatusBadRequest)
				return
			}
			st, err := rom.start(name, body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			audit(r, "peer-mesh", "service.rolling_replace_start", name+" => "+body.Image)
			httpx.WriteJSON(w, http.StatusAccepted, st)
			return
		}
		if len(parts) == 2 && parts[1] == "rolling-replace" && r.Method == http.MethodGet {
			st, ok := rom.get(name)
			if !ok {
				http.Error(w, "no active rolling replace for "+name, http.StatusNotFound)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, st)
			return
		}
		if len(parts) == 2 && parts[1] == "stage" && r.Method == http.MethodPost {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// Same reasoning as replace above: route-mutation risk only, so
			// the outer self-guard is judged sufficient here too.
			if _, ok := onb.Get(name); ok {
				if err := dc.stageOnboarded(r.Context(), name, body, onb, routesConfigPath); err != nil {
					writeServiceErr(w, err)
					return
				}
				proxyRefresh(proxyURL)
			} else if err := dc.stageCanary(r.Context(), name, body); err != nil {
				writeServiceErr(w, err)
				return
			}
			audit(r, "peer-mesh", "service.stage", name+" => "+body.Image)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "staged"})
			return
		}
		if len(parts) == 2 && parts[1] == "promote" && r.Method == http.MethodPost {
			if _, ok := onb.Get(name); ok {
				if err := dc.promoteOnboarded(r.Context(), name, onb, routesConfigPath); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURL)
			} else if err := dc.promoteCanary(r.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(r, "peer-mesh", "service.promote", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "promoted"})
			return
		}
		if len(parts) == 2 && parts[1] == "canary" && r.Method == http.MethodDelete {
			if _, ok := onb.Get(name); ok {
				if err := dc.discardOnboarded(r.Context(), name, onb, routesConfigPath); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURL)
			} else if err := dc.discardCanary(r.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(r, "peer-mesh", "service.discard_canary", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
			return
		}
		if len(parts) == 2 && parts[1] == "offboard" && r.Method == http.MethodPost {
			_, wasOnboarded := onb.Get(name)
			if err := runServiceOffboard(r.Context(), dc, onb, routesConfigPath, name); err != nil {
				writeServiceRemovalErr(w, err, !wasOnboarded)
				return
			}
			proxyRefresh(proxyURL)
			audit(r, "peer-mesh", "service.offboard", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "offboarded"})
			return
		}
		if len(parts) == 2 && parts[1] == "duplicate" && r.Method == http.MethodPost {
			var body DuplicateServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// The inbound assertion (if any) is relayed as-is to the second
			// hop's peerMutate call inside runServiceDuplicate — forwardedActorTTL
			// (2 minutes) comfortably covers this synchronous two-hop chain, and
			// relaying the original signed claims keeps audit attribution on the
			// real user across both hops rather than collapsing to "peer-mesh".
			resp, err := runServiceDuplicate(r.Context(), dc, registry, onb, routesConfigPath, name, body, r.Header.Get(actorHeader))
			if err != nil {
				var pe *peerDuplicateError
				switch {
				case errors.As(err, &pe):
					mapPeerMutationErr(w, pe.statusCode, pe.body)
				case errors.Is(err, errDuplicateNotFound):
					http.Error(w, "service not found", http.StatusNotFound)
				default:
					http.Error(w, err.Error(), http.StatusBadRequest)
				}
				return
			}
			audit(r, "peer-mesh", "service.duplicate", name+" => "+body.Target)
			httpx.WriteJSON(w, http.StatusOK, resp)
			return
		}
		if len(parts) == 2 && parts[1] == "spread" && r.Method == http.MethodPost {
			var body SpreadServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// Inbound assertion relayed as-is to the second hop, same
			// reasoning as the duplicate branch above.
			resp, err := runServiceSpread(r.Context(), dc, registry, onb, name, body, r.Header.Get(actorHeader))
			if err != nil {
				var pe *peerSpreadError
				switch {
				case errors.As(err, &pe):
					mapPeerMutationErr(w, pe.statusCode, pe.body)
				case errors.Is(err, errSpreadNotFound):
					http.Error(w, "service not found", http.StatusNotFound)
				default:
					http.Error(w, err.Error(), http.StatusBadRequest)
				}
				return
			}
			audit(r, "peer-mesh", "service.spread", name+" => "+body.Target)
			httpx.WriteJSON(w, http.StatusOK, resp)
			return
		}
		if len(parts) == 1 && r.Method == http.MethodDelete {
			// Unlike the local handler, confirmation is REQUIRED here, not
			// optional — this branch is only ever reached via
			// forwardServiceMutation, which always sends {"confirm": name}
			// constructed server-side. A missing/mismatched confirm this far
			// in means something upstream is malformed or spoofed, so it's
			// rejected before any Docker mutation regardless of cause.
			var body struct {
				Confirm string `json:"confirm"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if body.Confirm == "" || body.Confirm != name {
				http.Error(w, "confirmation does not match service name", http.StatusBadRequest)
				return
			}
			_, wasOnboarded := onb.Get(name)
			membersActed, err := runServiceDelete(r.Context(), dc, onb, routesConfigPath, name)
			if err != nil {
				if wasOnboarded {
					writeServiceRemovalErr(w, err, false)
				} else {
					writeServiceDeleteErr(w, membersActed, err)
				}
				return
			}
			if wasOnboarded {
				proxyRefresh(proxyURL)
				audit(r, "peer-mesh", "service.offboard", name)
				httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "offboarded"})
				return
			}
			audit(r, "peer-mesh", "service.delete", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "deleted", "members_acted": membersActed})
			return
		}
		http.NotFound(w, r)
	})
}

// peerDiscoveryMutateHandler returns the HTTP handler for POST
// /peer/discovery/{name}/onboard on the dedicated peer-handshake port — the
// write-side onboard counterpart, kept out of peerServicesMutateHandler's
// dispatch on purpose. Onboard targets are UNLABELED containers (they never
// carry proxy.service/etc labels), so the label-based outer self-guard used
// by peerServicesMutateHandler (dc.serviceContainsSelfByName) literally
// cannot fire against them — it would always return false ("not self") for
// any onboard target, giving zero protection. The actual guard is
// checkOnboardTarget, IDENTITY-based (compares the container's Docker ID
// against selfHostname()) plus infra-name-based, and it already lives inside
// onboardContainer itself, called right after the target container is
// resolved and before any mutation. It fails CLOSED on a selfHostname()
// lookup error. So: no outer self-guard is added here — checkOnboardTarget is
// the complete, correct, already-fail-closed guard for this handler.
func peerDiscoveryMutateHandler(secret, identity string, dc *dockerClient, proxyURL string, writesEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || !writesEnabled {
			http.NotFound(w, r)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sub := strings.TrimPrefix(r.URL.Path, "/peer/discovery/")
		parts := strings.SplitN(sub, "/", 2)
		if len(parts) != 2 || parts[1] != "onboard" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		name := parts[0]
		if name == "" {
			http.NotFound(w, r)
			return
		}
		var body OnboardRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if err := dc.onboardContainer(r.Context(), name, body); err != nil {
			writeOnboardErr(w, err)
			return
		}
		proxyRefresh(proxyURL)
		audit(r, "peer-mesh", "service.onboard", name)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "onboarded", "name": name})
	})
}

// peerImagesResp is the wire shape for GET /peer/images — this peer's own
// local per-service image inventory (buildImagesInfo) plus its identity.
// Machine is left unset on Images — the peer never tags its own local data
// (same convention as peerServicesResp/peerServiceStatusResp); the caller
// tags it after decoding.
type peerImagesResp struct {
	Identity string          `json:"identity"`
	Images   *imagesInfoResp `json:"images"`
}

// peerImagesHandler returns the HTTP handler for GET /peer/images on the
// dedicated peer-handshake port — same bearer-auth shape as
// peerServicesHandler. Always returns THIS host's own local image inventory
// — mirrors what the LOCAL /api/images handler does today (api.go), calling
// dc.listServices directly rather than buildManagedServices, so this is NOT
// self-excluding. The caller (api.go's /api/images host-forwarding branch)
// is the one that resolves which peer to ask.
func peerImagesHandler(secret, identity string, dc *dockerClient, rs *ReleasesStore, ih *ImageHistoryStore, onb *OnboardedStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		svcs, err := dc.listServices(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info, err := buildImagesInfo(r.Context(), dc, rs, ih, svcs, onb.List())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerImagesResp{Identity: identity, Images: info})
	})
}

// peerImagesMutateHandler returns the HTTP handler for POST/DELETE
// /peer/images/{mark,delete,prune} on the dedicated peer-handshake port —
// the write-side counterpart of peerImagesHandler. Gated on writesEnabled in
// addition to the shared secret: a peer with writes not yet enabled answers
// 404 here, same as an unconfigured secret does on every other /peer/*
// route — mapPeerMutationErr's doc comment already covers why that
// ambiguity is intentional, not disambiguated here.
//
// mark/unmark identify the target the same way the local /api/images/
// handler's body does (service, tag) — no change needed, mark/unmark never
// touched DeleteToken. delete identifies by (service, ref) instead of a
// DeleteToken: ref is imageEntry.Ref, already unstripped on the read path
// (for a tagged image it's the exact same string DeleteToken would be), so
// nothing new is disclosed — the peer still independently resolves its own
// DeleteToken from a fresh buildImagesInfo rather than trusting a
// client-supplied one. Every branch recomputes protection LIVE from this
// host's own Docker state before mutating anything, mirroring the local
// handler's exact safety checks (api.go) — but against the peer's own
// dc/rs/ih/onb, never the caller's claims.
func peerImagesMutateHandler(secret, identity string, dc *dockerClient, rs *ReleasesStore, ih *ImageHistoryStore, onb *OnboardedStore, writesEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || !writesEnabled {
			http.NotFound(w, r)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sub := strings.TrimPrefix(r.URL.Path, "/peer/images/")
		switch {
		case sub == "mark" && r.Method == http.MethodPost:
			var body struct{ Service, Tag, Label string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			base, ok := resolveImageBase(r.Context(), dc, onb, body.Service)
			if !ok {
				http.Error(w, "unknown service", http.StatusNotFound)
				return
			}
			if err := rs.Mark(base, body.Tag, body.Label, auditUser(r, "peer-mesh")); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(r, "peer-mesh", "image.mark", body.Service+":"+body.Tag)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "marked", "tag": body.Tag})
		case sub == "mark" && r.Method == http.MethodDelete:
			var body struct{ Service, Tag string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			base, ok := resolveImageBase(r.Context(), dc, onb, body.Service)
			if !ok {
				http.Error(w, "unknown service", http.StatusNotFound)
				return
			}
			if err := rs.Unmark(base, body.Tag); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(r, "peer-mesh", "image.unmark", body.Service+":"+body.Tag)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "unmarked", "tag": body.Tag})
		case sub == "delete" && r.Method == http.MethodDelete:
			var body struct{ Service, Ref string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if body.Service == "" || body.Ref == "" {
				http.Error(w, "service and ref required", http.StatusBadRequest)
				return
			}
			svcs, err := dc.listServices(r.Context())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// SAFETY: recompute this peer's own image inventory + protection
			// fresh, right here — never trust the requester's claim that ref
			// is safe to delete.
			info, err := buildImagesInfo(r.Context(), dc, rs, ih, svcs, onb.List())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			var target *imageEntry
			for i := range info.Services {
				if info.Services[i].Service != body.Service {
					continue
				}
				for j := range info.Services[i].Entries {
					if info.Services[i].Entries[j].Ref == body.Ref {
						target = &info.Services[i].Entries[j]
					}
				}
			}
			if target == nil || !target.OnDisk {
				http.Error(w, "unknown image", http.StatusNotFound)
				return
			}
			// target.Protected (protectedRefs[Ref] || protectedIDs[fullID])
			// already subsumes both of the local handler's checks — direct
			// token protection AND "in use under another tag" (an ID shared
			// across tags is protected under either tag's Ref) — so there's
			// no separate listImages loop to replicate here.
			if target.Protected {
				httpx.WriteJSON(w, http.StatusConflict, map[string]string{
					"error": "image is protected (in use, current, or marked stable) — not deleted",
				})
				return
			}
			if err := dc.removeImage(r.Context(), target.DeleteToken, false); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(r, "peer-mesh", "image.delete", body.Service+":"+body.Ref)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted", "ref": body.Ref})
		case sub == "prune" && r.Method == http.MethodPost:
			var body struct {
				Service string `json:"service"`
				KeepN   int    `json:"keep_n"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			svcs, err := dc.listServices(r.Context())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			info, err := buildImagesInfo(r.Context(), dc, rs, ih, svcs, onb.List())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			deleted, failed, reclaimed := runImagePrune(r.Context(), dc, info, body.Service, body.KeepN, func(token string) {
				audit(r, "peer-mesh", "image.delete", token)
			})
			target := "all"
			if body.Service != "" {
				target = body.Service
			}
			audit(r, "peer-mesh", "image.prune",
				target+" keep_n="+strconv.Itoa(body.KeepN)+" deleted="+strconv.Itoa(len(deleted))+
					" failed="+strconv.Itoa(len(failed))+" reclaimed_bytes="+strconv.FormatInt(reclaimed, 10))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"deleted": deleted, "failed": failed, "reclaimed_bytes": reclaimed,
			})
		default:
			http.NotFound(w, r)
		}
	})
}

// peerLogsContainersResp is the wire shape for GET /peer/logs/containers —
// this peer's own FULL container list (dc.listContainerSummaries — every
// container Docker knows about, NOT scoped to managed/labeled services, and
// NOT excludeSelf-filtered) plus its identity. Mirrors exactly what the
// LOCAL /api/logs/containers handler (logs.go) returns today, so a peer's
// Logs picker shows what logging into that peer directly would show —
// deliberately NOT built from Phase 1's narrower Service.Members data.
type peerLogsContainersResp struct {
	Identity   string             `json:"identity"`
	Containers []containerSummary `json:"containers"`
}

// peerLogsResp is the wire shape for GET /peer/logs/{container} — this
// peer's own containerLogs() output for one container, plus its identity.
type peerLogsResp struct {
	Identity  string    `json:"identity"`
	Container string    `json:"container"`
	Tail      int       `json:"tail"`
	Lines     []logLine `json:"lines"`
}

// peerLogsContainersHandler returns the HTTP handler for GET
// /peer/logs/containers on the dedicated peer-handshake port — same
// bearer-auth shape as peerImagesHandler.
func peerLogsContainersHandler(secret, identity string, dc *dockerClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		list, err := dc.listContainerSummaries(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerLogsContainersResp{Identity: identity, Containers: list})
	})
}

// peerLogsHandler returns the HTTP handler for GET /peer/logs/{container} on
// the dedicated peer-handshake port. Re-validates the container name itself
// (validContainerName) rather than trusting the forwarding host's own
// check — this handler is reachable by anyone possessing the shared peer
// secret and must be safe to call directly.
func peerLogsHandler(secret, identity string, dc *dockerClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/peer/logs/")
		if name == "" || name == "containers" || !validContainerName(name) {
			http.NotFound(w, r)
			return
		}
		tail := 200
		if t := r.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.Atoi(t); err == nil {
				tail = n
			}
		}
		lines, err := dc.containerLogs(r.Context(), name, tail)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerLogsResp{Identity: identity, Container: name, Tail: tail, Lines: lines})
	})
}

// peerStatsResp is the wire shape for GET /peer/stats — this peer's own
// local SysStats (GetStats(), stats.go) plus its identity.
type peerStatsResp struct {
	Identity string   `json:"identity"`
	Stats    SysStats `json:"stats"`
}

// peerStatsHandler returns the HTTP handler for GET /peer/stats on the
// dedicated peer-handshake port — same bearer-auth shape as
// peerServicesHandler/peerImagesHandler. No dockerClient dependency: GetStats
// reads host /proc + statfs directly, independent of Docker.
func peerStatsHandler(secret, identity string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerStatsResp{Identity: identity, Stats: GetStats()})
	})
}

// peerGETTimeout is peerGET with a caller-chosen timeout instead of the
// fixed 2s — for resources whose payload isn't small-and-fast like
// service-status/services/images. Container logs can be up to 4MB of raw
// daemon output demuxed and re-encoded as JSON; on a loaded peer a
// tail=5000 fetch can plausibly exceed 2s, and peerGET's fixed timeout would
// then look identical to "peer unreachable" in the UI. Callers must give
// their own *http.Client a Timeout >= timeout — a shorter client.Timeout
// silently defeats this function's whole purpose.
func peerGETTimeout(ctx context.Context, client *http.Client, peerBaseURL, secret, path string, timeout time.Duration, out any) error {
	url := strings.TrimRight(peerBaseURL, "/") + path
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s → %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s bad response: %w", url, err)
	}
	return nil
}

// peerMutate and mapPeerMutationErr are the write-side siblings of
// peerGET/peerGETTimeout, for the write-capable peer mesh (Phase 0 plumbing
// only — no /peer/* route calls these yet). Two things set writes apart from
// the read-only forwarding above:
//
//  1. Mutations are NOT idempotent. Onboarding, replacing an image, pruning —
//     all have side effects that must not double-fire. peerGET/peerGETTimeout
//     have no retry logic today either, but a future read-side retry would be
//     harmless; a future write-side retry is not, so peerMutate is written to
//     make a single attempt a structural guarantee, not just current behavior.
//  2. The error mapping is deliberately NOT the same collapse-everything-to-502
//     pattern peerGET's callers use (see writeCFErr for the read-side/Cloudflare
//     equivalent). A spurious 502 on a GET costs nothing — the caller just
//     retries the read. A mutation's caller needs to know the difference
//     between "the peer definitively rejected this" (400/404/409 — safe to
//     retry with corrected input) and "outcome unknown" (timeout, network
//     error, 5xx — the request may have already applied on the peer; blindly
//     retrying could double-fire it). mapPeerMutationErr preserves that
//     distinction instead of erasing it.

// peerMutate performs exactly ONE bearer-authenticated write request (method
// may be POST/DELETE/…) against peerBaseURL+path, bounded by the given
// timeout, and returns the peer's raw status code and response body bytes —
// unlike peerGET/peerGETTimeout, it does not collapse a non-2xx into an error
// the caller can only log; the caller passes statusCode+body straight to
// mapPeerMutationErr to relay a rejection verbatim. If respOut is non-nil and
// the peer answered 2xx, the body is also JSON-decoded into respOut.
// actorAssertion, when non-empty, is forwarded as the X-Pmgr-Actor header
// (see actor.go) so the peer's own audit()/auditUser() attributes the action
// to the real end user instead of a generic fallback — empty is a normal,
// silent no-op, not an error.
//
// Never retries, not even on a timeout — see the doc comment above. A caller
// that wants a retry must decide that itself, deliberately, with knowledge of
// which side of the "definitely rejected" vs "outcome unknown" line the
// failure fell on.
func peerMutate(ctx context.Context, client *http.Client, peerBaseURL, secret, method, path string, timeout time.Duration, reqBody io.Reader, respOut any, actorAssertion string) (statusCode int, respBody []byte, err error) {
	url := strings.TrimRight(peerBaseURL, "/") + path
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, url, reqBody)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if actorAssertion != "" {
		req.Header.Set(actorHeader, actorAssertion)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%s bad response: %w", url, err)
	}
	if respOut != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, respOut); err != nil {
			return resp.StatusCode, body, fmt.Errorf("%s bad response: %w", url, err)
		}
	}
	return resp.StatusCode, body, nil
}

// mapPeerMutationErr relays a peer's mutation response onto w. peerBody is
// only the already-drained response bytes (see peerMutate) — not the
// original *http.Response — so the peer's own Content-Type header isn't
// available here; relayed bodies are written as application/json, which
// matches every JSON-emitting /peer/* handler in this codebase.
//
//   - 400, 404, 409: the peer definitively rejected the request. Relay its
//     status code and body verbatim — the caller sees exactly why and can
//     safely retry with corrected input. Note: every existing /peer/*
//     handler also answers 404 when its bearer secret is empty (endpoint
//     disabled) — a peer with writes not yet enabled will therefore show up
//     here as "not found" rather than "writes disabled." That ambiguity is
//     inherent to relaying the peer's status verbatim, not a bug; a future
//     phase wanting to distinguish the two needs a signal outside this status
//     code (e.g. the writes-enabled field peerWritable in ui.go is waiting on).
//   - 401, 403: NOT relayed verbatim. That's the dashboard's own auth
//     vocabulary for the browser's session (see writeCFErr) — relaying a
//     peer's auth failure as-is would incorrectly pop a "session expired"
//     dialog in the requesting user's own browser. Mapped to 502.
//   - anything else (5xx, or peerStatusCode == 0 meaning a transport-level
//     error/timeout from peerMutate): 502 with a generic, debuggable message.
//     This is the "outcome unknown" case — the request may have already
//     applied on the peer before the error/timeout fired, so the caller must
//     not treat this the same as a definitive rejection. Convention for the
//     peerStatusCode == 0 case: callers pass peerMutate's returned error as
//     peerBody (i.e. []byte(err.Error())) so the message below still carries
//     the underlying transport error instead of being empty.
func mapPeerMutationErr(w http.ResponseWriter, peerStatusCode int, peerBody []byte) {
	switch peerStatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(peerStatusCode)
		w.Write(peerBody)
	case http.StatusUnauthorized, http.StatusForbidden:
		http.Error(w, "peer rejected mesh credentials", http.StatusBadGateway)
	default:
		http.Error(w, fmt.Sprintf("peer mutation outcome unknown (peer status %d): %s", peerStatusCode, string(peerBody)), http.StatusBadGateway)
	}
}

// fetchPeerServices polls every configured peer's /peer/services in parallel
// and returns their services, each tagged with that peer's own identity as
// Machine. A peer that's down, slow, or misconfigured is logged and simply
// omitted — advisory display only, same as fetchPeerServiceStatus.
func fetchPeerServices(ctx context.Context, registry *PeerRegistry, secret string) []Service {
	peers := registry.Peers()
	if len(peers) == 0 || secret == "" {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	results := make(chan []Service, len(peers))
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			var body peerServicesResp
			if err := peerGET(ctx, client, peer, secret, "/peer/services", &body); err != nil {
				log.Printf("dashboard peer services: %v", err)
				results <- nil
				return
			}
			machine := body.Identity
			if machine == "" {
				machine = peer // fallback label so a peer that forgot to set DASHBOARD_HOST still shows up as *something*
			}
			svcs := body.Services
			for i := range svcs {
				svcs[i].Machine = machine
			}
			results <- svcs
		}(peer)
	}
	wg.Wait()
	close(results)
	var out []Service
	for s := range results {
		out = append(out, s...)
	}
	return out
}

type peerView struct {
	URL         string    `json:"url"`
	Identity    string    `json:"identity,omitempty"`
	Version     string    `json:"version,omitempty"`
	OK          bool      `json:"ok"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	Behind      bool      `json:"behind,omitempty"`
	// Writes mirrors peerStatus.Writes — the frontend's only signal for
	// whether to show write controls for this peer (ui.go's peerWritable).
	Writes bool `json:"writes,omitempty"`
}

// peersStatusHandler serves this host's own identity+version, each
// configured peer's last-known identity/version/reachability, and the
// derived mesh floor — advisory display only, never gates any action.
func peersStatusHandler(registry *PeerRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := struct {
			Self struct {
				Identity string `json:"identity"`
				Version  string `json:"version"`
			} `json:"self"`
			Peers     []peerView `json:"peers"`
			MeshFloor *int       `json:"mesh_floor,omitempty"`
		}{}
		resp.Self.Version = buildVersion
		if registry != nil {
			resp.Self.Identity = registry.Identity()
			floor, floorOK := registry.MeshFloor()
			if floorOK {
				resp.MeshFloor = &floor
			}
			for url, st := range registry.Status() {
				v := peerView{URL: url, Identity: st.Identity, Version: st.Version, OK: st.OK, LastAttempt: st.LastAttempt, LastSuccess: st.LastSuccess, Writes: st.Writes}
				if floorOK {
					if n, err := strconv.Atoi(st.Version); err == nil && n < floor {
						v.Behind = true
					}
				}
				resp.Peers = append(resp.Peers, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

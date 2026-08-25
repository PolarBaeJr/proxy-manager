package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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
		p.recordResult(peer, false, "", "")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Auth failure or malformed request: worth surfacing so misconfig is visible.
		log.Printf("dashboard peer handshake: %s → %s", url, resp.Status)
		p.recordResult(peer, false, "", "")
		return
	}
	var body struct {
		Peer    string `json:"peer"`
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	p.recordResult(peer, true, body.Peer, body.Version)
}

// recordResult only overwrites Identity/Version on a successful handshake
// with a non-empty version — a transient failure preserves the last-known
// values instead of blanking them.
func (p *PeerRegistry) recordResult(peer string, ok bool, identity, version string) {
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
// minimal ok:true body with this host's identity/version.
func peerHandshakeHandler(secret, identity, version string) http.Handler {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"peer": identity, "ok": true, "version": version})
	})
}

// peerServiceStatusResp is the wire shape for GET /peer/service-status —
// this peer's own local service-status (never further merged, so a mesh
// can't loop) plus its identity, since the fetcher needs both to label the
// groups it merges in.
type peerServiceStatusResp struct {
	Identity string            `json:"identity"`
	Status   ServiceStatusResp `json:"status"`
}

// peerServiceStatusHandler returns the HTTP handler for GET
// /peer/service-status on the dedicated peer-handshake port — same
// bearer-auth shape as peerHandshakeHandler. Always returns THIS host's own
// local status only (never re-merges groups already tagged with another
// peer's Machine) — the caller (api.go's /api/service-status) is the one
// that fans out to every configured peer, so a peer answering this request
// never needs to know about the mesh beyond itself.
func peerServiceStatusHandler(secret, identity string, dc *dockerClient, proxyURL string) http.Handler {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerServiceStatusResp{Identity: identity, Status: status})
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
// their groups, each tagged with that peer's own identity as Machine. A
// peer that's down, slow, or misconfigured is logged and simply omitted —
// this is advisory display only, same as the rest of the peer mesh, never
// something the caller should fail on.
func fetchPeerServiceStatus(ctx context.Context, registry *PeerRegistry, secret string) []ServiceStatusGroup {
	peers := registry.Peers()
	if len(peers) == 0 || secret == "" {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	results := make(chan []ServiceStatusGroup, len(peers))
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			var body peerServiceStatusResp
			if err := peerGET(ctx, client, peer, secret, "/peer/service-status", &body); err != nil {
				log.Printf("dashboard peer service-status: %v", err)
				results <- nil
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
			results <- groups
		}(peer)
	}
	wg.Wait()
	close(results)
	var out []ServiceStatusGroup
	for g := range results {
		out = append(out, g...)
	}
	return out
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
func peerServicesHandler(secret, identity string, dc *dockerClient, onb *OnboardedStore, ic *imageChecker) http.Handler {
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
		svcs, err := buildManagedServices(r.Context(), dc, onb, ic)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerServicesResp{Identity: identity, Services: svcs})
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
				v := peerView{URL: url, Identity: st.Identity, Version: st.Version, OK: st.OK, LastAttempt: st.LastAttempt, LastSuccess: st.LastSuccess}
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

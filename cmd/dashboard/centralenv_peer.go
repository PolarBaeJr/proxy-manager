// Central per-service env: the PEER-MESH endpoints under /peer/central-env/.
//
//	GET  {svc}?for=<identity>  origin only — svc's env resolved for identity
//	POST {svc}/set             origin only — apply a set/unset edit
//	POST {svc}/notify          non-origin — "version N exists, go fetch it"
//	GET  {svc}/status          any host — role, version, members, job
//	POST {svc}/release         non-origin — drop this host's cached copy
//
// Every endpoint answers 404 unless DASHBOARD_PEER_SECRET is set AND
// CENTRAL_ENV is on here, so a pre-central-env peer and a feature-off peer
// look identical to a caller (both "unsupported"). Everything but /status
// additionally needs -peer-writes. The first GET is the only thing in the
// whole feature that puts resolved values on the wire; it is never logged,
// and its audit entry names the service, the asking host and the version.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

// centralEnvFetchTimeout bounds one origin fetch. Short on purpose: a create
// path waits on it, and an unreachable origin falls back to the cache.
const centralEnvFetchTimeout = 3 * time.Second

// peerCentralEnvResponse is the wire shape of the origin's
// GET /peer/central-env/{svc}?for=.
type peerCentralEnvResponse struct {
	Origin      string           `json:"origin"`
	Version     uint64           `json:"version"`
	Env         []string         `json:"env"`
	Healthcheck *healthcheckSpec `json:"healthcheck,omitempty"`
}

// centralEnvSetRequest is a PATCH-style edit of svc's central env: keys to
// set (literal or "ref:NAME") and unset in the base, and per-host override
// keys to set or drop. Compare-and-swapped against IfVersion; RequestID makes
// a retry of a timed-out call apply at most once.
type centralEnvSetRequest struct {
	RequestID          string                       `json:"request_id"`
	IfVersion          uint64                       `json:"if_version"`
	Set                map[string]string            `json:"set,omitempty"`
	Unset              []string                     `json:"unset,omitempty"`
	HostOverrides      map[string]map[string]string `json:"host_overrides,omitempty"`
	UnsetHostOverrides map[string][]string          `json:"unset_host_overrides,omitempty"`
}

// centralEnvSetResponse names what changed — keys, never values.
type centralEnvSetResponse struct {
	Version     uint64   `json:"version"`
	ChangedKeys []string `json:"changed_keys"`
	NoOp        bool     `json:"no_op,omitempty"`
	Replayed    bool     `json:"replayed,omitempty"`
}

// applyCentralEnvSet is the one place an edit lands in the origin store —
// shared by POST /peer/central-env/{svc}/set (a forwarded edit) and the
// origin's own /api/services/{svc}/env. Every "ref:NAME" is resolved once
// for every host the record names before anything is written, so a typo'd
// secret name is a 400 now rather than a failed propagation later.
func applyCentralEnvSet(ce *centralEnv, svc string, req centralEnvSetRequest, actor string) (centralEnvSetResponse, error) {
	rec, ok, err := ce.store.Get(svc)
	if err != nil {
		return centralEnvSetResponse{}, err
	}
	if !ok {
		return centralEnvSetResponse{}, errCentralEnvNotFound
	}
	base := copyStringMap(rec.Base)
	if base == nil {
		base = map[string]string{}
	}
	overrides := copyOverrides(rec.Overrides)
	if overrides == nil {
		overrides = map[string]map[string]string{}
	}
	changed := map[string]bool{}
	for k, v := range req.Set {
		if cur, ok := base[k]; !ok || cur != v {
			changed[k] = true
		}
		base[k] = v
	}
	for _, k := range req.Unset {
		if _, ok := base[k]; ok {
			changed[k] = true
		}
		delete(base, k)
	}
	for host, kv := range req.HostOverrides {
		if !validHostIdentity(host) {
			return centralEnvSetResponse{}, fmt.Errorf("invalid override host identity %q", host)
		}
		if overrides[host] == nil {
			overrides[host] = map[string]string{}
		}
		for k, v := range kv {
			if cur, ok := overrides[host][k]; !ok || cur != v {
				changed[k] = true
			}
			overrides[host][k] = v
		}
	}
	for host, keys := range req.UnsetHostOverrides {
		for _, k := range keys {
			if _, ok := overrides[host][k]; ok {
				changed[k] = true
			}
			delete(overrides[host], k)
		}
		if len(overrides[host]) == 0 {
			delete(overrides, host)
		}
	}
	if err := validateCentralEnvContent(base, overrides); err != nil {
		return centralEnvSetResponse{}, err
	}
	hosts := []string{"", ce.identity}
	for h := range overrides {
		hosts = append(hosts, h)
	}
	for _, h := range hosts {
		merged := copyStringMap(base)
		if merged == nil {
			merged = map[string]string{}
		}
		for k, v := range overrides[h] {
			merged[k] = v
		}
		if _, _, err := resolveSecretRefs(svc, merged, ce.secrets); err != nil {
			return centralEnvSetResponse{}, errCentralEnvBadRef{err: err}
		}
	}
	res, err := ce.store.Apply(svc, centralEnvChange{IfVersion: req.IfVersion, RequestID: req.RequestID, Base: base, Overrides: overrides}, actor)
	if err != nil {
		return centralEnvSetResponse{}, err
	}
	keys := make([]string, 0, len(changed))
	if !res.NoOp && !res.Replayed {
		for k := range changed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	out := centralEnvSetResponse{Version: res.Version, ChangedKeys: keys, NoOp: res.NoOp, Replayed: res.Replayed}
	if !res.NoOp && ce.sync != nil {
		// A replay requests too: harmless (request is idempotent and a
		// converged job rolls nothing), and it re-kicks a propagation the
		// original call's caller never saw start.
		ce.sync.request(svc)
	}
	return out, nil
}

// errCentralEnvBadRef is a "ref:NAME" in an edit that doesn't resolve. The
// wrapped error names the key and the secret — never a value.
type errCentralEnvBadRef struct{ err error }

func (e errCentralEnvBadRef) Error() string { return e.err.Error() }
func (e errCentralEnvBadRef) Unwrap() error { return e.err }

// validHostIdentity is the shape a peer identity (DASHBOARD_HOST, or the
// hostname fallback) can take — an override keyed by anything else could
// never match a host.
func validHostIdentity(s string) bool {
	return s != "" && len(s) <= 253 && !strings.ContainsAny(s, " /\\=\x00\t\n")
}

// newOriginFetcher is centralEnv.fetchFromOrigin for the real mesh: the
// origin's peer URL comes from the handshake registry (keyed by identity, so
// a notify can only ever make this host fetch from a configured peer), and
// the request is bearer-authenticated with DASHBOARD_PEER_SECRET.
func newOriginFetcher(registry *PeerRegistry, secret string) func(ctx context.Context, origin, svc, forIdentity string) (centralEnvFetched, error) {
	client := &http.Client{Timeout: centralEnvFetchTimeout + time.Second}
	return func(ctx context.Context, origin, svc, forIdentity string) (centralEnvFetched, error) {
		if registry == nil || secret == "" {
			return centralEnvFetched{}, errors.New("peer mesh not configured")
		}
		peerURL, ok := registry.URLForIdentity(origin)
		if !ok {
			return centralEnvFetched{}, fmt.Errorf("origin %s is not a known peer", origin)
		}
		var resp peerCentralEnvResponse
		path := "/peer/central-env/" + url.PathEscape(svc) + "?for=" + url.QueryEscape(forIdentity)
		if err := peerGETTimeout(ctx, client, peerURL, secret, path, centralEnvFetchTimeout, &resp); err != nil {
			// peerGETTimeout's error names the URL and status, never the
			// body — fine to return as-is.
			return centralEnvFetched{}, err
		}
		if resp.Origin != origin || resp.Version == 0 {
			return centralEnvFetched{}, fmt.Errorf("origin %s answered for %q with an unexpected origin/version", origin, svc)
		}
		return centralEnvFetched{Env: resp.Env, Version: resp.Version, Healthcheck: resp.Healthcheck}, nil
	}
}

// originHealthcheck is svc's template healthcheck on this host (nil when it
// has none, or no replica to read it from) — what the origin ships alongside
// the env so a peer's recreated replicas are health-gated the same way.
func originHealthcheck(ctx context.Context, dc *dockerClient, svc string) *healthcheckSpec {
	all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err != nil {
		return nil
	}
	live := liveOnly(all)
	if len(live) == 0 {
		return nil
	}
	spec, err := dc.inspectCloneSpec(ctx, preferRunning(live)[0].ID)
	if err != nil || healthcheckMissing(spec.Healthcheck) {
		return nil
	}
	return copyHealthcheck(spec.Healthcheck)
}

type centralEnvPeer struct {
	secret        string
	ce            *centralEnv
	dc            *dockerClient
	writesEnabled bool
}

// peerAuth gates every /peer/central-env/ endpoint: 404 (indistinguishable
// from an old dashboard) unless the mesh secret is set, central env is on,
// and — for needWrites — -peer-writes is on; then a constant-time bearer
// check.
func (p *centralEnvPeer) peerAuth(w http.ResponseWriter, r *http.Request, needWrites bool) bool {
	if p.secret == "" || !p.ce.Enabled() || (needWrites && !p.writesEnabled) {
		http.NotFound(w, r)
		return false
	}
	want := []byte("Bearer " + p.secret)
	got := []byte(r.Header.Get("Authorization"))
	if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func peerCentralEnvHandler(secret string, ce *centralEnv, dc *dockerClient, writesEnabled bool) http.Handler {
	p := &centralEnvPeer{secret: secret, ce: ce, dc: dc, writesEnabled: writesEnabled}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := strings.TrimPrefix(r.URL.Path, "/peer/central-env/")
		svc, action, _ := strings.Cut(sub, "/")
		if !validServiceName(svc) {
			http.NotFound(w, r)
			return
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			p.getEnv(w, r, svc)
		case action == "set" && r.Method == http.MethodPost:
			p.set(w, r, svc)
		case action == "notify" && r.Method == http.MethodPost:
			p.notify(w, r, svc)
		case action == "status" && r.Method == http.MethodGet:
			p.status(w, r, svc)
		case action == "release" && r.Method == http.MethodPost:
			p.release(w, r, svc)
		default:
			// Auth first so an unauthenticated caller can't map which
			// action names exist.
			if p.peerAuth(w, r, false) {
				http.NotFound(w, r)
			}
		}
	})
}

// getEnv is the ONE response in the feature that carries values. Origin
// only; never logged.
func (p *centralEnvPeer) getEnv(w http.ResponseWriter, r *http.Request, svc string) {
	if !p.peerAuth(w, r, true) {
		return
	}
	forIdentity := r.URL.Query().Get("for")
	if !validHostIdentity(forIdentity) {
		http.Error(w, "for=<identity> is required", http.StatusBadRequest)
		return
	}
	res, owned, err := p.ce.ResolveForPeer(svc, forIdentity)
	if !owned {
		http.Error(w, "not this host's central env", http.StatusNotFound)
		return
	}
	if err != nil {
		// effectiveEnvAt's errors name keys and secrets, never values.
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	audit(r, "peer-mesh", "service.env_fetch", fmt.Sprintf("%s v%d for %s", svc, res.Version, forIdentity))
	httpx.WriteJSON(w, http.StatusOK, peerCentralEnvResponse{
		Origin: res.Origin, Version: res.Version, Env: res.Env,
		Healthcheck: originHealthcheck(r.Context(), p.dc, svc),
	})
}

func (p *centralEnvPeer) set(w http.ResponseWriter, r *http.Request, svc string) {
	if !p.peerAuth(w, r, true) {
		return
	}
	if !p.ce.store.Has(svc) {
		http.Error(w, "not this host's central env", http.StatusNotFound)
		return
	}
	var req centralEnvSetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	res, err := applyCentralEnvSet(p.ce, svc, req, auditUser(r, "peer-mesh"))
	if err != nil {
		writeCentralEnvErr(w, err)
		return
	}
	if !res.NoOp && !res.Replayed {
		audit(r, "peer-mesh", "service.env_set", fmt.Sprintf("%s v%d keys=%s", svc, res.Version, strings.Join(res.ChangedKeys, ",")))
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (p *centralEnvPeer) notify(w http.ResponseWriter, r *http.Request, svc string) {
	if !p.peerAuth(w, r, true) {
		return
	}
	var body struct {
		Origin  string `json:"origin"`
		Version uint64 `json:"version"`
		// Retry: an operator's explicit sync — forget a version this host
		// failed (and rolled back from) and try it again.
		Retry bool `json:"retry,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.Origin == "" || body.Version == 0 {
		http.Error(w, "origin and version are required", http.StatusBadRequest)
		return
	}
	if p.ce.store.Has(svc) || body.Origin == p.ce.identity {
		http.Error(w, "this host is the central env origin for "+svc, http.StatusConflict)
		return
	}
	cached, hasCache := p.ce.cache.Get(svc)
	if hasCache && cached.Origin != body.Origin {
		http.Error(w, fmt.Sprintf("%s is managed from %s here, not %s", svc, cached.Origin, body.Origin), http.StatusConflict)
		return
	}
	if p.ce.sync == nil {
		http.Error(w, "central env propagation is not running", http.StatusServiceUnavailable)
		return
	}
	if body.Retry {
		p.ce.sync.clearFailures(svc)
	}
	if hasCache && cached.Version >= body.Version && !p.ce.sync.busy(svc) {
		if _, members, err := p.ce.sync.localMembers(r.Context(), svc); err == nil && membersAtLeast(members, body.Version) {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "converged", "version": cached.Version})
			return
		}
	}
	p.ce.sync.requestPeer(svc, body.Origin, body.Version)
	audit(r, "peer-mesh", "service.env_sync_start", fmt.Sprintf("%s v%d from %s", svc, body.Version, body.Origin))
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "version": body.Version})
}

func membersAtLeast(members []envMember, version uint64) bool {
	for _, mb := range members {
		if !mb.Foreign && mb.EnvVersion < version {
			return false
		}
	}
	return true
}

func (p *centralEnvPeer) status(w http.ResponseWriter, r *http.Request, svc string) {
	if !p.peerAuth(w, r, false) {
		return
	}
	if p.ce.sync == nil {
		http.Error(w, "central env propagation is not running", http.StatusServiceUnavailable)
		return
	}
	st, err := p.ce.sync.status(r.Context(), svc)
	if err != nil {
		httpx.WriteErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

// release drops this host's cached copy of svc's env. Replicas already
// carrying pmgr.env.origin stay managed (Resolve re-fetches from the
// origin on the next create), so this is for a host that no longer runs svc
// at all, or ahead of an un-adopt.
func (p *centralEnvPeer) release(w http.ResponseWriter, r *http.Request, svc string) {
	if !p.peerAuth(w, r, true) {
		return
	}
	if p.ce.store.Has(svc) {
		http.Error(w, "this host is the central env origin for "+svc, http.StatusConflict)
		return
	}
	if err := p.ce.cache.Delete(svc); err != nil {
		httpx.WriteErr(w, err)
		return
	}
	audit(r, "peer-mesh", "service.env_release", svc)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "released"})
}

// writeCentralEnvErr maps a central-env error onto its status. Bodies carry
// names and versions only — every one of these errors is built that way.
func writeCentralEnvErr(w http.ResponseWriter, err error) {
	var conflict errEnvVersionConflict
	var managed errEnvCentrallyManaged
	var unavailable errCentralEnvUnavailable
	var badRef errCentralEnvBadRef
	var broken errCentralEnvBroken
	var unknown errCentralEnvOutcomeUnknown
	var relay errCentralEnvRelay
	var failedHere errCentralEnvFailedHere
	var cacheFailed errCentralEnvCacheFailed
	switch {
	case errors.As(err, &unknown):
		httpx.WriteJSON(w, http.StatusGatewayTimeout, map[string]any{"error": err.Error(), "request_id": unknown.RequestID})
	case errors.As(err, &relay):
		mapPeerMutationErr(w, relay.code, relay.body)
	case errors.As(err, &conflict):
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "current_version": conflict.Current})
	case errors.As(err, &managed):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.As(err, &unavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errCentralEnvNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.As(err, &broken), errors.As(err, &failedHere), errors.As(err, &cacheFailed):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.As(err, &badRef):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

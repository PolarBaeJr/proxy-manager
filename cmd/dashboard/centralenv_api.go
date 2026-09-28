// Central per-service env: the dashboard API.
//
//	GET  /api/services/{svc}/env       names-only view (any host)
//	POST /api/services/{svc}/env       set/unset keys and host overrides
//	POST /api/services/{svc}/env/sync  kick propagation + one reconcile pass
//	POST /api/services/{svc}/env/adopt   preflight (dry run, the default) or
//	                                      adopt; the origin is this host, or
//	                                      ?host=<identity> (forwarded there)
//	POST /api/services/{svc}/env/release un-adopt; runs on the origin — a
//	                                      non-origin forwards it there
//
// Dispatched ahead of ?host= forwarding: a central env has one owner no
// matter which host the request lands on, so a non-origin host forwards the
// edit to the origin itself (one write, one version) rather than to whatever
// host the UI happened to name. Adopt and release read ?host= themselves:
// for adopt it names the origin to create, for release it is only an
// assertion of which host the origin is. Responses carry key names, versions
// and counts — never a value.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

// centralEnvForwardTimeout bounds a non-origin host's forwarded set. A var so
// tests can shrink it.
var centralEnvForwardTimeout = 10 * time.Second

// centralEnvAdoptForwardTimeout bounds an adopt forwarded to the origin it
// names. Longer than a set: the origin's preflight asks every peer's
// live-keys (adoptPeerCallTimeout each, the forwarding host included), and
// an execute also fetches live-env. A var so tests can shrink it.
var centralEnvAdoptForwardTimeout = 45 * time.Second

// centralEnvAdoptForwardClient carries forwarded adopts. No Timeout of its
// own: m.client's 15s would cut a slow-but-healthy preflight short, so
// peerMutate's context deadline (centralEnvAdoptForwardTimeout) governs.
var centralEnvAdoptForwardClient = &http.Client{}

// errCentralEnvPeerForward is a forwarded adopt/release that got no usable
// answer from the host it was sent to — Status is what this host answers.
type errCentralEnvPeerForward struct {
	Status int
	Msg    string
}

func (e errCentralEnvPeerForward) Error() string { return e.Msg }

// errCentralEnvOutcomeUnknown: a forwarded set was sent but no answer came
// back in time — the origin may or may not have applied it. Retrying with
// the same request_id is safe (the origin replays it rather than applying
// twice).
type errCentralEnvOutcomeUnknown struct {
	Service   string
	Origin    string
	RequestID string
}

func (e errCentralEnvOutcomeUnknown) Error() string {
	return fmt.Sprintf("central env for %q: origin %s did not answer in time — outcome unknown; retry with the same request_id", e.Service, e.Origin)
}

// errCentralEnvRelay is the origin's own definitive rejection of a forwarded
// set (400/404/409), relayed verbatim — its bodies are names/versions only.
type errCentralEnvRelay struct {
	code int
	body []byte
}

func (e errCentralEnvRelay) Error() string {
	return fmt.Sprintf("origin rejected the change (status %d): %s", e.code, strings.TrimSpace(string(e.body)))
}

func centralEnvOf(dc *dockerClient) *centralEnv {
	ce, _ := dc.central.(*centralEnv)
	if !ce.Enabled() || ce.sync == nil {
		return nil
	}
	return ce
}

func newCentralEnvRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// centralEnvServiceTemplate is svc's preferred live replica on this host,
// the same one every create path clones from. ok=false: none here.
func centralEnvServiceTemplate(ctx context.Context, dc *dockerClient, svc string) (dockerContainer, bool, error) {
	all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err != nil {
		return dockerContainer{}, false, err
	}
	live := liveOnly(all)
	if len(live) == 0 {
		return dockerContainer{}, false, nil
	}
	return preferRunning(live)[0], true, nil
}

// centralEnvOriginFor is svc's origin as this host knows it: itself, the
// cache's origin, or the pmgr.env.origin stamp on a local replica.
func centralEnvOriginFor(ce *centralEnv, svc string, labels map[string]string) string {
	if ce.store.Has(svc) {
		return ce.identity
	}
	if cached, ok := ce.cache.Get(svc); ok {
		return cached.Origin
	}
	return labels[labelEnvOrigin]
}

// submitCentralEnvSet lands req in svc's origin store: directly on the
// origin, forwarded over the peer mesh otherwise. An origin that can't be
// reached at all is errCentralEnvUnavailable — nothing was written anywhere;
// one that was reached but never answered is errCentralEnvOutcomeUnknown.
func submitCentralEnvSet(ctx context.Context, r *http.Request, ce *centralEnv, svc, origin string, req centralEnvSetRequest, actor string) (centralEnvSetResponse, error) {
	if origin == ce.identity {
		return applyCentralEnvSet(ce, svc, req, actor)
	}
	m := ce.sync
	peerURL, ok := "", false
	if m.registry != nil && m.secret != "" {
		peerURL, ok = m.registry.URLForIdentity(origin)
	}
	if !ok {
		return centralEnvSetResponse{}, errCentralEnvUnavailable{Service: svc, Origin: origin}
	}
	body, _ := json.Marshal(req)
	var out centralEnvSetResponse
	code, respBody, err := peerMutate(ctx, m.client, peerURL, m.secret, http.MethodPost, "/peer/central-env/"+url.PathEscape(svc)+"/set", centralEnvForwardTimeout, bytes.NewReader(body), &out, mintForwardedActor(r, actor))
	if err != nil {
		if code == 0 && isDialError(err) {
			return centralEnvSetResponse{}, errCentralEnvUnavailable{Service: svc, Origin: origin}
		}
		return centralEnvSetResponse{}, errCentralEnvOutcomeUnknown{Service: svc, Origin: origin, RequestID: req.RequestID}
	}
	switch code {
	case http.StatusOK:
		return out, nil
	case http.StatusConflict:
		var c struct {
			CurrentVersion uint64 `json:"current_version"`
		}
		if json.Unmarshal(respBody, &c) == nil && c.CurrentVersion > 0 {
			return centralEnvSetResponse{}, errEnvVersionConflict{Current: c.CurrentVersion}
		}
		return centralEnvSetResponse{}, errCentralEnvRelay{code: code, body: respBody}
	case http.StatusBadRequest, http.StatusNotFound:
		return centralEnvSetResponse{}, errCentralEnvRelay{code: code, body: respBody}
	case http.StatusServiceUnavailable:
		// The origin answered but couldn't resolve its own env (a missing
		// secret). Nothing was written.
		return centralEnvSetResponse{}, errCentralEnvUnavailable{Service: svc, Origin: origin}
	}
	return centralEnvSetResponse{}, errCentralEnvOutcomeUnknown{Service: svc, Origin: origin, RequestID: req.RequestID}
}

// isDialError: the connection was never established, so the request can't
// have been applied.
func isDialError(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

// writePeerRelay writes a peer's answer through as-is.
func writePeerRelay(w http.ResponseWriter, code int, body []byte) {
	if json.Valid(body) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(code)
	w.Write(body)
}

// forwardCentralEnvAdopt runs an adopt (dry run or execute) on target, which
// becomes the origin. Never retries: an execute's request_id is minted
// before the forward, so an operator's retry after an unknown outcome
// replays on target instead of adopting twice. The peer's 401/403 are never
// relayed as-is — to the UI they mean "session expired" / "2FA required".
func forwardCentralEnvAdopt(w http.ResponseWriter, r *http.Request, ce *centralEnv, svc, target string, req centralEnvAdoptRequest, actor string) {
	m := ce.sync
	if !validHostIdentity(target) {
		http.Error(w, fmt.Sprintf("invalid host %q", target), http.StatusBadRequest)
		return
	}
	if m.registry == nil || m.secret == "" {
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusServiceUnavailable, Msg: "peer mesh not configured here — cannot adopt onto " + target})
		return
	}
	peerURL, ok := m.registry.URLForIdentity(target)
	if !ok {
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusNotFound, Msg: "unknown host " + target})
		return
	}
	body, _ := json.Marshal(req)
	code, respBody, err := peerMutate(r.Context(), centralEnvAdoptForwardClient, peerURL, m.secret, http.MethodPost, "/peer/central-env/"+url.PathEscape(svc)+"/adopt", centralEnvAdoptForwardTimeout, bytes.NewReader(body), nil, mintForwardedActor(r, actor))
	if err != nil {
		switch {
		case code == 0 && isDialError(err):
			writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusServiceUnavailable, Msg: target + " is unreachable — nothing was sent"})
		case req.dryRun():
			writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusGatewayTimeout, Msg: target + " did not answer the adopt dry run in time — a dry run changes nothing; retry"})
		default:
			writeCentralEnvErr(w, errCentralEnvOutcomeUnknown{Service: svc, Origin: target, RequestID: req.RequestID})
		}
		return
	}
	peerText := strings.TrimSpace(string(respBody))
	switch code {
	case http.StatusOK, http.StatusAccepted, http.StatusBadRequest, http.StatusConflict:
		writePeerRelay(w, code, respBody)
	case http.StatusNotFound:
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusConflict, Msg: "peer " + target + " does not support forwarded adopt — deploy the new dashboard there (and enable -peer-writes)"})
	case http.StatusUnauthorized:
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusBadGateway, Msg: "peer " + target + " rejected mesh credentials"})
	case http.StatusForbidden:
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": peerText})
	default:
		// A definitive failure: nothing runs after CreateAdopted errors, so
		// this is not an unknown outcome.
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusBadGateway, Msg: "adopt on " + target + " failed: " + peerText})
	}
}

// serveCentralEnvAdoptLocal runs an adopt with this host as the origin —
// shared by the API and the peer /adopt, so a forwarded adopt answers
// exactly like a local one.
func serveCentralEnvAdoptLocal(w http.ResponseWriter, r *http.Request, ce *centralEnv, svc string, req centralEnvAdoptRequest, actor string) {
	if req.dryRun() {
		rep, _, err := ce.sync.adoptPreflight(r.Context(), svc, req, "")
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rep)
		return
	}
	if req.RequestID == "" {
		req.RequestID = newCentralEnvRequestID()
	}
	res, err := ce.sync.adoptExecute(r.Context(), r, svc, req, actor)
	var refused errAdoptRefused
	switch {
	case errors.As(err, &refused):
		body := map[string]any{"error": refused.reason, "request_id": req.RequestID}
		if refused.report.Service != "" {
			body["report"] = refused.report
		}
		httpx.WriteJSON(w, http.StatusConflict, body)
	case errors.Is(err, errCentralEnvNotFound):
		// 404 on the peer wire means "unsupported" to a forwarder.
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		writeCentralEnvErr(w, err)
	default:
		httpx.WriteJSON(w, http.StatusAccepted, res)
	}
}

// forwardCentralEnvRelease asks origin to start releasing svc. Releasing
// again is always safe (it resumes), so an unanswered forward says so.
func forwardCentralEnvRelease(w http.ResponseWriter, r *http.Request, ce *centralEnv, svc, origin, actor string) {
	m := ce.sync
	if !validHostIdentity(origin) {
		http.Error(w, fmt.Sprintf("invalid origin %q", origin), http.StatusBadRequest)
		return
	}
	if m.registry == nil || m.secret == "" {
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusServiceUnavailable, Msg: "peer mesh not configured here — cannot reach origin " + origin})
		return
	}
	peerURL, ok := m.registry.URLForIdentity(origin)
	if !ok {
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusServiceUnavailable, Msg: "origin " + origin + " is not a known peer"})
		return
	}
	code, respBody, err := peerMutate(r.Context(), m.client, peerURL, m.secret, http.MethodPost, "/peer/central-env/"+url.PathEscape(svc)+"/release-request", centralEnvForwardTimeout, nil, nil, mintForwardedActor(r, actor))
	if err != nil {
		if code == 0 && isDialError(err) {
			writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusServiceUnavailable, Msg: "origin " + origin + " is unreachable — release not started"})
			return
		}
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusGatewayTimeout, Msg: "origin " + origin + " did not answer in time — the release may or may not have started; releasing again is safe (it resumes)"})
		return
	}
	switch code {
	case http.StatusAccepted, http.StatusConflict:
		writePeerRelay(w, code, respBody)
	case http.StatusNotFound:
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusConflict, Msg: "origin " + origin + " does not support forwarded release — deploy the new dashboard there (and enable -peer-writes), or release on " + origin + " directly"})
	case http.StatusUnauthorized, http.StatusForbidden:
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusBadGateway, Msg: "origin " + origin + " rejected mesh credentials"})
	default:
		writeCentralEnvErr(w, errCentralEnvPeerForward{Status: http.StatusBadGateway, Msg: "release on " + origin + " failed: " + strings.TrimSpace(string(respBody))})
	}
}

// centralEnvHostView is one host's convergence in the GET /env view.
type centralEnvHostView struct {
	Identity  string `json:"identity"`
	Version   uint64 `json:"version"`
	Stale     bool   `json:"stale,omitempty"`
	FetchedAt int64  `json:"fetched_at,omitempty"`
	Converged bool   `json:"converged"`
	Capable   bool   `json:"capable"`
	Replicas  int    `json:"replicas"`
	// FailedVersion: a version this host failed its health gate on and
	// rolled back from; it stays on the earlier one until a newer arrives.
	FailedVersion uint64 `json:"failed_version,omitempty"`
}

// centralEnvView is GET /api/services/{svc}/env. Names only.
type centralEnvView struct {
	Managed   bool                 `json:"managed"`
	Origin    string               `json:"origin,omitempty"`
	Role      string               `json:"role,omitempty"`
	Version   uint64               `json:"version,omitempty"`
	State     string               `json:"state,omitempty"`
	Adopting  bool                 `json:"adopting,omitempty"`
	Stale     bool                 `json:"stale,omitempty"`
	Keys      []string             `json:"keys"`
	Refs      map[string]string    `json:"refs,omitempty"`
	Overrides map[string][]string  `json:"overrides,omitempty"`
	Hosts     []centralEnvHostView `json:"hosts"`
	Job       *envSyncJob          `json:"job,omitempty"`
	// LastFailure is the origin's most recent non-converged propagation —
	// kept after later jobs replace Job, so a revert or a partial never
	// silently drops out of view.
	LastFailure *envSyncFailure `json:"last_failure,omitempty"`
	Warnings    []string        `json:"warnings"`
}

func hostViewFrom(ps peerCentralEnvStatus, capable bool, target uint64) centralEnvHostView {
	n := 0
	for _, mb := range ps.Members {
		if !mb.Foreign {
			n++
		}
	}
	return centralEnvHostView{Identity: ps.Identity, Version: ps.Version, Stale: ps.Stale, FetchedAt: ps.FetchedAt,
		Converged: ps.atLeast(target), Capable: capable, Replicas: n, FailedVersion: ps.FailedVersion}
}

// buildCentralEnvView assembles the view for svc: names from the origin
// (itself, or asked over the mesh — falling back to this host's cached key
// names, flagged stale), then every host's convergence.
func buildCentralEnvView(ctx context.Context, ce *centralEnv, svc string) (centralEnvView, error) {
	view := centralEnvView{Keys: []string{}, Hosts: []centralEnvHostView{}, Warnings: []string{}}
	tpl, hasTpl, err := centralEnvServiceTemplate(ctx, ce.sync.dc, svc)
	if err != nil {
		return view, err
	}
	var labels map[string]string
	if hasTpl {
		labels = tpl.Labels
	}
	if !ce.Managed(svc, labels) {
		// An adopt that was undone, or a finished release, leaves no record
		// — its job and failure must still be visible here.
		if j, ok := ce.sync.get(svc); ok {
			view.Job = j
		}
		view.LastFailure = ce.sync.failure(svc)
		return view, nil
	}
	view.Managed = true
	view.Origin = centralEnvOriginFor(ce, svc, labels)
	m := ce.sync

	self, err := m.status(ctx, svc)
	if err != nil {
		return view, err
	}
	if view.Origin == ce.identity {
		view.Role = envSyncRoleOrigin
		view.LastFailure = self.LastFailure
		view.Version, view.State, view.Adopting = self.Version, self.State, self.Adopting
		view.Keys, view.Refs, view.Overrides = self.Keys, self.Refs, self.Overrides
	} else {
		view.Role = envSyncRolePeer
		var ps peerCentralEnvStatus
		reached := false
		if m.registry != nil && m.secret != "" {
			if peerURL, ok := m.registry.URLForIdentity(view.Origin); ok {
				var code int
				ps, code, err = m.peerStatus(ctx, peerURL, svc)
				reached = err == nil && code == http.StatusOK && ps.Role == envSyncRoleOrigin
			}
		}
		if reached {
			m.markOriginSeen(svc)
			view.LastFailure = ps.LastFailure
			view.Version, view.State, view.Adopting = ps.Version, ps.State, ps.Adopting
			view.Keys, view.Refs, view.Overrides = ps.Keys, ps.Refs, ps.Overrides
			self, _ = m.status(ctx, svc)
		} else {
			view.Stale = true
			view.LastFailure = self.LastFailure
			if cached, ok := ce.cache.Get(svc); ok {
				view.Version = cached.Version
				for _, e := range cached.Env {
					if k, _, ok := splitEnvEntry(e); ok {
						view.Keys = append(view.Keys, k)
					}
				}
				sort.Strings(view.Keys)
			}
			view.Warnings = append(view.Warnings, fmt.Sprintf("origin %s unreachable — showing this host's cached key names (v%d)", view.Origin, view.Version))
		}
	}
	if view.Keys == nil {
		view.Keys = []string{}
	}
	view.Hosts = append(view.Hosts, hostViewFrom(self, true, view.Version))
	for _, p := range m.peers() {
		if !p.capable {
			continue
		}
		ps, code, err := m.peerStatus(ctx, p.url, svc)
		if err != nil || code != http.StatusOK || !ps.runsOrCaches() {
			continue
		}
		view.Hosts = append(view.Hosts, hostViewFrom(ps, true, view.Version))
	}
	if j, ok := m.get(svc); ok {
		view.Job = j
		view.Warnings = append(view.Warnings, j.Warnings...)
		if j.Status == envSyncStatusPartial || j.Status == envSyncStatusFailedReverted || j.Status == envSyncStatusDegraded || j.Status == envSyncStatusFailed {
			view.Warnings = append(view.Warnings, fmt.Sprintf("last propagation of v%d ended %s", j.Target, j.Status))
		}
	}
	if f := view.LastFailure; f != nil && (view.Job == nil || view.Job.Target != f.Version || view.Job.Status != f.Status) {
		view.Warnings = append(view.Warnings, fmt.Sprintf("most recent failure: v%d ended %s at %s", f.Version, f.Status, f.At.UTC().Format(time.RFC3339)))
	}
	for _, h := range view.Hosts {
		switch {
		case h.FailedVersion > h.Version:
			view.Warnings = append(view.Warnings, fmt.Sprintf("%s failed v%d and stays on v%d until a newer version (or a sync retries it)", h.Identity, h.FailedVersion, h.Version))
		case h.FailedVersion > 0:
			view.Warnings = append(view.Warnings, fmt.Sprintf("%s failed v%d with no earlier version to fall back to — new replicas there are refused until a newer version (or a sync retries it)", h.Identity, h.FailedVersion))
		}
		if h.Stale {
			view.Warnings = append(view.Warnings, fmt.Sprintf("%s has lost touch with the origin (using its cached copy)", h.Identity))
		}
	}
	return view, nil
}

// serveCentralEnvAPI handles /api/services/{svc}/env and /env/sync. False
// when central env isn't wired here — the caller then 404s, exactly like
// before the feature existed.
func serveCentralEnvAPI(w http.ResponseWriter, r *http.Request, dc *dockerClient, auth *AuthStore, svc, sub string) bool {
	ce := centralEnvOf(dc)
	if ce == nil {
		return false
	}
	actor := sessionUser(sessionFromReq(auth, r))
	if actor == "" {
		actor = principalFrom(r)
	}
	switch {
	case sub == "env" && r.Method == http.MethodGet:
		view, err := buildCentralEnvView(r.Context(), ce, svc)
		if err != nil {
			httpx.WriteErr(w, err)
			return true
		}
		httpx.WriteJSON(w, http.StatusOK, view)
	case sub == "env" && r.Method == http.MethodPost:
		if self, err := dc.serviceContainsSelfByName(r.Context(), svc); err == nil && self {
			http.Error(w, "refusing to manage the dashboard's own service from within itself — use docker compose on the host", http.StatusForbidden)
			return true
		}
		var req centralEnvSetRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return true
		}
		tpl, hasTpl, err := centralEnvServiceTemplate(r.Context(), dc, svc)
		if err != nil {
			httpx.WriteErr(w, err)
			return true
		}
		var labels map[string]string
		if hasTpl {
			labels = tpl.Labels
		}
		if !ce.Managed(svc, labels) {
			http.Error(w, fmt.Sprintf("%q is not centrally managed", svc), http.StatusNotFound)
			return true
		}
		if req.RequestID == "" {
			req.RequestID = newCentralEnvRequestID()
		}
		origin := centralEnvOriginFor(ce, svc, labels)
		res, err := submitCentralEnvSet(r.Context(), r, ce, svc, origin, req, actor)
		if err != nil {
			writeCentralEnvErr(w, err)
			return true
		}
		if !res.NoOp && !res.Replayed {
			audit(r, actor, "service.env_set", fmt.Sprintf("%s v%d keys=%s via %s", svc, res.Version, strings.Join(res.ChangedKeys, ","), origin))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"version": res.Version, "changed_keys": res.ChangedKeys, "no_op": res.NoOp, "replayed": res.Replayed,
			"request_id": req.RequestID, "origin": origin,
		})
	case sub == "env/sync" && r.Method == http.MethodPost:
		tpl, hasTpl, err := centralEnvServiceTemplate(r.Context(), dc, svc)
		if err != nil {
			httpx.WriteErr(w, err)
			return true
		}
		var labels map[string]string
		if hasTpl {
			labels = tpl.Labels
		}
		if !ce.Managed(svc, labels) {
			http.Error(w, fmt.Sprintf("%q is not centrally managed", svc), http.StatusNotFound)
			return true
		}
		m := ce.sync
		m.clearFailures(svc)
		if ce.store.Has(svc) {
			m.request(svc)
		} else {
			origin := centralEnvOriginFor(ce, svc, labels)
			v, _ := ce.Version(svc)
			// A floor, not a target: the job always fetches the origin's
			// newest, and reconcilePeer below raises it if the origin is
			// already past what's cached.
			m.reconcilePeer(r.Context(), svc)
			m.requestPeer(svc, origin, v)
		}
		audit(r, actor, "service.env_sync_start", svc+" (manual)")
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "sync requested"})
	case sub == "env/adopt" && r.Method == http.MethodPost:
		var req centralEnvAdoptRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return true
		}
		target := strings.TrimSpace(r.URL.Query().Get("host"))
		if target == "" || target == ce.identity {
			serveCentralEnvAdoptLocal(w, r, ce, svc, req, actor)
			return true
		}
		if !req.dryRun() && req.RequestID == "" {
			req.RequestID = newCentralEnvRequestID()
		}
		forwardCentralEnvAdopt(w, r, ce, svc, target, req, actor)
	case sub == "env/release" && r.Method == http.MethodPost:
		// The origin decides from its own record alone, so a Docker hiccup
		// can't block a release there; only a non-origin needs the labels.
		var labels map[string]string
		if !ce.store.Has(svc) {
			tpl, hasTpl, err := centralEnvServiceTemplate(r.Context(), dc, svc)
			if err != nil {
				httpx.WriteErr(w, err)
				return true
			}
			if hasTpl {
				labels = tpl.Labels
			}
		}
		managed := ce.Managed(svc, labels)
		origin := centralEnvOriginFor(ce, svc, labels)
		if want := strings.TrimSpace(r.URL.Query().Get("host")); want != "" && managed && want != origin {
			http.Error(w, fmt.Sprintf("%q's central env origin is %s, not %s — release always runs on the origin", svc, origin, want), http.StatusConflict)
			return true
		}
		switch {
		case ce.store.Has(svc):
			body, err := startCentralEnvRelease(r, ce, svc, actor)
			if err != nil {
				writeCentralEnvErr(w, err)
				return true
			}
			httpx.WriteJSON(w, http.StatusAccepted, body)
		case managed && (origin == "" || origin == ce.identity):
			http.Error(w, fmt.Sprintf("release %q on its origin %s", svc, origin), http.StatusConflict)
		case managed:
			forwardCentralEnvRelease(w, r, ce, svc, origin, actor)
		default:
			http.Error(w, fmt.Sprintf("%q is not centrally managed", svc), http.StatusNotFound)
		}
	default:
		http.NotFound(w, r)
	}
	return true
}

// interceptCentralEnvReplace turns a replace / rolling-replace that carries
// Env on a centrally managed service into what it has to mean now: an edit
// of the central env (which then rolls every host, health-gated) — but only
// when the image is unchanged, since one request can't be both a central env
// change and a per-host image change. Runs synchronously before the replace
// itself (rolling-replace would otherwise only discover the refusal inside
// its background job). handled=false: not managed, or no Env — carry on.
func interceptCentralEnvReplace(w http.ResponseWriter, r *http.Request, dc *dockerClient, name string, body ReplaceServiceRequest, actor string) bool {
	if len(body.Env) == 0 {
		return false
	}
	ce := centralEnvOf(dc)
	if ce == nil {
		return false
	}
	tpl, hasTpl, err := centralEnvServiceTemplate(r.Context(), dc, name)
	if err != nil || !hasTpl || !ce.Managed(name, tpl.Labels) {
		return false
	}
	current := tpl.Image
	if ref, err := dc.inspectConfigImage(r.Context(), tpl.ID); err == nil && ref != "" {
		current = ref
	}
	if body.Image != "" && body.Image != current {
		http.Error(w, "env for centrally-managed services changes via /env; replace the image separately", http.StatusConflict)
		return true
	}
	origin := centralEnvOriginFor(ce, name, tpl.Labels)
	version, err := centralEnvCurrentVersion(r.Context(), ce, name, origin)
	if err != nil {
		writeCentralEnvErr(w, err)
		return true
	}
	req := centralEnvSetRequest{RequestID: newCentralEnvRequestID(), IfVersion: version, Set: body.Env}
	res, err := submitCentralEnvSet(r.Context(), r, ce, name, origin, req, actor)
	if err != nil {
		writeCentralEnvErr(w, err)
		return true
	}
	if !res.NoOp && !res.Replayed {
		audit(r, actor, "service.env_set", fmt.Sprintf("%s v%d keys=%s via %s (from replace)", name, res.Version, strings.Join(res.ChangedKeys, ","), origin))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "env-set", "version": res.Version, "changed_keys": res.ChangedKeys, "origin": origin})
	return true
}

// centralEnvCurrentVersion is the origin's current version for svc — the
// if_version a converted replace compares against. Asked of the origin, not
// read from this host's cache: a cache lagging by one version would turn
// every converted replace into a spurious conflict.
func centralEnvCurrentVersion(ctx context.Context, ce *centralEnv, svc, origin string) (uint64, error) {
	if origin == ce.identity {
		rec, ok, err := ce.store.Get(svc)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, errCentralEnvNotFound
		}
		return rec.Version, nil
	}
	m := ce.sync
	if m.registry == nil || m.secret == "" {
		return 0, errCentralEnvUnavailable{Service: svc, Origin: origin}
	}
	peerURL, ok := m.registry.URLForIdentity(origin)
	if !ok {
		return 0, errCentralEnvUnavailable{Service: svc, Origin: origin}
	}
	ps, _, err := m.peerStatus(ctx, peerURL, svc)
	if err != nil || ps.Role != envSyncRoleOrigin {
		return 0, errCentralEnvUnavailable{Service: svc, Origin: origin}
	}
	return ps.Version, nil
}

// refuseManagedRollingEnv is the peer-mesh rolling-replace's synchronous
// refusal of Env on a managed service (there's no conversion over the mesh:
// the origin's own API is where an env edit belongs).
func refuseManagedRollingEnv(ctx context.Context, dc *dockerClient, name string, body ReplaceServiceRequest) error {
	if len(body.Env) == 0 {
		return nil
	}
	tpl, ok, err := centralEnvServiceTemplate(ctx, dc, name)
	if err != nil || !ok {
		return nil
	}
	return dc.refuseCentrallyManaged(name, tpl.Labels, "env edits must go through its central env, not a replace/stage request")
}

// isCentralEnvErr reports whether err is one writeCentralEnvErr has a
// specific status for.
func isCentralEnvErr(err error) bool {
	var conflict errEnvVersionConflict
	var managed errEnvCentrallyManaged
	var unavailable errCentralEnvUnavailable
	var broken errCentralEnvBroken
	var unknown errCentralEnvOutcomeUnknown
	var relay errCentralEnvRelay
	var failedHere errCentralEnvFailedHere
	var cacheFailed errCentralEnvCacheFailed
	var fwd errCentralEnvPeerForward
	return errors.As(err, &conflict) || errors.As(err, &managed) || errors.As(err, &unavailable) ||
		errors.As(err, &broken) || errors.As(err, &unknown) || errors.As(err, &relay) ||
		errors.As(err, &failedHere) || errors.As(err, &cacheFailed) || errors.As(err, &fwd) ||
		errors.Is(err, errCentralEnvReleasing)
}

// Central per-service env: RELEASE (un-adopt).
//
// Deleting a record is not by itself a way out: replicas stamped
// pmgr.env.origin=<origin> make Resolve fail closed once the record is gone.
// So a release first marks the record "releasing" (creates keep resolving
// from it meanwhile), recreates every stamped replica on every host from
// the current effective env but WITHOUT the stamp — the origin first, then
// each peer, which also drops its cache — and only then deletes the record.
// Any failure leaves the record "releasing"; releasing again resumes.
//
// The same un-stamping roll backs an adopt whose very first origin roll
// failed (unAdopt).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

// peerCentralEnvReleaseResponse is the wire shape of POST
// /peer/central-env/{svc}/release. Unstamped marks a peer that recreates its
// stamped replicas before dropping its cache; a central-env/1-only peer
// answers without it (having dropped the cache and nothing else).
type peerCentralEnvReleaseResponse struct {
	Status    string `json:"status"`
	Unstamped bool   `json:"unstamped"`
}

// requestRelease starts (or resumes) releasing svc, which must already be
// marked releasing in the store.
func (m *envSyncManager) requestRelease(svc string) {
	m.request(svc)
}

// requestPeerRelease is the non-origin side: recreate this host's stamped
// replicas of svc without the stamp, then drop the cache.
func (m *envSyncManager) requestPeerRelease(svc, origin string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A finished earlier release attempt must not be mistaken by the
	// origin's poll for the outcome of this one.
	if j := m.jobs[svc]; j != nil && j.Role == envSyncRoleRelease && envSyncTerminal(j.Status) {
		delete(m.jobs, svc)
	}
	m.peerRelease[svc] = origin
	m.startLocked(svc)
}

// originReleasing asks origin whether svc may be released here: its record
// is marked releasing, or it no longer has one. An error means the origin
// couldn't be asked — the caller must fail closed.
func (m *envSyncManager) originReleasing(ctx context.Context, origin, svc string) (bool, error) {
	if m.registry == nil || m.secret == "" {
		return false, errors.New("peer mesh not configured")
	}
	peerURL, ok := m.registry.URLForIdentity(origin)
	if !ok {
		return false, fmt.Errorf("%s is not a known peer", origin)
	}
	ps, _, err := m.peerStatus(ctx, peerURL, svc)
	if err != nil {
		return false, err
	}
	if ps.Role != envSyncRoleOrigin || ps.Origin != origin {
		return true, nil
	}
	return ps.State == centralEnvStateReleasing, nil
}

func (m *envSyncManager) takePeerRelease(svc string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.peerRelease[svc]
	delete(m.peerRelease, svc)
	return o
}

// forget drops every in-memory per-service marker for a service this host
// no longer has a central env for (jobs and last_failure stay, so the
// outcome remains visible).
func (m *envSyncManager) forget(svc string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peerFailed, svc)
	delete(m.resolved, svc)
	delete(m.retry, svc)
	delete(m.peerTarget, svc)
	delete(m.peerAdopt, svc)
}

// runRelease is the origin's release job (runOrigin dispatches here for a
// releasing record; the job was begun with envSyncRoleRelease).
func (m *envSyncManager) runRelease(ctx context.Context, svc string) {
	if !m.waitIdle(ctx, svc) {
		m.finish(svc, envSyncStatusFailed, "timed out waiting for another rollout to finish — still releasing; release again to resume")
		return
	}
	claimed := true
	defer func() {
		if claimed {
			m.releaseClaim(svc)
		}
	}()
	env, version, err := m.ce.store.effectiveEnvAt(svc, m.ce.identity, m.ce.secrets)
	if err != nil {
		// effectiveEnvAt's errors name keys and secrets, never values.
		m.finish(svc, envSyncStatusFailed, err.Error()+" — still releasing")
		audit(nil, envSyncAuditUser, "service.env_release_failed", fmt.Sprintf("%s v%d: cannot resolve", svc, version))
		return
	}
	m.update(svc, func(j *envSyncJob) { j.Phase = envSyncRoleRelease + ":" + m.ce.identity; j.Target = version })
	audit(nil, envSyncAuditUser, "service.env_release_start", fmt.Sprintf("%s v%d", svc, version))
	warnings, err := m.rollLocal(ctx, svc, env, m.ce.identity, version, nil, envRollMode{unstamp: true})
	m.addWarnings(svc, warnings)
	if err != nil {
		msg := scrubEnvValues(err.Error(), env)
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostFailed, Version: version, Error: msg})
		m.finish(svc, envSyncStatusFailed, msg+" — still releasing; release again to resume")
		audit(nil, envSyncAuditUser, "service.env_release_failed", fmt.Sprintf("%s on %s", svc, m.ce.identity))
		return
	}
	m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostConverged, Version: version})
	m.releaseClaim(svc)
	claimed = false

	if m.releasePeers(ctx, svc, version) {
		m.finish(svc, envSyncStatusPartial, "not every host released — still releasing; release again to resume")
		audit(nil, envSyncAuditUser, "service.env_release_failed", fmt.Sprintf("%s partial", svc))
		return
	}
	if err := m.ce.store.Delete(svc); err != nil {
		m.finish(svc, envSyncStatusFailed, "every replica was unstamped but the record could not be deleted: "+err.Error())
		return
	}
	m.forget(svc)
	m.finish(svc, envSyncStatusConverged, "")
	audit(nil, envSyncAuditUser, "service.env_release", fmt.Sprintf("%s (was v%d)", svc, version))
	log.Printf("central env: %s released — its replicas are per-host again", svc)
}

// releasePeers asks every capable peer that runs or caches svc to release
// it, one at a time, waiting for each. Reports whether any did not.
func (m *envSyncManager) releasePeers(ctx context.Context, svc string, version uint64) (failed bool) {
	if m.secret == "" {
		return false
	}
	for _, p := range m.peers() {
		if ctx.Err() != nil {
			return true
		}
		if !p.capable {
			// Can't have a cache or stamped replicas from this origin.
			continue
		}
		m.update(svc, func(j *envSyncJob) { j.Phase = envSyncRoleRelease + ":" + p.identity })
		r := m.releaseOnePeer(ctx, p, svc, version)
		if r.Status == "" {
			continue
		}
		m.addHost(svc, r)
		if r.Status != envSyncHostConverged {
			failed = true
		}
	}
	return failed
}

func peerReleased(ps peerCentralEnvStatus) bool {
	if ps.Version != 0 {
		return false
	}
	for _, mb := range ps.Members {
		if !mb.Foreign {
			return false
		}
	}
	return true
}

func (m *envSyncManager) releaseOnePeer(ctx context.Context, p envSyncPeer, svc string, version uint64) envSyncHostResult {
	res := envSyncHostResult{Host: p.identity, Version: version}
	ps, code, err := m.peerStatus(ctx, p.url, svc)
	switch {
	case code == http.StatusNotFound:
		return envSyncHostResult{}
	case err != nil:
		res.Status, res.Error = envSyncHostUnreachable, "status: "+err.Error()
		return res
	case ps.Role == envSyncRoleOrigin || peerReleased(ps):
		return envSyncHostResult{}
	case !p.adoptCapable:
		res.Status, res.Error = envSyncHostUnsupported, "runs or caches this service but cannot release it (upgrade its dashboard)"
		return res
	}
	body, _ := json.Marshal(map[string]any{"origin": m.ce.identity, "unstamp": true})
	var out peerCentralEnvReleaseResponse
	code, _, err = peerMutate(ctx, m.client, p.url, m.secret, http.MethodPost, "/peer/central-env/"+url.PathEscape(svc)+"/release", 10*time.Second, bytes.NewReader(body), &out, "")
	switch {
	case err != nil:
		res.Status, res.Error = envSyncHostUnreachable, "release: "+err.Error()
		return res
	case (code == http.StatusOK || code == http.StatusAccepted) && !out.Unstamped:
		res.Status, res.Error = envSyncHostUnsupported, "does not recreate its replicas on release (upgrade its dashboard)"
		return res
	case code == http.StatusOK:
		res.Status = envSyncHostConverged
		return res
	case code != http.StatusAccepted:
		res.Status, res.Error = envSyncHostFailed, fmt.Sprintf("release: status %d", code)
		return res
	}
	deadline := time.Now().Add(envSyncPeerTimeout)
	for {
		select {
		case <-ctx.Done():
			res.Status, res.Error = envSyncHostFailed, "timed out waiting for the peer"
			return res
		case <-time.After(envSyncPeerPollInterval):
		}
		ps, _, err := m.peerStatus(ctx, p.url, svc)
		if err == nil {
			if j := ps.Job; j != nil && j.Role == envSyncRoleRelease && envSyncTerminal(j.Status) {
				if j.Status == envSyncStatusConverged && peerReleased(ps) {
					res.Status = envSyncHostConverged
				} else {
					res.Status, res.Error = envSyncHostFailed, j.LastError
				}
				return res
			}
			if (ps.Job == nil || envSyncTerminal(ps.Job.Status)) && peerReleased(ps) {
				res.Status = envSyncHostConverged
				return res
			}
		}
		if time.Now().After(deadline) {
			res.Status, res.Error = envSyncHostFailed, "timed out waiting for the peer"
			return res
		}
	}
}

// runPeerRelease is the non-origin release job: recreate this host's stamped
// replicas of svc without the stamp, from the env the origin resolves for
// this host (or the cached copy if the origin can't be reached), then drop
// the cache. The cache is never written here — a release must not leave one
// behind.
func (m *envSyncManager) runPeerRelease(svc, origin string) {
	ctx, cancel := context.WithTimeout(context.Background(), rollingOpTimeout)
	defer cancel()
	cached, hasCache := m.ce.cache.Get(svc)
	m.begin(svc, envSyncRoleRelease, cached.Version)
	if !m.waitIdle(ctx, svc) {
		m.finish(svc, envSyncStatusFailed, "timed out waiting for another rollout to finish")
		return
	}
	defer m.releaseClaim(svc)
	var env []string
	var version uint64
	var hc *healthcheckSpec
	if m.ce.fetchFromOrigin != nil {
		if got, err := m.ce.fetchFromOrigin(ctx, origin, svc, m.ce.identity); err == nil {
			env, version, hc = got.Env, got.Version, got.Healthcheck
		}
	}
	if env == nil && hasCache && cached.Origin == origin && cached.usable() {
		env, version, hc = cached.Env, cached.Version, cached.Healthcheck
	}
	if hc == nil && hasCache {
		hc = cached.Healthcheck
	}
	_, members, err := m.localMembers(ctx, svc)
	if err != nil {
		m.finish(svc, envSyncStatusFailed, err.Error())
		return
	}
	if env == nil && !hasForeignOnly(members) {
		m.finish(svc, envSyncStatusFailed, fmt.Sprintf("origin %s unreachable and no usable cached env — cannot recreate the stamped replicas", origin))
		return
	}
	if env != nil {
		m.update(svc, func(j *envSyncJob) { j.Target = version })
		warnings, err := m.rollLocal(ctx, svc, env, origin, version, hc, envRollMode{unstamp: true})
		m.addWarnings(svc, warnings)
		if err != nil {
			msg := scrubEnvValues(err.Error(), env, cached.Env)
			m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostFailed, Version: version, Error: msg})
			m.finish(svc, envSyncStatusFailed, msg)
			audit(nil, envSyncAuditUser, "service.env_release_failed", fmt.Sprintf("%s on %s", svc, m.ce.identity))
			return
		}
	}
	if err := m.ce.cache.Delete(svc); err != nil {
		m.finish(svc, envSyncStatusFailed, "replicas unstamped but the cache could not be dropped: "+err.Error())
		return
	}
	m.forget(svc)
	m.mu.Lock()
	// A propagation queued before the release would re-fetch from the
	// (still releasing) origin and write the cache straight back. A second
	// release request that arrived meanwhile still gets its run.
	if m.peerRelease[svc] == "" {
		m.dirty[svc] = false
	}
	m.mu.Unlock()
	m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostConverged, Version: version})
	m.finish(svc, envSyncStatusConverged, "")
	audit(nil, envSyncAuditUser, "service.env_release", fmt.Sprintf("%s from %s", svc, origin))
}

// hasForeignOnly: nothing here carries a stamp (no live replicas counts).
func hasForeignOnly(members []envMember) bool {
	for _, mb := range members {
		if !mb.Foreign {
			return false
		}
	}
	return true
}

// unAdopt undoes an adopt whose first origin roll failed (runOrigin holds
// the claim). With removeOnGateFailure the replicas it didn't get to are
// untouched and still unstamped, so the record can simply go — unless some
// were already swapped, which are recreated without the stamp first
// (deleting under them would leave them failing closed). If that roll fails
// too the record is left "releasing" for an operator to retry.
func (m *envSyncManager) unAdopt(svc string, env []string, target uint64, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), rollingOpTimeout)
	defer cancel()
	_, members, err := m.localMembers(ctx, svc)
	if err == nil && !hasForeignOnly(members) {
		m.update(svc, func(j *envSyncJob) { j.Phase = "unadopt" })
		var rollErr error
		_, rollErr = m.rollLocal(ctx, svc, env, m.ce.identity, target, nil, envRollMode{unstamp: true})
		err = rollErr
	}
	if err != nil {
		if serr := m.ce.store.SetState(svc, centralEnvStateReleasing); serr != nil {
			log.Printf("central env: %s: could not mark releasing: %v", svc, serr)
		}
		m.finish(svc, envSyncStatusFailed, msg+" — undoing the adopt also failed: "+scrubEnvValues(err.Error(), env)+"; left releasing, release again to finish undoing it")
		audit(nil, envSyncAuditUser, "service.env_adopt_failed", fmt.Sprintf("%s v%d, undo failed — releasing", svc, target))
		return
	}
	if err := m.ce.store.Delete(svc); err != nil {
		m.finish(svc, envSyncStatusFailed, msg+" — the adopt's record could not be removed: "+err.Error())
		return
	}
	m.forget(svc)
	m.finish(svc, envSyncStatusAdoptUndone, msg+" — adopt undone: the record was removed and every replica here is per-host again")
	audit(nil, envSyncAuditUser, "service.env_adopt_failed", fmt.Sprintf("%s v%d on %s, undone", svc, target, m.ce.identity))
	log.Printf("central env: %s: adopt failed on %s and was undone", svc, m.ce.identity)
}

// errCentralEnvReleasing refuses an edit of a record mid-release.
var errCentralEnvReleasing = errors.New("central env is being released — edits are refused until the release finishes (release again to resume it)")

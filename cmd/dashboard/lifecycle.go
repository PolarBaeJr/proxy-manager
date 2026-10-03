// lifecycle: per-replica + per-service stop/start without losing the
// service's identity. `docker stop` keeps the container's image, env,
// labels, and network config — `docker start` brings it back in seconds.
// Stopped containers reserve zero CPU / RAM (only their layer disk).
//
// Auto-onboarding a labeled-managed service into OnboardedStore on its first
// Stop/Start (promoteToOnboarded) is REMOVED as of the onboarding rework:
// stopping a label-managed service no longer needs a legacy OnboardedStore
// record to pick up the full managed-service surface — it already has it,
// since Stage/Promote/Replace/Rollback all operate on label-managed services
// directly (docker.go's stageCanary/promoteCanary/replaceService). And
// assembleGroups (cmd/proxy/router.go) already keeps a stopped container's
// RouteGroup alive as a 503, not a 404, so there's nothing routing-wise that
// needed the snapshot either. See git history for the removed
// promoteToOnboarded if it's ever needed for reference.

package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

// memberDrainSeconds is the proxy.drain grace of the service member with
// the given container ID (the default when it isn't found).
func memberDrainSeconds(svc Service, id string) int {
	for _, m := range svc.Members {
		if m.ID == id {
			return drainSeconds(m.Labels)
		}
	}
	return defaultDrainSeconds
}

// findService loads the named service from listServices. Returns ok=false
// if no service by that name has any containers (i.e. neither labeled nor
// stopped-and-still-labeled).
func findService(ctx context.Context, dc *dockerClient, name string) (Service, bool, error) {
	svcs, err := dc.listServices(ctx)
	if err != nil {
		return Service{}, false, err
	}
	for _, s := range svcs {
		if s.Name == name {
			return s, true, nil
		}
	}
	return Service{}, false, nil
}

// stopServiceMembers stops every non-canary container belonging to a
// service. Canary members are left running so a staged deploy isn't
// silently killed by a "stop service" click on the live half. Returns
// (acted, firstErr): acted counts how many containers we actually
// touched (state was running pre-call), so the caller can distinguish
// "everything was already stopped" (acted=0, err=nil) from real failures.
// Members stop in parallel, each with its proxy.drain grace, detached from
// ctx's cancellation so a disconnecting client can't cut a drain short.
func stopServiceMembers(ctx context.Context, dc *dockerClient, svc Service) (int, error) {
	ctx = context.WithoutCancel(ctx)
	acted := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, m := range svc.MemberSummaries {
		if m.IsCanary || m.State != "running" {
			continue
		}
		acted++
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			err := dc.stopContainerT(ctx, id, memberDrainSeconds(svc, id))
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(m.ID)
	}
	wg.Wait()
	return acted, firstErr
}

// startServiceMembers starts every stopped non-canary container belonging
// to a service.
func startServiceMembers(ctx context.Context, dc *dockerClient, svc Service) (int, error) {
	acted := 0
	var firstErr error
	for _, m := range svc.MemberSummaries {
		if m.IsCanary || m.State == "running" {
			continue
		}
		acted++
		if err := dc.startContainer(ctx, m.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return acted, firstErr
}

// runReplicaRestart serves POST .../replicas/{member}/restart (local API and
// peer mesh alike) for a non-canary member the caller already looked up,
// and returns the mode it ran ("" when it wrote an error). A proxy.overlap
// singleton gets an async overlap restart — a new copy from the same image
// reference, health-gated, then the old one drained — tracked as a
// rolling-replace job (202). Everything else keeps the old stop-then-start
// (200). Once on the overlap path it never falls back to stop/start.
func runReplicaRestart(ctx context.Context, w http.ResponseWriter, dc *dockerClient, onb *OnboardedStore, rom *rollingOpManager, proxyURL string, svc Service, member, id string) string {
	var ct dockerContainer
	for _, m := range svc.Members {
		if m.ID == id {
			ct = m
			break
		}
	}
	running := 0
	for _, m := range svc.MemberSummaries {
		if !m.IsCanary && m.State == "running" {
			running++
		}
	}
	_, onboarded := onb.Get(svc.Name)
	// Overlap only for the one live copy of a singleton: a stopped member has
	// nothing to keep serving, and more than one running isn't a singleton.
	if ct.State != "running" || onboarded || !overlapEnabled(ct.Labels) || running != 1 {
		if err := dc.stopContainerT(context.WithoutCancel(ctx), id, memberDrainSeconds(svc, id)); err != nil {
			httpx.WriteErr(w, err)
			return ""
		}
		if err := dc.startContainer(ctx, id); err != nil {
			httpx.WriteErr(w, fmt.Errorf("stop succeeded but start failed — replica %q of service %q is now STOPPED (no live backend from it), retry with action=start: %w", member, svc.Name, err))
			return ""
		}
		proxyRefresh(proxyURL)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "restarted", "mode": "stop-start", "member": member})
		return "stop-start"
	}
	if h := dc.claims.holder(svc.Name); h != "" {
		http.Error(w, fmt.Sprintf("%q is being recreated by %s right now — retry once it finishes", svc.Name, h), http.StatusConflict)
		return ""
	}
	// Config.Image, not the list's Image: the latter decays to a bare digest
	// once the creating tag is retagged or removed locally.
	ref := ct.Image
	if r, err := dc.inspectConfigImage(ctx, id); err == nil && r != "" && !looksLikeBareDigest(r) {
		ref = r
	}
	if ref == "" || looksLikeBareDigest(ref) {
		http.Error(w, fmt.Sprintf("can't overlap-restart %q: %s has no image reference to recreate from (only %q)", svc.Name, member, ref), http.StatusBadRequest)
		return ""
	}
	st, err := rom.startWith(svc.Name, ReplaceServiceRequest{Image: ref}, rollingOpts{removeOnGateFailure: true, skipPull: true, kind: rollingKindRestart})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return ""
	}
	httpx.WriteJSON(w, http.StatusAccepted, struct {
		*rollingOpState
		Mode    string `json:"mode"`
		Member  string `json:"member"`
		Message string `json:"message"`
	}{st, "overlap", member, fmt.Sprintf("overlap restart of %s started: a new copy is created from %s — the image that reference currently points to on this host, not re-pulled — health-gated, then %s is drained; poll GET /api/services/%s/rolling-replace", member, ref, member, svc.Name)})
	return "overlap"
}

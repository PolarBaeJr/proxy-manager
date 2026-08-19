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

import "context"

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
func stopServiceMembers(ctx context.Context, dc *dockerClient, svc Service) (int, error) {
	acted := 0
	var firstErr error
	for _, m := range svc.MemberSummaries {
		if m.IsCanary || m.State != "running" {
			continue
		}
		acted++
		if err := dc.stopContainer(ctx, m.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
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

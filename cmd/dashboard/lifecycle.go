// lifecycle: per-replica + per-service stop/start without losing the
// service's identity. `docker stop` keeps the container's image, env,
// labels, and network config — `docker start` brings it back in seconds.
// Stopped containers reserve zero CPU / RAM (only their layer disk).
//
// When the user stops a labeled-but-not-onboarded service for the first
// time, we also snapshot its config into OnboardedStore. That promotes
// it to the full managed-service surface (Stage/Promote/Replace/Rollback)
// instead of leaving it as a half-managed compose-only thing.

package main

import (
	"context"
	"time"
)

// promoteToOnboarded snapshots a labeled-managed service into the
// OnboardedStore so it picks up Stage/Promote/Replace/Rollback once the
// user starts managing it via Stop/Start. Best-effort: never errors out
// the calling lifecycle action — auto-onboarding is a nice-to-have, not
// a precondition for stopping a container.
//
// Failure modes handled:
//   - service already onboarded → no-op success
//   - service has zero members (race: every container removed since
//     listServices returned) → return nil, nothing to snapshot
//   - all members are canary → snapshot using the canary as template
//     anyway so the record exists; image is still useful
//   - inspectEnv fails (container exited between list and inspect) →
//     persist record with Env=nil rather than aborting
func promoteToOnboarded(ctx context.Context, dc *dockerClient, onb *OnboardedStore, svc Service) error {
	if _, ok := onb.Get(svc.Name); ok {
		return nil
	}
	if len(svc.Members) == 0 {
		return nil
	}
	tpl := &svc.Members[0]
	for i := range svc.Members {
		if svc.Members[i].Labels[labelCanary] != "true" {
			tpl = &svc.Members[i]
			break
		}
	}
	env, _ := dc.inspectEnv(ctx, tpl.ID) // best-effort; nil is fine
	return onb.Put(OnboardedService{
		Name:           svc.Name,
		Host:           svc.Host,
		Port:           svc.Port,
		Image:          svc.Image,
		Env:            env,
		Labels:         svc.Labels,
		Replicas:       svc.Replicas,
		OriginalRouted: true,
		CreatedAt:      time.Now().Unix(),
	})
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

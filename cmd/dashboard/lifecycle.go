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
	"fmt"
	"time"
)

// promoteToOnboarded snapshots a labeled-managed service into the
// OnboardedStore. No-op if the service is already onboarded. Reads the
// template container's env so the record is complete enough for the
// onboarded clone/scale paths to work later.
func promoteToOnboarded(ctx context.Context, dc *dockerClient, onb *OnboardedStore, svc Service) error {
	if _, ok := onb.Get(svc.Name); ok {
		return nil
	}
	if len(svc.Members) == 0 {
		return fmt.Errorf("service %q has no containers to snapshot", svc.Name)
	}
	// Use the first non-canary member as the template.
	var tpl *dockerContainer
	for i := range svc.Members {
		if svc.Members[i].Labels[labelCanary] != "true" {
			tpl = &svc.Members[i]
			break
		}
	}
	if tpl == nil {
		return fmt.Errorf("service %q has only canary members", svc.Name)
	}
	env, err := dc.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return fmt.Errorf("inspect %s env: %w", svc.Name, err)
	}
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

// findMemberByName locates a single container belonging to the named
// service. Returns (id, found). The match is exact on container name.
func findMemberByName(ctx context.Context, dc *dockerClient, service, member string) (string, bool, error) {
	svc, ok, err := findService(ctx, dc, service)
	if err != nil || !ok {
		return "", false, err
	}
	for _, m := range svc.MemberSummaries {
		if m.Name == member {
			return m.ID, true, nil
		}
	}
	return "", false, nil
}

// stopServiceMembers stops every non-canary container belonging to a
// service. Canary members are left running so a staged deploy isn't
// silently killed by a "stop service" click on the live half.
func stopServiceMembers(ctx context.Context, dc *dockerClient, svc Service) error {
	var firstErr error
	for _, m := range svc.MemberSummaries {
		if m.IsCanary || m.State != "running" {
			continue
		}
		if err := dc.stopContainer(ctx, m.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// startServiceMembers starts every stopped non-canary container belonging
// to a service.
func startServiceMembers(ctx context.Context, dc *dockerClient, svc Service) error {
	var firstErr error
	for _, m := range svc.MemberSummaries {
		if m.IsCanary || m.State == "running" {
			continue
		}
		if err := dc.startContainer(ctx, m.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

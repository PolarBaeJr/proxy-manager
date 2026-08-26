package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// autoUpdater — opt-in unattended updates. After each image-checker cycle it
// walks the services, and for any that opted in (proxy.autoupdate label or the
// per-onboarded-service toggle) with a newer registry digest available, runs
// the same replace path as the dashboard's "Pull update + restart" button.

const autoUpdateMaxFailures = 3 // stop retrying a service after this many consecutive failures

// autoUpdateGap is a var (not const), same seam as docker.go's
// replaceSettleDelay, so tests exercising multiple runOnce cycles don't pay
// the real gap in wall-clock time.
var autoUpdateGap = 2 * time.Second

type autoUpdater struct {
	dc         *dockerClient
	ic         *imageChecker
	onb        *OnboardedStore
	routesPath string
	proxyURL   string
	blocks     *autoUpdateBlockStore
	// rm/rom let runOnce defer (not fail) a label-managed replace for a
	// service that already has a canary rollout or a rolling replace in
	// flight — racing either of those with a plain replaceService call would
	// mutate the same containers out from under it.
	rm  *rolloutManager
	rom *rollingOpManager
	// failures counts consecutive failed auto-updates per service. Only ever
	// touched from the image-checker loop goroutine — no locking needed.
	failures map[string]int
}

func newAutoUpdater(dc *dockerClient, ic *imageChecker, onb *OnboardedStore, routesPath, proxyURL string, blocks *autoUpdateBlockStore, rm *rolloutManager, rom *rollingOpManager) *autoUpdater {
	if rm == nil {
		rm = newRolloutManager(dc, onb, routesPath, proxyURL)
	}
	if rom == nil {
		rom = newRollingOpManager(dc)
	}
	return &autoUpdater{
		dc: dc, ic: ic, onb: onb,
		routesPath: routesPath, proxyURL: proxyURL,
		blocks:   blocks,
		rm:       rm,
		rom:      rom,
		failures: map[string]int{},
	}
}

// autoUpdateBlockStore holds the last-known reason a service's auto-update
// is stuck at the retry cap, keyed by service name — the piece that went
// missing once shouldAutoUpdate's cap silences runOnce's own logging for
// good: once failures >= autoUpdateMaxFailures, shouldAutoUpdate refuses
// forever, so a blocked service can NEVER reach the success branch again to
// clear itself. The only clearing path is the top of runOnce's loop, when
// the registry stops reporting a difference (st == nil || !st.UpdateAvailable)
// — i.e. someone fixes the problem and pulls the image manually. Shared
// between the single autoupdate loop goroutine (the only writer) and
// list_services/buildManagedServices (readers from HTTP handler goroutines),
// the same cross-goroutine sharing pattern imageChecker already uses for ic.
//
// All methods are nil-receiver-safe so call sites (mainly tests) that don't
// care about this feature can pass a nil *autoUpdateBlockStore.
type autoUpdateBlockStore struct {
	mu      sync.Mutex
	blocked map[string]string
}

func newAutoUpdateBlockStore() *autoUpdateBlockStore {
	return &autoUpdateBlockStore{blocked: map[string]string{}}
}

func (s *autoUpdateBlockStore) Set(name, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[name] = reason
}

func (s *autoUpdateBlockStore) Clear(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocked, name)
}

func (s *autoUpdateBlockStore) Get(name string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked[name]
}

// shouldAutoUpdate is the pure gate: opted in, has an image, no canary in
// flight, not fully stopped, checker says a newer digest exists cleanly, and
// we haven't hit the consecutive-failure backoff cap.
func shouldAutoUpdate(svc Service, st *imageStatus, consecutiveFailures int) bool {
	return svc.AutoUpdate &&
		svc.Image != "" &&
		svc.CanaryImage == "" &&
		!svc.AllStopped &&
		st != nil &&
		st.UpdateAvailable &&
		st.Err == "" &&
		consecutiveFailures < autoUpdateMaxFailures
}

// autoUpdateSkipReason explains, for list_services/the dashboard, why a
// service sitting on update_available=true won't actually get picked up by
// the next auto-update tick — the exact question that went unanswered when
// badminton-staging-admin/-player sat with update_available=true and
// auto_update simply never set, with nothing anywhere saying so. Mirrors
// shouldAutoUpdate's gates but can't see the consecutive-failure backoff
// count (that's private state inside autoUpdater, not available to a
// services-list request) — that one case is left unexplained rather than
// guessed at. Empty string means either no update is pending (nothing to
// explain) or shouldAutoUpdate would actually fire.
//
// Also proactively checks inspectHostConfigUnknowns for a service that would
// otherwise fire: prepareReplaceTemplate refuses the whole replace when a
// container carries HostConfig it cannot reproduce on recreate (published
// ports, extra networks, etc — see hostConfigRefuseFields), but runOnce only
// records that as a generic per-cycle failure, so an operator saw nothing
// concrete until autoUpdateMaxFailures consecutive cycles had already been
// burned. This surfaces the same refusal immediately, before any failed
// cycles happen. Best-effort: an inspect error here is swallowed rather than
// surfaced, since a transient inspect failure isn't itself an auto-update
// blocker and runOnce's own path will report a real error if it recurs.
func autoUpdateSkipReason(ctx context.Context, dc *dockerClient, svc Service, st *imageStatus) string {
	if st == nil || !st.UpdateAvailable {
		return ""
	}
	switch {
	case !svc.AutoUpdate:
		return "auto-update is off for this service"
	case svc.Image == "":
		return "no image recorded"
	case svc.CanaryImage != "":
		return "a canary is staged — promote or discard first"
	case svc.AllStopped:
		return "service is fully stopped"
	case st.Err != "":
		return "last registry check failed: " + st.Err
	}
	for _, ct := range liveOnly(svc.Members) {
		unknowns, err := dc.inspectHostConfigUnknowns(ctx, ct.ID)
		if err != nil || len(unknowns) == 0 {
			continue
		}
		return fmt.Sprintf("auto-update would drop %s on recreate — update manually", strings.Join(unknowns, ", "))
	}
	return ""
}

// runOnce is called synchronously after each image-checker cycle (single
// goroutine — cycles never overlap). A human clicking Replace at the same
// moment is unserialized; the engine itself runs one update at a time.
func (a *autoUpdater) runOnce(ctx context.Context) {
	svcs, err := a.dc.listServices(ctx)
	if err != nil {
		log.Printf("autoupdate: list services: %v", err)
		return
	}
	// Merge onboarded records the same way the GET /api/services handler does.
	labeledIdx := map[string]int{}
	for i := range svcs {
		labeledIdx[svcs[i].Name] = i
	}
	for _, o := range a.onb.List() {
		if i, ok := labeledIdx[o.Name]; ok {
			svcs[i].AutoUpdate = svcs[i].AutoUpdate || o.AutoUpdate
			if svcs[i].CanaryImage == "" {
				svcs[i].CanaryImage = o.CanaryImage
			}
			continue
		}
		if o.Host == "" {
			// Managed-only: no route, no replace path.
			continue
		}
		svcs = append(svcs, Service{
			Name:        o.Name,
			Image:       o.Image,
			CanaryImage: o.CanaryImage,
			AutoUpdate:  o.AutoUpdate,
			Onboarded:   true,
		})
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })

	for _, svc := range svcs {
		if ctx.Err() != nil {
			return
		}
		st := a.ic.Get(svc.Image)
		if st == nil || !st.UpdateAvailable {
			delete(a.failures, svc.Name)
			a.blocks.Clear(svc.Name)
			continue
		}
		if !shouldAutoUpdate(svc, st, a.failures[svc.Name]) {
			continue
		}
		_, onboardedSvc := a.onb.Get(svc.Name)
		if !onboardedSvc {
			// A canary rollout or a rolling replace already owns this
			// service's containers — deferring (not failing) here matters:
			// this isn't the service's fault, so it must not burn its
			// failures budget or trip the permanent-block state, either of
			// which would happen if this fell through to the uerr != nil
			// branch below. The next tick tries again once the other job
			// finishes.
			if st, ok := a.rm.get(svc.Name); ok && rolloutActive(st.Status) {
				log.Printf("autoupdate: %s — active rollout in progress, deferring", svc.Name)
				continue
			}
			if st, ok := a.rom.get(svc.Name); ok && rollingOpActive(st.Status) {
				log.Printf("autoupdate: %s — active rolling replace in progress, deferring", svc.Name)
				continue
			}
		}
		log.Printf("autoupdate: %s — newer digest for %s, replacing", svc.Name, svc.Image)
		var uerr error
		if onboardedSvc {
			uerr = a.dc.replaceOnboarded(ctx, svc.Name, ReplaceServiceRequest{Image: svc.Image}, a.onb, a.routesPath)
			if uerr == nil {
				proxyRefresh(a.proxyURL)
			}
		} else {
			uerr = a.dc.replaceService(ctx, svc.Name, ReplaceServiceRequest{Image: svc.Image})
		}
		if uerr != nil {
			a.failures[svc.Name]++
			log.Printf("autoupdate: %s failed (%d/%d): %v", svc.Name, a.failures[svc.Name], autoUpdateMaxFailures, uerr)
			audit(nil, "autoupdate", "service.autoupdate_failed", svc.Name+": "+uerr.Error())
			if a.failures[svc.Name] >= autoUpdateMaxFailures {
				// This is the last thing anyone hears about this service:
				// shouldAutoUpdate's cap check above means runOnce silently
				// skips it on every future tick with no further log line,
				// so without this it looks identical to "resolved" from the
				// outside while staying permanently stale.
				a.blocks.Set(svc.Name, uerr.Error())
			}
		} else {
			delete(a.failures, svc.Name)
			a.blocks.Clear(svc.Name)
			// Re-check immediately so the "update available" flag clears
			// without waiting for the next 10-min poll.
			a.ic.Check(ctx, svc.Image)
			audit(nil, "autoupdate", "service.replace", svc.Name+" => "+svc.Image)
			log.Printf("autoupdate: %s updated to newest %s", svc.Name, svc.Image)
		}
		time.Sleep(autoUpdateGap)
	}
}

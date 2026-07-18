package main

import (
	"context"
	"log"
	"sort"
	"time"
)

// autoUpdater — opt-in unattended updates. After each image-checker cycle it
// walks the services, and for any that opted in (proxy.autoupdate label or the
// per-onboarded-service toggle) with a newer registry digest available, runs
// the same replace path as the dashboard's "Pull update + restart" button.

const (
	autoUpdateMaxFailures = 3 // stop retrying a service after this many consecutive failures
	autoUpdateGap         = 2 * time.Second
)

type autoUpdater struct {
	dc         *dockerClient
	ic         *imageChecker
	onb        *OnboardedStore
	routesPath string
	proxyURL   string
	// failures counts consecutive failed auto-updates per service. Only ever
	// touched from the image-checker loop goroutine — no locking needed.
	failures map[string]int
}

func newAutoUpdater(dc *dockerClient, ic *imageChecker, onb *OnboardedStore, routesPath, proxyURL string) *autoUpdater {
	return &autoUpdater{
		dc: dc, ic: ic, onb: onb,
		routesPath: routesPath, proxyURL: proxyURL,
		failures: map[string]int{},
	}
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
			continue
		}
		if !shouldAutoUpdate(svc, st, a.failures[svc.Name]) {
			continue
		}
		log.Printf("autoupdate: %s — newer digest for %s, replacing", svc.Name, svc.Image)
		var uerr error
		if _, ok := a.onb.Get(svc.Name); ok {
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
		} else {
			delete(a.failures, svc.Name)
			// Re-check immediately so the "update available" flag clears
			// without waiting for the next 10-min poll.
			a.ic.Check(ctx, svc.Image)
			audit(nil, "autoupdate", "service.replace", svc.Name+" => "+svc.Image)
			log.Printf("autoupdate: %s updated to newest %s", svc.Name, svc.Image)
		}
		time.Sleep(autoUpdateGap)
	}
}

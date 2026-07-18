// dashboard: management UI (login + 2FA + service mgmt + DNS).
// Does NOT serve user traffic. The proxy is a separate binary.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":8093", "dashboard listen address")
	metricsAddr := flag.String("metrics-addr", ":8094", "internal metrics endpoint listen address")
	authFile := flag.String("auth", "/data/auth.json", "auth state file (created on first run)")
	auditFile := flag.String("audit", "/data/audit.log", "audit log file path")
	onboardedFile := flag.String("onboarded", "/data/onboarded.json", "onboarded-services state file")
	releasesFile := flag.String("releases", "/data/releases.json", "release marks (stable-tag pins) state file")
	imageHistoryFile := flag.String("image-history", "/data/image-history.json", "per-service image version history state file")
	prefsFile := flag.String("prefs", "/data/prefs.json", "per-user UI preferences state file")
	staticConfig := flag.String("routes-config", "/etc/proxy/routes.json", "static routes file (rw: dashboard appends onboarded routes here)")
	flag.Parse()

	metrics := NewMetrics()
	metricsServer(*metricsAddr, metrics)
	log.Printf("metrics on %s/metrics", *metricsAddr)

	auth, err := loadAuthStore(*authFile)
	if err != nil {
		log.Fatalf("auth store: %v", err)
	}
	if !auth.IsSetup() {
		log.Printf("⚠ auth not yet set up — visit the dashboard to create the first user")
	}

	if err := openAuditLog(*auditFile); err != nil {
		log.Printf("⚠ audit log unavailable: %v", err)
	}

	dc := newDockerClient()

	var cf *cloudflareClient
	if tok := os.Getenv("CLOUDFLARE_API_TOKEN"); tok != "" {
		if zone := os.Getenv("CLOUDFLARE_ZONE_ID"); zone != "" {
			cf = newCloudflareClient(tok, zone, os.Getenv("CLOUDFLARE_DOMAIN"))
			log.Printf("cloudflare integration enabled for zone %s", zone)
		}
	}

	limiter := newRateLimiter()
	ic := newImageChecker(dc)

	onboarded, err := loadOnboardedStore(*onboardedFile)
	if err != nil {
		log.Fatalf("onboarded store: %v", err)
	}

	releases, err := loadReleasesStore(*releasesFile)
	if err != nil {
		log.Fatalf("releases store: %v", err)
	}

	prefs, err := loadPrefsStore(*prefsFile)
	if err != nil {
		log.Fatalf("prefs store: %v", err)
	}

	imageHistory, err := loadImageHistoryStore(*imageHistoryFile)
	if err != nil {
		log.Fatalf("image history store: %v", err)
	}

	pm, err := newPasskeyManager(os.Getenv("PASSKEY_RP_ID"), os.Getenv("PASSKEY_RP_ORIGINS"))
	if err != nil {
		log.Printf("⚠ passkey support disabled: %v", err)
		pm = nil
	} else {
		log.Printf("passkey support enabled (rp_id=%q)", firstNonEmpty(os.Getenv("PASSKEY_RP_ID"), "localhost"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background: poll registries every 10 min for newer image digests, then
	// let the auto-updater act on any opted-in service with a newer digest.
	au := newAutoUpdater(dc, ic, onboarded, *staticConfig, proxyURLFromEnv())
	go ic.Loop(ctx, func() []string {
		svcs, err := dc.listServices(ctx)
		if err != nil {
			return nil
		}
		// Piggyback on the same tick to persist per-service image history —
		// this is what keeps replaced versions findable on the Images panel.
		imageHistory.Record(svcs, onboarded.List())
		var imgs []string
		for _, s := range svcs {
			if s.Image != "" {
				imgs = append(imgs, s.Image)
			}
			if s.CanaryImage != "" {
				imgs = append(imgs, s.CanaryImage)
			}
		}
		// Onboarded-only services aren't label-discovered — include their
		// images so they get update badges (and auto-updates) too.
		for _, o := range onboarded.List() {
			if o.Image != "" {
				imgs = append(imgs, o.Image)
			}
			if o.CanaryImage != "" {
				imgs = append(imgs, o.CanaryImage)
			}
		}
		return imgs
	}, au.runOnce)

	// Background: sample CPU once per second for the header stats widget.
	go statsLoop(ctx)

	mux := newDashboardMux(dc, cf, auth, limiter, ic, *staticConfig, pm, onboarded, releases, prefs, imageHistory)

	log.Printf("dashboard on %s", *addr)
	if err := http.ListenAndServe(*addr, withMetrics(mux, metrics)); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

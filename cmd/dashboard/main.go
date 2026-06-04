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

	pm, err := newPasskeyManager(os.Getenv("PASSKEY_RP_ID"), os.Getenv("PASSKEY_RP_ORIGINS"))
	if err != nil {
		log.Printf("⚠ passkey support disabled: %v", err)
		pm = nil
	} else {
		log.Printf("passkey support enabled (rp_id=%q)", firstNonEmpty(os.Getenv("PASSKEY_RP_ID"), "localhost"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background: poll registries every 10 min for newer image digests.
	go ic.Loop(ctx, func() []string {
		svcs, err := dc.listServices(ctx)
		if err != nil {
			return nil
		}
		var imgs []string
		for _, s := range svcs {
			if s.Image != "" {
				imgs = append(imgs, s.Image)
			}
			if s.CanaryImage != "" {
				imgs = append(imgs, s.CanaryImage)
			}
		}
		return imgs
	})

	// Background: sample CPU once per second for the header stats widget.
	go statsLoop(ctx)

	mux := newDashboardMux(dc, cf, auth, limiter, ic, *staticConfig, pm, onboarded)

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

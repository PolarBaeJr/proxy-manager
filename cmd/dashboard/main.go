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
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// peerWritesEnabled mirrors the -peer-writes flag for the rest of the
// package (peerHandlers construction, future write-side gating) — there's no
// write behavior to gate on it yet in this phase, but future phases need a
// signal to check.
var peerWritesEnabled bool

func main() {
	addr := flag.String("addr", ":8093", "dashboard listen address")
	metricsAddr := flag.String("metrics-addr", ":8094", "internal metrics endpoint listen address")
	mcpAddr := flag.String("mcp-addr", ":8097", "MCP endpoint listen address (proxied at mcp.<domain>/mcp/dashboard)")
	authFile := flag.String("auth", "/data/auth.json", "auth state file (created on first run)")
	auditFile := flag.String("audit", "/data/audit.log", "audit log file path")
	onboardedFile := flag.String("onboarded", "/data/onboarded.json", "onboarded-services state file")
	releasesFile := flag.String("releases", "/data/releases.json", "release marks (stable-tag pins) state file")
	imageHistoryFile := flag.String("image-history", "/data/image-history.json", "per-service image version history state file")
	prefsFile := flag.String("prefs", "/data/prefs.json", "per-user UI preferences state file")
	staticConfig := flag.String("routes-config", "/etc/proxy/routes.json", "static routes file (rw: dashboard appends onboarded routes here)")
	serviceTokenDir := flag.String("service-token-dir", "/tokens", "directory to write auto-provisioned service credentials (e.g. statusbot's token) — a sibling container mounts this read-only")
	redisAddr := flag.String("redis-addr", "", "shared Redis address for cross-peer user identity (passkeys/tokens/passwords), e.g. host:6379 (empty = local-file-only auth, today's behavior)")
	peers := flag.String("peers", "", "comma-separated peer dashboard peer-handshake base URLs, e.g. http://100.83.62.68:8098 (empty = outbound handshake disabled)")
	peerSyncInterval := flag.Duration("peer-sync-interval", 5*time.Second, "how often to handshake with peer dashboards")
	peerAddr := flag.String("peer-addr", ":8098", "internal peer-handshake listen address (only started if DASHBOARD_PEER_SECRET is set)")
	// Separate opt-in from DASHBOARD_PEER_SECRET on purpose: the write mesh
	// changes the blast radius of a leaked/misused peer secret from "data
	// disclosure" (today's read-only mesh) to "arbitrary container mutation
	// on both hosts" — it must not silently activate for every existing
	// deployment the moment someone sets a peer secret.
	peerWrites := flag.Bool("peer-writes", false, "enable write-capable /peer/* handlers on top of the read-only peer mesh (requires DASHBOARD_PEER_SECRET too)")
	flag.Parse()
	peerWritesEnabled = *peerWrites

	metrics := NewMetrics()
	metricsServer(*metricsAddr, metrics)
	log.Printf("metrics on %s/metrics", *metricsAddr)

	auth, err := loadAuthStore(*authFile)
	if err != nil {
		log.Fatalf("auth store: %v", err)
	}
	var redisClient *redis.Client
	if *redisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: *redisAddr, Password: os.Getenv("REDIS_PASSWORD")})
		auth.txRunner = &redisTxRunner{client: redisClient}
		if err := auth.syncFromRedisOrImport(context.Background()); err != nil {
			log.Printf("dashboard auth: redis sync failed at startup, continuing with local users until reachable: %v", err)
		}
		log.Printf("shared user identity enabled via redis at %s", *redisAddr)
	}

	if !auth.IsSetup() {
		log.Printf("⚠ auth not yet set up — visit the dashboard to create the first user")
	}

	if err := provisionServiceToken(auth, *serviceTokenDir, "statusbot"); err != nil {
		log.Printf("⚠ statusbot token provisioning: %v", err)
	}

	if err := openAuditLog(*auditFile); err != nil {
		log.Printf("⚠ audit log unavailable: %v", err)
	}

	for _, m := range initActorSecret(os.Getenv) {
		log.Printf("%s", m)
	}

	dc := newDockerClient()

	// One-shot visibility check: confirm self-identification (isSelfContainer,
	// selfidentity.go) actually matches this process's own container among
	// what's running right now. If a future compose change adds a
	// hostname: override to the dashboard service, os.Hostname() stops
	// returning the container ID and this silently regresses — log it here
	// instead of only discovering it the hard way in the UI. Run in a
	// goroutine with its own bounded timeout — this is diagnostic logging
	// only, so a hung Docker socket (the Pi has a history of exactly that
	// during SSD dropouts) must never delay the HTTP server from binding.
	go func() {
		h, err := selfHostname()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		containers, listErr := dc.listAll(ctx, "")
		switch {
		case listErr != nil:
			log.Printf("dashboard self-identification: hostname=%q — could not list containers to verify: %v", h, listErr)
		default:
			matched := slices.ContainsFunc(containers, isSelfContainer)
			if matched {
				log.Printf("dashboard self-identification: hostname=%q matched container=true", h)
			} else {
				log.Printf("⚠ dashboard self-identification: hostname=%q matched no running container — self-exclusion in /api/services will not work (compose hostname: override?)", h)
			}
		}
	}()

	secrets, secretsMsgs := newSecretsFromEnv(os.Getenv)
	for _, m := range secretsMsgs {
		log.Printf("%s", m)
	}
	if secrets != nil {
		log.Printf("secret refs enabled (ref:NAME env values resolve from %s/<service>.env)", secrets.dir)
	}
	dc.secrets = secrets

	cf, cfMsgs := newCloudflareRegistryFromEnv(os.Getenv)
	for _, m := range cfMsgs {
		log.Printf("%s", m)
	}
	if cf != nil {
		// Discovery (started below) usually adds to this list within a second;
		// what's logged here is only what the env pinned.
		log.Printf("cloudflare integration enabled for zone(s) %s", strings.Join(cf.Domains(), ", "))
	}

	maint, maintMsgs := newMaintFromEnv(os.Getenv)
	for _, m := range maintMsgs {
		log.Printf("%s", m)
	}
	if maint != nil {
		log.Printf("maintenance mode enabled (flag dir %s)", maint.dir)
	}

	maintPages, maintPageMsgs := newMaintPageFromEnv(os.Getenv)
	for _, m := range maintPageMsgs {
		log.Printf("%s", m)
	}
	if maintPages != nil {
		log.Printf("per-app maintenance pages enabled (page dir %s)", maintPages.dir)
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

	// Background: health-gate every service currently mid-rollout and
	// auto-roll-back the moment a canary looks unhealthy between steps.
	rm := newRolloutManager(dc)
	go rm.Run(ctx)

	// Background: sample CPU once per second for the header stats widget.
	go statsLoop(ctx)

	// Background: sample per-container CPU/mem for the Status sub-tab (and
	// later, statusbot) — served from cache, never blocks a live request.
	go dockerStatsLoop(ctx, dc)

	// Background: keep the DNS zone list matching the Cloudflare account, so a
	// newly added domain needs no env edit or restart.
	go cf.SyncLoop(ctx)

	// Background: poll Redis for user identity changes made by a peer
	// instance, so reads (findUser et al) stay in sync without every read
	// going through Redis.
	if auth.txRunner != nil {
		go auth.refreshLoop(ctx)
	}

	// Background: copy each proxy.maintenance app's own 503 page onto disk
	// ahead of time — it has to already be there when the app goes down.
	if maintPages != nil {
		go maintPages.SyncLoop(ctx, dc)
	}

	// Cross-host dashboard peer handshake (Stage 2 of the cross-host control
	// feature). Identity must be distinguishable across instances — unlike
	// cmd/proxy, both dashboard containers share container_name "dashboard",
	// so os.Hostname() alone can't tell them apart. Prefer DASHBOARD_HOST
	// (already set per-instance for WebAuthn), then hostname, then a literal
	// fallback.
	identity := strings.TrimSpace(os.Getenv("DASHBOARD_HOST"))
	if identity == "" {
		if h, err := os.Hostname(); err == nil {
			identity = h
		}
	}
	if identity == "" {
		identity = "dashboard"
		log.Printf("⚠ DASHBOARD_HOST and hostname both unset — peer identity %q is indistinguishable from other dashboard instances", identity)
	}
	// Separate trust boundary from PMGR_PEER_SECRET (proxy mesh) and
	// PMGR_ACTOR_SECRET (audit attribution) — never read, share, or fall back
	// between them.
	peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	peerList := splitAndTrim(*peers)
	registry := newPeerRegistry(peerList, peerSecret, identity, buildVersion, *peerSyncInterval, redisClient)

	mux := newDashboardMux(dc, cf, auth, limiter, ic, *staticConfig, pm, onboarded, releases, prefs, imageHistory, maint, maintPages, registry, rm)

	// MCP on its own port, proxied at mcp.<domain>/mcp/dashboard. Separate from
	// :8093 because that one is loopback-only and serves the UI at "/", which
	// the MCP transport also wants. Tools dispatch back through `mux` in-process
	// so every API handler's guardrails and audit entries still apply.
	//
	// Does NO auth of its own: the proxy's oauth mode gates the path and binds
	// the token to it. Never publish this port.
	if err := mintInternalToken(); err != nil {
		log.Fatalf("mcp: cannot mint internal credential: %v", err)
	}
	mcpSrv := NewServer("proxy-manager-dashboard", "1")
	mcpWrites := isTrue(os.Getenv("MCP_ALLOW_WRITES"))
	mcpPeerWrites := isTrue(os.Getenv("MCP_ALLOW_PEER_WRITES"))
	registerMCPTools(mcpSrv, &apiCaller{mux: mux}, mcpWrites, mcpPeerWrites)
	serveMCP(*mcpAddr, mcpSrv, mcpWrites, mcpPeerWrites)

	if redisClient != nil {
		go registry.ratchetOwnVersion(ctx)
	}
	if len(peerList) > 0 || redisClient != nil {
		go registry.Run(ctx)
	}
	peerHandlers := map[string]http.Handler{
		"/peer/handshake":       peerHandshakeHandler(peerSecret, identity, buildVersion, peerWritesEnabled),
		"/peer/service-status":  peerServiceStatusHandler(peerSecret, identity, dc, proxyURLFromEnv()),
		"/peer/services":        peerServicesHandler(peerSecret, identity, dc, onboarded, ic),
		"/peer/services/":       peerServicesMutateHandler(peerSecret, identity, dc, onboarded, ic, *staticConfig, proxyURLFromEnv(), peerWritesEnabled),
		"/peer/discovery/":      peerDiscoveryMutateHandler(peerSecret, identity, dc, proxyURLFromEnv(), peerWritesEnabled),
		"/peer/images":          peerImagesHandler(peerSecret, identity, dc, releases, imageHistory, onboarded),
		"/peer/images/":         peerImagesMutateHandler(peerSecret, identity, dc, releases, imageHistory, onboarded, peerWritesEnabled),
		"/peer/access":          peerAccessHandler(peerSecret, identity, proxyURLFromEnv()),
		"/peer/logs/containers": peerLogsContainersHandler(peerSecret, identity, dc),
		"/peer/logs/":           peerLogsHandler(peerSecret, identity, dc),
	}
	writesMsg := "(writes disabled)"
	if peerWritesEnabled {
		writesMsg = "(writes enabled)"
	}
	switch {
	case peerSecret != "" && len(peerList) > 0:
		peerServer(*peerAddr, peerHandlers)
		log.Printf("dashboard peers: full mesh — handshaking with %d peer(s) every %s, /peer/handshake, /peer/service-status, /peer/services, /peer/services/, /peer/discovery/, /peer/images, /peer/images/, /peer/access, /peer/logs/containers, and /peer/logs/ on %s %s", len(peerList), *peerSyncInterval, *peerAddr, writesMsg)
	case peerSecret != "":
		peerServer(*peerAddr, peerHandlers)
		log.Printf("dashboard peers: /peer/handshake, /peer/service-status, /peer/services, /peer/services/, /peer/discovery/, /peer/images, /peer/images/, /peer/access, /peer/logs/containers, and /peer/logs/ enabled on %s (receive-only, no outbound peers configured) %s", *peerAddr, writesMsg)
	case len(peerList) > 0:
		log.Printf("dashboard peers: peers configured but DASHBOARD_PEER_SECRET empty — handshake disabled")
	}

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

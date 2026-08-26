package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

func monitorURLFromEnv() string { return os.Getenv("MONITOR_URL") }
func proxyURLFromEnv() string   { return os.Getenv("PROXY_URL") }

// fetchMonitorHealth polls the monitor binary's /api/overview and recomputes
// an overall "up"/"degraded" status from its non-absent targets — shared by
// /api/health (this host's own public health check) and /api/service-status
// (which also stamps a per-host reachability summary onto its Hosts field).
// Always returns a non-nil, possibly-empty targets slice — monitorURL == ""
// (no MONITOR_URL configured), an unreachable monitor, a decode error, and
// an all-targets-absent response all fall through to the same empty-slice
// shape, matching /api/health's pre-existing JSON ("targets": [], never
// null) for every one of those cases.
func fetchMonitorHealth(monitorURL string) (string, []HealthTarget) {
	overall := "up"
	targets := []HealthTarget{}
	if monitorURL == "" {
		return overall, targets
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(monitorURL + "/api/overview")
	if err != nil {
		return "degraded", targets
	}
	defer resp.Body.Close()
	var o struct {
		Health  string `json:"health"`
		Targets []struct {
			Name   string `json:"name"`
			Health string `json:"health"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return overall, targets
	}
	// Recompute overall from non-absent targets so the public health
	// endpoint isn't poisoned by services the user hasn't deployed (e.g.
	// edge with profile off).
	anyDegraded := false
	for _, t := range o.Targets {
		if t.Health == "absent" {
			continue
		}
		targets = append(targets, HealthTarget{Name: t.Name, Health: t.Health})
		if t.Health != "up" {
			anyDegraded = true
		}
	}
	if anyDegraded {
		overall = "degraded"
	}
	return overall, targets
}

func newDashboardMux(dc *dockerClient, cf *cloudflareRegistry, auth *AuthStore, rl *rateLimiter, ic *imageChecker, routesConfigPath string, pm *passkeyManager, onb *OnboardedStore, rs *ReleasesStore, prefs *PrefsStore, ih *ImageHistoryStore, mt *maintStore, mp *maintPageStore, registry *PeerRegistry, rm *rolloutManager) http.Handler {
	if rm == nil {
		rm = newRolloutManager(dc, onb, routesConfigPath, proxyURLFromEnv())
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(renderDashboardHTML()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// Public health endpoint — no auth, sanitized output. Safe to expose to
	// Uptime Kuma / Pingdom / Statuspage / curl scripts. Does NOT leak host names,
	// route details, or traffic counts. Returns only per-binary up/down state.
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		overall, targets := fetchMonitorHealth(monitorURLFromEnv())
		status := http.StatusOK
		if overall != "up" {
			status = http.StatusServiceUnavailable
		}
		httpx.WriteJSON(w, status, map[string]any{
			"status":     overall,
			"targets":    targets,
			"checked_at": time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Host CPU / memory / disk for the header widget.
	mux.HandleFunc("/api/stats", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		if host, isPeer := hostForReq(req, registry); isPeer {
			if registry == nil {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			peerURL, ok := registry.URLForIdentity(host)
			if !ok {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
			if peerSecret == "" {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			client := &http.Client{Timeout: 2 * time.Second}
			var resp peerStatsResp
			if err := peerGET(req.Context(), client, peerURL, peerSecret, "/peer/stats", &resp); err != nil {
				httpx.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			httpx.WriteJSON(w, http.StatusOK, resp.Stats)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, GetStats())
	}))

	// Cross-host dashboard peer identity/version/reachability — advisory
	// display only, never gates any action.
	mux.HandleFunc("/api/peers", auth.requireAuth(peersStatusHandler(registry)))

	// Per-service-group health/usage for the Status sub-tab (and later,
	// statusbot's Discord embed). Combines listServices, the proxy's access
	// log, and the docker-stats cache — see servicestatus.go.
	mux.HandleFunc("/api/service-status", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		status, err := buildServiceStatus(req.Context(), dc, proxyURLFromEnv())
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		// Merge in every configured peer's own service-status (see
		// peers.go's fetchPeerServiceStatus) so one dashboard's /api/
		// service-status — the endpoint statusbot polls — reports the whole
		// mesh, not just this host. Local groups get this host's own
		// identity as Machine too, so every group in the response is
		// consistently labeled, not just the merged-in ones.
		if registry != nil {
			for i := range status.Groups {
				status.Groups[i].Machine = registry.Identity()
			}
			localStatus, localTargets := fetchMonitorHealth(monitorURLFromEnv())
			status.Hosts = append(status.Hosts, HostHealth{
				Machine: registry.Identity(), Reachable: true,
				Status: localStatus, Targets: localTargets,
				CheckedAt: time.Now().UTC().Format(time.RFC3339),
			})
			peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
			peerGroups, peerHosts := fetchPeerServiceStatus(req.Context(), registry, peerSecret)
			status.Groups = append(status.Groups, peerGroups...)
			status.Hosts = append(status.Hosts, peerHosts...)
		}
		httpx.WriteJSON(w, http.StatusOK, status)
	}))

	// ---- Per-user UI preferences (pmgr-* localStorage mirror) ----
	// Deliberately requireAuth (not requireElevated) for writes: prefs are
	// cosmetic per-user state written fire-and-forget on every chip click;
	// requiring elevation would silently drop them whenever it lapses.
	mux.HandleFunc("/api/prefs", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		info := sessionFromReq(auth, req)
		if info == nil {
			http.Error(w, "prefs require a session", http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case "GET":
			httpx.WriteJSON(w, http.StatusOK, prefs.Get(info.Username))
		case "PUT", "POST":
			var kv map[string]string
			if err := json.NewDecoder(req.Body).Decode(&kv); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := prefs.Merge(info.Username, kv); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, prefs.Get(info.Username))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// Proxy through to the monitor binary for traffic metrics. Keeps the auth
	// boundary on the dashboard rather than exposing monitor publicly.
	monitorURL := monitorURLFromEnv()
	if monitorURL != "" {
		fwd := func(suffix string) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) {
				url := monitorURL + suffix
				if q := req.URL.RawQuery; q != "" {
					url += "?" + q
				}
				resp, err := http.Get(url)
				if err != nil {
					http.Error(w, "monitor unreachable", http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.Copy(w, resp.Body)
			}
		}
		mux.HandleFunc("/api/monitor/overview", auth.requireAuth(fwd("/api/overview")))
		mux.HandleFunc("/api/monitor/snapshot", auth.requireAuth(fwd("/api/snapshot")))
		mux.HandleFunc("/api/monitor/series", auth.requireAuth(fwd("/api/series")))
		mux.HandleFunc("/api/monitor/certs", auth.requireAuth(fwd("/api/certs")))

		// Per-target detail endpoints. Path passthrough — /api/monitor/target/proxy
		// hits monitor's /api/target/proxy and so on for /hosts /errors /series.
		mux.HandleFunc("/api/monitor/target/", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
			sub := strings.TrimPrefix(req.URL.Path, "/api/monitor/target/")
			fwd("/api/target/"+sub)(w, req)
		}))
	}

	// ---- Auth (rate-limited where it matters) ----
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, req *http.Request) {
		resp := map[string]any{
			"setup_complete": auth.IsSetup(),
			"authenticated":  false,
			"elevated_until": int64(0),
			"username":       "",
			"now":            time.Now().Unix(),
		}
		if auth.IsSetup() {
			if info, ok := auth.sessionFrom(req); ok {
				resp["authenticated"] = true
				resp["elevated_until"] = info.ElevatedUntil
				resp["username"] = info.Username
			}
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/api/auth/setup", rl.limit(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Username, Password string
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		secret, uri, err := auth.BeginSetup(body.Username, body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{
			"username": body.Username, "totp_secret": secret, "otpauth_uri": uri,
			"qr_data_url": qrDataURL(uri),
		})
	}))

	mux.HandleFunc("/api/auth/setup/confirm", rl.limit(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ Username, Code string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if err := auth.ConfirmPending(body.Username, body.Code); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(req, body.Username, "user.setup_confirmed", body.Username)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "confirmed", "username": body.Username})
	}))

	mux.HandleFunc("/api/auth/login", rl.limit(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Username, Password, Code string
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if !auth.IsSetup() {
			http.Error(w, "auth not set up", http.StatusServiceUnavailable)
			return
		}
		if !auth.VerifyPassword(body.Username, body.Password) {
			audit(req, body.Username, "auth.login_failed", "")
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		// totp_enrolled / code_valid let callers (the SSO login service)
		// enforce 2FA themselves: enrolled user + invalid code must not get
		// an SSO session, even though the password alone opens a (non-
		// elevated) dashboard session.
		enrolled := auth.HasTOTP(body.Username)
		codeValid := body.Code != "" && auth.VerifyTOTP(body.Username, body.Code)
		var elev time.Time
		if codeValid {
			elev = time.Now().Add(elevatedLifetime)
		}
		setSessionCookie(w, auth.newCookie(body.Username, elev))
		audit(req, body.Username, "auth.login_ok", "")
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"username": body.Username, "elevated_until": elev.Unix(),
			"totp_enrolled": enrolled, "code_valid": codeValid,
		})
	}))

	// Used by the SSO portal's passkey login as a fail-closed existence check
	// before minting a cookie: a passkey must not outlive its account. Rate-
	// limited like login. Reveals only a boolean, never account details.
	mux.HandleFunc("/api/auth/user-exists", rl.limit(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ Username string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"exists": auth.Exists(body.Username)})
	}))

	// Used by the proxy's auth gate to resolve a bearer API token (pmt_...)
	// to its owning username. Rate-limited like login — token guessing gets
	// the same treatment as password guessing.
	mux.HandleFunc("/api/auth/verify-token", rl.limit(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ Token string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		u := auth.VerifyToken(body.Token)
		if u == "" {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		audit(req, u, "auth.token_verified", "")
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"username": u})
	}))

	mux.HandleFunc("/api/auth/verify-2fa", rl.limit(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		info, ok := auth.sessionFrom(req)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct{ Code string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if !auth.VerifyTOTP(info.Username, body.Code) {
			audit(req, info.Username, "auth.2fa_failed", "")
			http.Error(w, "invalid code", http.StatusUnauthorized)
			return
		}
		elev := time.Now().Add(elevatedLifetime)
		setSessionCookie(w, auth.newCookie(info.Username, elev))
		audit(req, info.Username, "auth.2fa_ok", "")
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"elevated_until": elev.Unix()})
	}))

	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, req *http.Request) {
		if info, ok := auth.sessionFrom(req); ok {
			audit(req, info.Username, "auth.logout", "")
		}
		clearSessionCookie(w)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
	})

	// ---- Users ----
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
				httpx.WriteJSON(w, http.StatusOK, auth.ListUsers())
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				var body struct{ Username, Password string }
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				secret, uri, err := auth.BeginCreateUser(body.Username, body.Password)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "user.begin_create", body.Username)
				httpx.WriteJSON(w, http.StatusOK, map[string]string{"username": body.Username, "totp_secret": secret, "otpauth_uri": uri})
			})(w, req)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// ---- API tokens (per-user, generated on demand) ----
	mux.HandleFunc("/api/users/tokens", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
				info, _ := auth.sessionFrom(req)
				if info == nil {
					http.Error(w, "tokens listing requires a session", http.StatusUnauthorized)
					return
				}
				httpx.WriteJSON(w, http.StatusOK, auth.ListTokens(info.Username))
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				info, _ := auth.sessionFrom(req)
				if info == nil {
					http.Error(w, "token creation requires a logged-in session (not another token)", http.StatusUnauthorized)
					return
				}
				var body struct {
					Label string `json:"label"`
				}
				_ = json.NewDecoder(req.Body).Decode(&body)
				raw, t, err := auth.CreateToken(info.Username, body.Label)
				if err != nil {
					httpx.WriteErr(w, err)
					return
				}
				audit(req, info.Username, "user.token_create", t.ID)
				httpx.WriteJSON(w, http.StatusOK, map[string]any{
					"token": raw, // shown ONCE; never retrievable again
					"id":    t.ID,
					"label": t.Label,
				})
			})(w, req)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/users/tokens/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "DELETE" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		info, _ := auth.sessionFrom(req)
		if info == nil {
			http.Error(w, "token deletion requires a logged-in session", http.StatusUnauthorized)
			return
		}
		id := strings.TrimPrefix(req.URL.Path, "/api/users/tokens/")
		if err := auth.DeleteToken(info.Username, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(req, info.Username, "user.token_delete", id)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}))

	mux.HandleFunc("/api/users/confirm", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ Username, Code string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if err := auth.ConfirmPending(body.Username, body.Code); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, _ := auth.sessionFrom(req)
		audit(req, sessionUser(info), "user.confirm_create", body.Username)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "confirmed", "username": body.Username})
	}))

	mux.HandleFunc("/api/users/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "DELETE" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(req.URL.Path, "/api/users/")
		info, _ := auth.sessionFrom(req)
		if info != nil && strings.EqualFold(info.Username, name) {
			http.Error(w, "cannot delete yourself", http.StatusBadRequest)
			return
		}
		if err := auth.DeleteUser(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(req, sessionUser(info), "user.delete", name)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}))

	// ---- Routes (view via dashboard's own docker discovery; no dep on proxy) ----
	mux.HandleFunc("/api/routes", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		routes, err := dc.listRoutes(req.Context(), routesConfigPath)
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		// Match the proxy's old JSON shape so the UI doesn't care which it talks to.
		type uiBackend struct {
			URL       string `json:"url"`
			Weight    int    `json:"weight"`
			Container string `json:"container"`
			Healthy   *bool  `json:"healthy,omitempty"`
			LastErr   string `json:"last_error,omitempty"`
		}
		type uiGroup struct {
			Host     string      `json:"host"`
			Path     string      `json:"path,omitempty"`
			Strip    bool        `json:"strip,omitempty"`
			Name     string      `json:"name,omitempty"`
			Service  string      `json:"service,omitempty"`
			Backends []uiBackend `json:"backends"`
		}
		out := make([]uiGroup, 0, len(routes))
		for _, r := range routes {
			bs := make([]uiBackend, 0, len(r.Backends))
			for _, b := range r.Backends {
				bs = append(bs, uiBackend{URL: b.URL, Weight: b.Weight, Container: b.Container})
			}
			out = append(out, uiGroup{Host: r.Host, Path: r.Path, Strip: r.Strip, Name: r.Name, Service: r.Service, Backends: bs})
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}))

	// ---- Services ----
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
				svcs, err := buildManagedServices(req.Context(), dc, onb, ic)
				if err != nil {
					httpx.WriteErr(w, err)
					return
				}
				// Merge in every configured peer's own managed services (see
				// peers.go's fetchPeerServices) so one dashboard's
				// /api/services reports the whole mesh, not just this host.
				// Local services get this host's own identity as Machine
				// too, so every service in the response is consistently
				// labeled, not just the merged-in ones.
				if registry != nil {
					for i := range svcs {
						svcs[i].Machine = registry.Identity()
					}
					peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
					svcs = append(svcs, fetchPeerServices(req.Context(), registry, peerSecret)...)
				}
				httpx.WriteJSON(w, http.StatusOK, svcs)
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				var body CreateServiceRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				if err := dc.createService(req.Context(), body); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "service.create", body.Name)
				httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "created"})
			})(w, req)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/services/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		rest := strings.TrimPrefix(req.URL.Path, "/api/services/")
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		if name == "" {
			http.NotFound(w, req)
			return
		}
		// Forwarding must be checked BEFORE the self-guard below: a local
		// service that happens to share a name with an unrelated peer
		// service (a common case — see buildManagedServices) must never be
		// judged by the LOCAL self-guard when the mutation targets a peer.
		// The peer's own peerServicesMutateHandler runs its own self-guard
		// against its own Docker state.
		if host, isPeer := hostForReq(req, registry); isPeer {
			actor := sessionUser(sessionFromReq(auth, req))
			if actor == "" {
				// Same fallback as /api/images/'s forwarding branch (see the
				// comment there): a token-authenticated call has no session
				// cookie, so without this the forwarded assertion would
				// always be empty for exactly the callers that most need
				// correct attribution.
				actor = principalFrom(req)
			}
			forwardServiceMutation(w, req, host, registry, name, parts, actor)
			return
		}
		// Centralized guard, before any subpath dispatch (scale/replace/
		// stop/start/canary/etc.) — refuses ANY mutating action targeting
		// the dashboard's own service. Lives here rather than per-branch so
		// nothing new added below can accidentally skip it. This also
		// covers MCP write tools for free, since they dispatch through this
		// same mux/handler in-process (see main.go's registerMCPTools).
		// On a Docker error (err != nil) this falls through without
		// blocking — err is deliberately not treated as "assume self".
		if self, err := dc.serviceContainsSelfByName(req.Context(), name); err == nil && self {
			http.Error(w, "refusing to manage the dashboard's own service from within itself — use docker compose on the host", http.StatusForbidden)
			return
		}
		info, _ := auth.sessionFrom(req)
		// A service actively mid-rollout must only be touched via the
		// rollout endpoints themselves — any other mutation (stage/promote/
		// canary/scale/replicas/offboard/delete/etc.) racing the rollout
		// manager's own container manipulation could leave both in an
		// inconsistent state. This also closes a pre-existing gap for
		// label-managed services: before this guard, nothing stopped
		// stageCanary/promoteCanary/discardCanary/scaleService from racing
		// an in-progress rollout, for either substrate.
		// "check" is exempt too: it only queries the registry for image-
		// update availability (runServiceCheckImage) and never touches
		// containers, so it can't race the rollout manager.
		exemptFromRolloutGuard := len(parts) == 2 && (parts[1] == "rollout" || parts[1] == "rollout/advance" || parts[1] == "rollout/abort" || parts[1] == "check" || parts[1] == "duplicate")
		if !exemptFromRolloutGuard {
			if st, ok := rm.get(name); ok && rolloutActive(st.Status) {
				http.Error(w, fmt.Sprintf("%q has an active rollout — advance or abort it before other mutations", name), http.StatusConflict)
				return
			}
		}
		if len(parts) == 2 && parts[1] == "scale" && req.Method == "POST" {
			var body struct{ Replicas int }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := runServiceScale(req.Context(), dc, onb, routesConfigPath, proxyURLFromEnv(), name, body.Replicas); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.scale", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "scaled", "replicas": body.Replicas})
			return
		}
		if len(parts) == 2 && parts[1] == "replace" && req.Method == "POST" {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if _, ok := onb.Get(name); ok {
				if err := dc.replaceOnboarded(req.Context(), name, body, onb, routesConfigPath); err != nil {
					writeServiceErr(w, err)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.replaceService(req.Context(), name, body); err != nil {
				writeServiceErr(w, err)
				return
			}
			// Re-check the image-checker immediately so the "update available"
			// badge clears without waiting for the next 10 min poll. The pull
			// during Replace updated the local digest; comparing now will say
			// local == registry → flag flips off on the next list-services call.
			ic.Check(req.Context(), body.Image)
			audit(req, sessionUser(info), "service.replace", name+" => "+body.Image)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "replaced", "image": body.Image})
			return
		}
		if len(parts) == 2 && parts[1] == "autoupdate" && req.Method == "POST" {
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := runServiceAutoUpdateSet(req.Context(), dc, onb, name, body.Enabled); err != nil {
				writeAutoUpdateErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.autoupdate_set", name+" => "+strconv.FormatBool(body.Enabled))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": body.Enabled})
			return
		}
		if len(parts) == 2 && parts[1] == "singleton" && req.Method == "POST" {
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if _, ok := onb.Get(name); ok {
				http.Error(w, "singleton toggle is only supported for label-managed services", http.StatusBadRequest)
				return
			}
			if err := dc.setUnscalableLabel(req.Context(), name, body.Enabled); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.singleton_set", name+" => "+strconv.FormatBool(body.Enabled))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": body.Enabled})
			return
		}
		if len(parts) == 2 && parts[1] == "check" && req.Method == "POST" {
			payload, status, err := runServiceCheckImage(req.Context(), dc, ic, onb, name)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if status == http.StatusNotFound {
				http.Error(w, "service not found", http.StatusNotFound)
				return
			}
			audit(req, sessionUser(info), "service.check_image", name)
			if status != http.StatusOK {
				msg, _ := payload.(string)
				if msg == "" {
					msg = "check failed"
				}
				http.Error(w, msg, status)
				return
			}
			httpx.WriteJSON(w, status, payload)
			return
		}
		// "duplicate" is NOT routed through the ?host= forwarding branch
		// above (there is no case for it in forwardServiceMutation) and must
		// stay that way: duplicating needs THIS host to read the real
		// service's live env/mounts server-side (which can include secrets)
		// rather than relay the browser's raw request body to a peer — see
		// duplicate.go's package doc comment. runServiceDuplicate calls
		// peerMutate directly instead.
		if len(parts) == 2 && parts[1] == "duplicate" && req.Method == "POST" {
			var body DuplicateServiceRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			actor := sessionUser(info)
			if actor == "" {
				actor = principalFrom(req)
			}
			resp, err := runServiceDuplicate(req.Context(), dc, registry, onb, routesConfigPath, name, body, mintForwardedActor(req, actor))
			if err != nil {
				var pe *peerDuplicateError
				switch {
				case errors.As(err, &pe):
					mapPeerMutationErr(w, pe.statusCode, pe.body)
				case errors.Is(err, errDuplicateNotFound):
					http.Error(w, "service not found", http.StatusNotFound)
				default:
					http.Error(w, err.Error(), http.StatusBadRequest)
				}
				return
			}
			audit(req, sessionUser(info), "service.duplicate", name+" => "+body.Target)
			httpx.WriteJSON(w, http.StatusOK, resp)
			return
		}
		if len(parts) == 2 && parts[1] == "stage" && req.Method == "POST" {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if _, ok := onb.Get(name); ok {
				if err := dc.stageOnboarded(req.Context(), name, body, onb, routesConfigPath); err != nil {
					writeServiceErr(w, err)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.stageCanary(req.Context(), name, body); err != nil {
				writeServiceErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.stage", name+" => "+body.Image)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "staged"})
			return
		}
		if len(parts) == 2 && parts[1] == "promote" && req.Method == "POST" {
			if _, ok := onb.Get(name); ok {
				if err := dc.promoteOnboarded(req.Context(), name, onb, routesConfigPath); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.promoteCanary(req.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.promote", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "promoted"})
			return
		}
		if len(parts) == 2 && parts[1] == "canary" && req.Method == "DELETE" {
			if _, ok := onb.Get(name); ok {
				if err := dc.discardOnboarded(req.Context(), name, onb, routesConfigPath); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.discardCanary(req.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.discard_canary", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
			return
		}
		if len(parts) == 2 && parts[1] == "rollout" && req.Method == "POST" {
			var body RolloutRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			st, err := rm.startRollout(req.Context(), name, ReplaceServiceRequest{Image: body.Image, Env: body.Env, EnvAck: body.EnvAck}, body.Steps)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.rollout_start", name+" => "+body.Image)
			httpx.WriteJSON(w, http.StatusOK, st)
			return
		}
		if len(parts) == 2 && parts[1] == "rollout" && req.Method == "GET" {
			st, ok := rm.get(name)
			if !ok {
				http.Error(w, "no active rollout for "+name, http.StatusNotFound)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, st)
			return
		}
		if len(parts) == 2 && parts[1] == "rollout/advance" && req.Method == "POST" {
			st, err := rm.advanceRollout(req.Context(), name)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.rollout_advance", name)
			httpx.WriteJSON(w, http.StatusOK, st)
			return
		}
		if len(parts) == 2 && parts[1] == "rollout/abort" && req.Method == "POST" {
			st, err := rm.abortRollout(req.Context(), name)
			if err != nil {
				writeServiceErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.rollout_abort", name)
			httpx.WriteJSON(w, http.StatusOK, st)
			return
		}
		// ---- Stop / Start (per-service or per-replica) ----
		// Stopping retains all container config — `docker start` brings it
		// back instantly.
		if len(parts) == 2 && (parts[1] == "stop" || parts[1] == "start") && req.Method == "POST" {
			svc, ok, err := findService(req.Context(), dc, name)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if !ok {
				http.Error(w, "service not found", http.StatusNotFound)
				return
			}
			act := parts[1]
			acted, err := runServiceLifecycle(req.Context(), dc, proxyURLFromEnv(), svc, act)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service."+act, name)
			msg := act + "ped"
			if acted == 0 {
				msg = "already-" + act + "ped"
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": msg, "members_acted": acted})
			return
		}
		if len(parts) == 2 && strings.HasPrefix(parts[1], "replicas/") && req.Method == "POST" {
			sub := strings.TrimPrefix(parts[1], "replicas/")
			memberParts := strings.SplitN(sub, "/", 2)
			if len(memberParts) != 2 || (memberParts[1] != "stop" && memberParts[1] != "start") {
				http.NotFound(w, req)
				return
			}
			member, act := memberParts[0], memberParts[1]
			svc, ok, err := findService(req.Context(), dc, name)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if !ok {
				http.Error(w, "service not found", http.StatusNotFound)
				return
			}
			var targetID string
			var targetIsCanary bool
			for _, m := range svc.MemberSummaries {
				if m.Name == member {
					targetID = m.ID
					targetIsCanary = m.IsCanary
					break
				}
			}
			if targetID == "" {
				http.Error(w, "replica not found", http.StatusNotFound)
				return
			}
			if targetIsCanary {
				http.Error(w, "canary replicas can't be stopped here — use Discard or Promote", http.StatusConflict)
				return
			}
			if act == "stop" {
				err = dc.stopContainer(req.Context(), targetID)
			} else {
				err = dc.startContainer(req.Context(), targetID)
			}
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			proxyRefresh(proxyURLFromEnv())
			audit(req, sessionUser(info), "service.replica_"+act, name+"/"+member)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": act + "ped", "member": member})
			return
		}
		// ---- Offboard: stop routing a service without destroying its
		// container(s) ----
		// Primary/documented path for ANY label-managed service: disconnect
		// its container(s) from the edge network, leaving them running with
		// their proxy.* labels intact — reconnect (or `docker compose up
		// -d`) to resume routing. A service still tracked in the legacy
		// OnboardedStore keeps its old teardown-the-clones-and-drop-the-
		// route behavior instead, so existing onboarded entries don't break.
		if len(parts) == 2 && parts[1] == "offboard" && req.Method == "POST" {
			_, wasOnboarded := onb.Get(name)
			if err := runServiceOffboard(req.Context(), dc, onb, routesConfigPath, name); err != nil {
				writeServiceRemovalErr(w, err, !wasOnboarded)
				return
			}
			proxyRefresh(proxyURLFromEnv())
			audit(req, sessionUser(info), "service.offboard", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "offboarded"})
			return
		}
		if req.Method == "DELETE" {
			// Optional confirmation body: {"confirm": "<name>"}. Absent or
			// empty preserves the pre-existing no-body-required behavior
			// (not a breaking change for existing direct-API/MCP callers);
			// present, it must equal name or the request is rejected before
			// any Docker mutation. forwardServiceMutation always sends this
			// field (constructed server-side, never trusted from the
			// incoming request) so cross-host deletes get the same guard.
			data, err := io.ReadAll(req.Body)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if len(bytes.TrimSpace(data)) > 0 {
				var confirmBody struct {
					Confirm string `json:"confirm"`
				}
				if err := json.Unmarshal(data, &confirmBody); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				if confirmBody.Confirm != "" && confirmBody.Confirm != name {
					http.Error(w, "confirmation does not match service name", http.StatusBadRequest)
					return
				}
			}
			// Onboarded services: tear down the clones + route, leave the
			// original container alone (the user started it) — DELETE on an
			// onboarded service has always meant "offboard", not "destroy".
			_, wasOnboarded := onb.Get(name)
			membersActed, err := runServiceDelete(req.Context(), dc, onb, routesConfigPath, name)
			if err != nil {
				if wasOnboarded {
					writeServiceRemovalErr(w, err, false)
				} else {
					writeServiceDeleteErr(w, membersActed, err)
				}
				return
			}
			if wasOnboarded {
				proxyRefresh(proxyURLFromEnv())
				audit(req, sessionUser(info), "service.offboard", name)
				httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "offboarded"})
				return
			}
			audit(req, sessionUser(info), "service.delete", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "deleted", "members_acted": membersActed})
			return
		}
		http.NotFound(w, req)
	}))

	// ---- Cloudflare ----
	mux.HandleFunc("/api/cf/enabled", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		// Pure local-config read (the UI polls this every 5s) — "domain" stays
		// the default zone so pre-multi-zone readers keep working.
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": cf.Usable(), "domain": cfDomain(cf), "zones": cf.Status()})
	}))

	// ---- Container logs (read-only; auth-gated) ----
	registerLogRoutes(mux, dc, auth, registry)

	// ---- Discovery: list containers NOT routed by the proxy (auth-gated) ----
	registerDiscoveryRoutes(mux, dc, auth, onb, routesConfigPath)

	// ---- Maintenance mode: per-host nginx 503 flag files (elevated) ----
	registerMaintenanceRoutes(mux, dc, auth, mt, mp, onb, routesConfigPath)

	// ---- Onboarding: one-click adopt an unlabelled container as a service ----
	mux.HandleFunc("/api/discovery/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		rest := strings.TrimPrefix(req.URL.Path, "/api/discovery/")
		info, _ := auth.sessionFrom(req)
		user := sessionUser(info)
		// Batch onboarding: adopt every unmanaged container of a compose project
		// as managed-only (no route, no edge, no env). Idempotent.
		if rest == "batch-onboard" && req.Method == "POST" {
			var body struct {
				Project string `json:"project"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if body.Project == "" || !validServiceName(body.Project) {
				http.Error(w, "invalid project", http.StatusBadRequest)
				return
			}
			items, err := dc.listUnmanaged(req.Context(), nil)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			targets, skipped := batchOnboardTargets(items, body.Project, func(n string) bool {
				_, ok := onb.Get(n)
				return ok
			})
			onboarded := []string{}
			failed := []map[string]string{}
			for _, name := range targets {
				if err := onboardManagedOnly(req.Context(), name, dc, onb); err != nil {
					failed = append(failed, map[string]string{"name": name, "error": err.Error()})
					continue
				}
				onboarded = append(onboarded, name)
				audit(req, user, "service.onboard", name)
			}
			audit(req, user, "service.onboard_batch", fmt.Sprintf("%s onboarded=%d skipped=%d failed=%d", body.Project, len(onboarded), len(skipped), len(failed)))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"project":   body.Project,
				"onboarded": onboarded,
				"skipped":   skipped,
				"failed":    failed,
			})
			return
		}
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[1] != "onboard" || req.Method != "POST" {
			http.NotFound(w, req)
			return
		}
		name := parts[0]
		if host, isPeer := hostForReq(req, registry); isPeer {
			actor := sessionUser(sessionFromReq(auth, req))
			if actor == "" {
				actor = principalFrom(req)
			}
			forwardOnboardMutation(w, req, host, registry, name, actor)
			return
		}
		var body OnboardRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if err := dc.onboardContainer(req.Context(), name, body); err != nil {
			writeOnboardErr(w, err)
			return
		}
		proxyRefresh(proxyURLFromEnv())
		audit(req, user, "service.onboard", name)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "onboarded", "name": name})
	}))

	// ---- Passkeys / WebAuthn (when PASSKEY_RP_ID is set or default localhost) ----
	registerPasskeyRoutes(mux, auth, pm, rl)

	// ---- Proxy access log (read-only; auth-gated). Always registered (not
	// gated on PROXY_URL like /api/ratelimit below) — an instance with no local
	// proxy configured must still be able to view a PEER's access log via
	// ?host=. Local viewing on such an instance returns 503, not a missing
	// route. ----
	mux.HandleFunc("/api/access", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		host, isPeer := hostForReq(req, registry)
		if isPeer && registry != nil {
			peerBase, ok := registry.URLForIdentity(host)
			if !ok {
				http.Error(w, "unknown host: "+host, http.StatusNotFound)
				return
			}
			peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
			if peerSecret == "" {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			forwardAccessLogToPeer(req.Context(), w, peerBase, peerSecret, req.URL.RawQuery)
			return
		}
		px := proxyURLFromEnv()
		if px == "" {
			http.Error(w, "no local proxy configured on this host", http.StatusServiceUnavailable)
			return
		}
		forwardAccessLog(req.Context(), w, px, req.URL.RawQuery)
	}))

	// ---- Rate limits ----
	if px := proxyURLFromEnv(); px != "" {
		// Rate-limit bucket state (read-only; auth-gated). Empty {"routes":[]}
		// whenever no route has proxy.ratelimit=true, whether the proxy's own
		// rate limiting is in-memory-only or Redis-backed — the dashboard
		// doesn't need to know which.
		mux.HandleFunc("/api/ratelimit", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
			resp, err := http.Get(px + "/ratelimit")
			if err != nil {
				http.Error(w, "proxy unreachable", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.Copy(w, resp.Body)
		}))
	}

	mux.HandleFunc("/api/cf/records", func(w http.ResponseWriter, req *http.Request) {
		if cf == nil {
			http.Error(w, "cloudflare not configured", http.StatusServiceUnavailable)
			return
		}
		// Zone resolution happens INSIDE the auth closures — doing it out here
		// would make ?zone= an unauthenticated zone-enumeration oracle.
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
				zc, domain, ok := cfZoneFromReq(cf, req)
				if !ok {
					http.Error(w, "unknown zone", http.StatusNotFound)
					return
				}
				recs, err := zc.List(req.Context())
				cf.noteResult(domain, err)
				if err != nil {
					writeCFErr(w, domain, err)
					return
				}
				httpx.WriteJSON(w, http.StatusOK, recs)
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				zc, domain, ok := cfZoneFromReq(cf, req)
				if !ok {
					http.Error(w, "unknown zone", http.StatusNotFound)
					return
				}
				var body CreateDNSRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				if err := zc.validateName(body.Name); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				rec, err := zc.Create(req.Context(), body)
				cf.noteResult(domain, err)
				if err != nil {
					writeCFErr(w, domain, err)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "dns.create", body.Name+" ("+domain+")")
				httpx.WriteJSON(w, http.StatusOK, rec)
			})(w, req)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/cf/records/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		if cf == nil {
			http.Error(w, "cloudflare not configured", http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimPrefix(req.URL.Path, "/api/cf/records/")
		if !validCFRecordID(id) {
			http.NotFound(w, req)
			return
		}
		zc, domain, ok := cfZoneFromReq(cf, req)
		if !ok {
			http.Error(w, "unknown zone", http.StatusNotFound)
			return
		}
		info, _ := auth.sessionFrom(req)
		switch req.Method {
		case "PATCH":
			var body UpdateDNSRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if body.Name != nil {
				if err := zc.validateName(*body.Name); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			rec, err := zc.Update(req.Context(), id, body)
			cf.noteResult(domain, err)
			if err != nil {
				writeCFErr(w, domain, err)
				return
			}
			audit(req, sessionUser(info), "dns.update", id+" ("+domain+")")
			httpx.WriteJSON(w, http.StatusOK, rec)
		case "DELETE":
			err := zc.Delete(req.Context(), id)
			cf.noteResult(domain, err)
			if err != nil {
				writeCFErr(w, domain, err)
				return
			}
			audit(req, sessionUser(info), "dns.delete", id+" ("+domain+")")
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// ---- Releases / version history (per infra service: dashboard/proxy/edge/monitor) ----
	// GET  /api/releases               → list all infra services with current+stable+ghcr tags
	// GET  /api/releases/{svc}         → single service detail
	// POST /api/releases/{svc}/mark    → body {"tag":"...","label":"..."} — mark a tag stable
	// DELETE /api/releases/{svc}/mark/{tag} → unmark
	mux.HandleFunc("/api/releases", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		out := []*releaseInfoResp{}
		for _, name := range infraServices {
			info, err := buildReleaseInfo(req.Context(), dc, rs, name)
			if err != nil {
				// Skip silently — container may not exist (edge with profile off).
				continue
			}
			out = append(out, info)
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("/api/releases/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		sub := strings.TrimPrefix(req.URL.Path, "/api/releases/")
		parts := strings.Split(sub, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, "service required", http.StatusBadRequest)
			return
		}
		svc := parts[0]
		// Validate against the known infra list to keep path-walking attackers out.
		known := false
		for _, n := range infraServices {
			if n == svc {
				known = true
				break
			}
		}
		if !known {
			http.Error(w, "unknown service", http.StatusNotFound)
			return
		}
		switch {
		case len(parts) == 1 && req.Method == "GET":
			info, err := buildReleaseInfo(req.Context(), dc, rs, svc)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, info)
		case len(parts) == 2 && parts[1] == "mark" && req.Method == "POST":
			var body struct{ Tag, Label string }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			info, err := buildReleaseInfo(req.Context(), dc, rs, svc)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := rs.Mark(info.ImageBase, body.Tag, body.Label, sessionUser(sessionFromReq(auth, req))); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(sessionFromReq(auth, req)), "release.mark", svc+":"+body.Tag)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "marked", "tag": body.Tag})
		case len(parts) == 3 && parts[1] == "mark" && req.Method == "DELETE":
			tag := parts[2]
			info, err := buildReleaseInfo(req.Context(), dc, rs, svc)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := rs.Unmark(info.ImageBase, tag); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(sessionFromReq(auth, req)), "release.unmark", svc+":"+tag)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "unmarked", "tag": tag})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))

	// ---- Images / phase-out (per managed service; LOCAL images only) ----
	// GET    /api/images         → per-service local image inventory + reclaimable bytes
	// POST   /api/images/mark    → body {"service","tag","label"} — mark a tag stable (protected)
	// DELETE /api/images/mark    → body {"service","tag"} — unmark
	// DELETE /api/images/delete  → body {"token"} — delete one local image (token in the
	//                              body, not the path: refs contain / and :)
	// POST   /api/images/prune   → body {"service","keep_n"} — keep stable+running+last N,
	//                              delete the rest (empty service = all)
	mux.HandleFunc("/api/images", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		host, isPeer := hostForReq(req, registry)
		if isPeer {
			if registry == nil {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			peerURL, ok := registry.URLForIdentity(host)
			if !ok {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
			if peerSecret == "" {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			client := &http.Client{Timeout: 2 * time.Second}
			var resp peerImagesResp
			if err := peerGET(req.Context(), client, peerURL, peerSecret, "/peer/images", &resp); err != nil {
				httpx.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			if resp.Images == nil {
				httpx.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "peer returned no data"})
				return
			}
			machine := resp.Identity
			if machine == "" {
				machine = host
			}
			resp.Images.Machine = machine
			// SECURITY: never let a mutating token minted on another host's disk
			// cross back over as something this UI could hand to the LOCAL
			// mutating endpoints. DeleteToken is the only field that round-trips
			// as an actionable value — zero it on every entry so a peer's image
			// ref/ID can never reach /api/images/delete, even if a future
			// frontend change accidentally emits a delete button for a peer row.
			// This Go-side stripping is the actual security boundary; the
			// frontend gating below is defense-in-depth on top of it, not the
			// primary control. Do not remove this as "dead code" — a future
			// frontend bug depends on it staying here.
			for i := range resp.Images.Services {
				for j := range resp.Images.Services[i].Entries {
					resp.Images.Services[i].Entries[j].DeleteToken = ""
				}
			}
			httpx.WriteJSON(w, http.StatusOK, resp.Images)
			return
		}
		svcs, err := dc.listServices(req.Context())
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		info, err := buildImagesInfo(req.Context(), dc, rs, ih, svcs, onb.List())
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if registry != nil {
			info.Machine = registry.Identity()
		}
		httpx.WriteJSON(w, http.StatusOK, info)
	}))
	mux.HandleFunc("/api/images/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		actor := sessionUser(sessionFromReq(auth, req))
		sub := strings.TrimPrefix(req.URL.Path, "/api/images/")
		if host, isPeer := hostForReq(req, registry); isPeer {
			fwdActor := actor
			if fwdActor == "" {
				// Same fallback audit() applies internally (see audit.go): a
				// token-authenticated call — MCP, most of all — has no
				// session cookie for sessionFromReq to read, so without this
				// the forwarded assertion would always be empty for exactly
				// the callers actor-forwarding exists for. Scoped to the
				// forwarding branch only — the local mutation path below
				// keeps its existing (unfallen-back) actor value unchanged.
				fwdActor = principalFrom(req)
			}
			forwardImageMutation(w, req, host, registry, sub, fwdActor)
			return
		}
		// Resolve a managed service's image base for mark/unmark (same base
		// keying the ReleasesStore uses for infra services).
		resolveBase := func(service string) (string, bool) {
			return resolveImageBase(req.Context(), dc, onb, service)
		}
		switch {
		case sub == "mark" && req.Method == "POST":
			var body struct{ Service, Tag, Label string }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			base, ok := resolveBase(body.Service)
			if !ok {
				http.Error(w, "unknown service", http.StatusNotFound)
				return
			}
			if err := rs.Mark(base, body.Tag, body.Label, actor); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, actor, "image.mark", body.Service+":"+body.Tag)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "marked", "tag": body.Tag})
		case sub == "mark" && req.Method == "DELETE":
			var body struct{ Service, Tag string }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			base, ok := resolveBase(body.Service)
			if !ok {
				http.Error(w, "unknown service", http.StatusNotFound)
				return
			}
			if err := rs.Unmark(base, body.Tag); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, actor, "image.unmark", body.Service+":"+body.Tag)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "unmarked", "tag": body.Tag})
		case sub == "delete" && req.Method == "DELETE":
			var body struct{ Token string }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if body.Token == "" {
				http.Error(w, "token required", http.StatusBadRequest)
				return
			}
			// SAFETY: recompute protection fresh — never from history.
			svcs, err := dc.listServices(req.Context())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			protRefs, protIDs, err := protectedSets(req.Context(), dc, rs, mergedServices(svcs, onb.List()))
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if protRefs[body.Token] || protIDs[body.Token] {
				httpx.WriteJSON(w, http.StatusConflict, map[string]string{
					"error": "image is protected (in use, current, or marked stable) — not deleted",
				})
				return
			}
			// A ref token may point at a protected image ID (another tag of a
			// running image) — resolve and check that too.
			imgs, err := dc.listImages(req.Context())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			for _, img := range imgs {
				for _, rt := range img.RepoTags {
					if rt == body.Token && protIDs[img.Id] {
						httpx.WriteJSON(w, http.StatusConflict, map[string]string{
							"error": "image is in use under another tag — not deleted",
						})
						return
					}
				}
			}
			if err := dc.removeImage(req.Context(), body.Token, false); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, actor, "image.delete", body.Token)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted", "token": body.Token})
		case sub == "prune" && req.Method == "POST":
			var body struct {
				Service string `json:"service"`
				KeepN   int    `json:"keep_n"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			svcs, err := dc.listServices(req.Context())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// buildImagesInfo computes the protection sets LIVE right here —
			// that (not history) decides what imagesToPrune may emit.
			info, err := buildImagesInfo(req.Context(), dc, rs, ih, svcs, onb.List())
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			deleted, failed, reclaimed := runImagePrune(req.Context(), dc, info, body.Service, body.KeepN, func(token string) {
				audit(req, actor, "image.delete", token)
			})
			target := "all"
			if body.Service != "" {
				target = body.Service
			}
			audit(req, actor, "image.prune",
				target+" keep_n="+strconv.Itoa(body.KeepN)+" deleted="+strconv.Itoa(len(deleted))+
					" failed="+strconv.Itoa(len(failed))+" reclaimed_bytes="+strconv.FormatInt(reclaimed, 10))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"deleted": deleted, "failed": failed, "reclaimed_bytes": reclaimed,
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))

	return mux
}

// buildManagedServices is the shared local-services assembly behind both
// GET /api/services and GET /peer/services (peers.go's peerServicesHandler)
// — self-exclusion (Phase 0's excludeSelf) is automatically applied to both,
// since both funnel through here, so a peer never learns about the
// dashboard managing itself either.
func buildManagedServices(ctx context.Context, dc *dockerClient, onb *OnboardedStore, ic *imageChecker) ([]Service, error) {
	svcs, err := dc.listServices(ctx)
	if err != nil {
		return nil, err
	}
	// Drop the dashboard's own service — it must never appear as
	// something you can stop/replace/scale from within its own
	// UI. Deliberately asymmetric with /api/service-status
	// (servicestatus.go), which does NOT filter itself out, so
	// statusbot/Observability still reports the dashboard's own
	// health.
	svcs = excludeSelf(svcs)
	// NOTE: excludeSelf runs before the onboarded-merge dedupe
	// below, which matches by name against svcs. In practice
	// this never collides — the discovery/onboarding UI only
	// offers containers WITHOUT proxy labels, and the
	// dashboard's own container always carries them — but if a
	// "dashboard" entry were ever forced into the onboarded
	// store anyway, it would no longer find a name match here
	// and would get appended as a second (onboarded-only) card.
	// Mutations would still be blocked by the guard below.
	// Merge in onboarded services. If a labeled service already
	// has the same name (auto-promoted via the lifecycle Stop
	// path), DON'T append a second entry — just mark the
	// existing labeled view as Onboarded so it picks up the
	// unified surface (Stage/Promote/Replace/Rollback). Pure
	// onboarded-only entries (adopted from unlabelled
	// containers) get appended as standalone Service cards.
	labeledIdx := map[string]int{}
	for i := range svcs {
		labeledIdx[svcs[i].Name] = i
	}
	for _, o := range onb.List() {
		if i, ok := labeledIdx[o.Name]; ok {
			svcs[i].Onboarded = true
			svcs[i].DualTracked = true
			// Opt-in from either source (label or dashboard toggle) wins.
			svcs[i].AutoUpdate = svcs[i].AutoUpdate || o.AutoUpdate
			if svcs[i].PreviousImage == "" {
				svcs[i].PreviousImage = o.PreviousImage
			}
			if svcs[i].CanaryImage == "" {
				svcs[i].CanaryImage = o.CanaryImage
				svcs[i].CanaryReplicas = o.CanaryReplicas
			}
			dc.mergeOnboardedLiveState(ctx, &svcs[i])
			continue
		}
		svcs = append(svcs, Service{
			Name:           o.Name,
			Image:          o.Image,
			Host:           o.Host,
			Port:           o.Port,
			Replicas:       o.Replicas,
			PreviousImage:  o.PreviousImage,
			AutoUpdate:     o.AutoUpdate,
			CanaryImage:    o.CanaryImage,
			CanaryReplicas: o.CanaryReplicas,
			Onboarded:      true,
			Unscalable:     o.Host == "",
		})
	}
	// Enrich with image-checker results AFTER the merge so
	// onboarded-only entries get the update badge too.
	for i := range svcs {
		if st := ic.Get(svcs[i].Image); st != nil {
			if st.UpdateAvailable {
				svcs[i].UpdateAvailable = true
			}
			if st.Err != "" {
				svcs[i].ImageCheckError = st.Err
			}
		}
	}
	return svcs, nil
}

// sessionFromReq is the nil-safe form of AuthStore.sessionFrom — returns
// the *sessionInfo if the cookie validates, else nil. Useful for audit
// callsites that just want the username (or "" if unauthenticated).
func sessionFromReq(auth *AuthStore, r *http.Request) *sessionInfo {
	info, ok := auth.sessionFrom(r)
	if !ok {
		return nil
	}
	return info
}

func sessionUser(info *sessionInfo) string {
	if info == nil {
		return ""
	}
	return info.Username
}

// writeServiceErr maps a service-mutation failure onto the response. An
// unresolved env conflict becomes a 409 carrying every conflicting key, which
// the dashboard renders as a keep-mine/keep-current picker; everything else
// keeps the existing 400.
func writeServiceErr(w http.ResponseWriter, err error) {
	var ce *envConflictError
	if errors.As(err, &ce) {
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":     err.Error(),
			"conflicts": ce.Conflicts,
		})
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func cfDomain(cf *cloudflareRegistry) string {
	return cf.DefaultDomain()
}

// cfZoneFromReq resolves the optional ?zone=<domain> selector. The value is
// only ever a registry key — it never reaches a Cloudflare URL. An absent zone
// means the default, so single-zone callers behave exactly as before.
func cfZoneFromReq(cf *cloudflareRegistry, req *http.Request) (*cloudflareClient, string, bool) {
	domain := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("zone")))
	c, ok := cf.Lookup(domain)
	if !ok {
		return nil, domain, false
	}
	if domain == "" {
		domain = cf.DefaultDomain()
	}
	return c, domain, true
}

// hostForReq resolves the optional ?host= query parameter against registry.
// host is the trimmed query value (possibly empty). isPeer is true iff host
// names something other than this instance's own identity — i.e. host is
// non-empty and either registry is nil (nothing to compare against) or host
// doesn't match registry.Identity(). Callers still need to resolve host to a
// peer URL (registry.URLForIdentity) and validate the peer secret themselves,
// since existing call sites differ in exactly how they respond to a nil
// registry / unresolvable host / missing secret — this only factors out the
// query-param parsing and local-vs-other comparison shared by all of them.
func hostForReq(req *http.Request, registry *PeerRegistry) (host string, isPeer bool) {
	host = strings.TrimSpace(req.URL.Query().Get("host"))
	if host == "" {
		return "", false
	}
	if registry != nil && host == registry.Identity() {
		return host, false
	}
	return host, true
}

// forwardImageMutation relays one mutating /api/images/<sub> request to the
// peer identified by host, translating the request's (sub, method) pair onto
// its /peer/images/<sub> counterpart — the write-mesh sibling of the
// /api/images GET handler's own forwarding branch above. Unlike that GET
// forwarder, this never sanitizes the peer's response body before relaying
// it: mark/unmark/delete/prune don't return anything as sensitive as a
// DeleteToken, and mapPeerMutationErr already keeps a peer's own auth
// failures from leaking through.
//
// Forwards the mutation unconditionally, regardless of whether THIS host's
// own -peer-writes is set — that flag only gates whether this host accepts
// inbound peer mutations, not whether it may request them of others. The
// target peer alone decides whether it accepts (peerImagesMutateHandler
// re-checks its own writesEnabled), the same way GET forwarding never checks
// anything about this host before asking a peer to answer.
func forwardImageMutation(w http.ResponseWriter, req *http.Request, host string, registry *PeerRegistry, sub, actor string) {
	var method, peerPath string
	switch {
	case sub == "mark" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/images/mark"
	case sub == "mark" && req.Method == http.MethodDelete:
		method, peerPath = http.MethodDelete, "/peer/images/mark"
	case sub == "delete" && req.Method == http.MethodDelete:
		method, peerPath = http.MethodDelete, "/peer/images/delete"
	case sub == "prune" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/images/prune"
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if registry == nil {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerURL, ok := registry.URLForIdentity(host)
	if !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if peerSecret == "" {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		httpx.WriteErr(w, err)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	code, respBody, mutErr := peerMutate(req.Context(), client, peerURL, peerSecret, method, peerPath,
		10*time.Second, bytes.NewReader(reqBody), nil, mintForwardedActor(req, actor))
	if mutErr != nil {
		mapPeerMutationErr(w, 0, []byte(mutErr.Error()))
		return
	}
	if code < 200 || code >= 300 {
		mapPeerMutationErr(w, code, respBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(respBody)
}

// runServiceScale is the scale logic shared by the local /api/services/
// {name}/scale handler and peers.go's peerServicesMutateHandler. Onboarded
// services have a separate scale path that clones via the saved template
// image+env and rewrites routes.json instead of relying on label-based
// discovery — that path calls proxyRefresh on success because routes.json
// needs an explicit nudge; scaleService (label-managed) does NOT, because
// label routing self-discovers via docker events. This asymmetry is
// intentional — do not "fix" it.
func runServiceScale(ctx context.Context, dc *dockerClient, onb *OnboardedStore, routesPath, proxyURL, name string, replicas int) error {
	if _, ok := onb.Get(name); ok {
		if err := dc.scaleOnboarded(ctx, name, replicas, onb, routesPath); err != nil {
			return err
		}
		proxyRefresh(proxyURL)
		return nil
	}
	return dc.scaleService(ctx, name, replicas)
}

// runServiceLifecycle is the stop/start logic shared by the local
// /api/services/{name}/{stop,start} handler and peers.go's
// peerServicesMutateHandler. Unlike runServiceScale, there's no
// onboarded/label-managed asymmetry here — proxyRefresh always runs on
// success, for both paths.
func runServiceLifecycle(ctx context.Context, dc *dockerClient, proxyURL string, svc Service, act string) (acted int, err error) {
	if act == "stop" {
		acted, err = stopServiceMembers(ctx, dc, svc)
	} else {
		acted, err = startServiceMembers(ctx, dc, svc)
	}
	if err != nil {
		return acted, err
	}
	proxyRefresh(proxyURL)
	return acted, nil
}

// errAutoUpdateSelf is returned by runServiceAutoUpdateSet when enabling
// autoupdate would let a future autoUpdater cycle replace the dashboard's own
// container — either because the service demonstrably IS the dashboard, or
// because a Docker error means that can't be ruled out. Both cases are
// wrapped in this same sentinel (via %w) so writeAutoUpdateErr maps either to
// 403, not 400: an identity check that failed is exactly as dangerous here as
// one that succeeded and found self, since the write path (onb.SetAutoUpdate
// / dc.setAutoUpdateLabel) has no independent live-state re-read of its own.
var errAutoUpdateSelf = errors.New("refusing to enable auto-update for the dashboard's own service — could trigger unattended self-replacement")

// runServiceAutoUpdateSet is the autoupdate-toggle logic shared by the local
// /api/services/{name}/autoupdate handler and peers.go's peer branch.
// onb.SetAutoUpdate is a pure store write with no live Docker re-read, so
// unlike dc.setAutoUpdateLabel (which re-reads live state itself via
// setAutoUpdateLabel's own container lookup), enabling it here must
// independently fail CLOSED on a Docker error, not pass through the way the
// outer per-request self-guard (dc.serviceContainsSelfByName, checked before
// dispatch) does. Only checked when enabled=true — disabling is
// de-escalating, so it skips the check entirely.
func runServiceAutoUpdateSet(ctx context.Context, dc *dockerClient, onb *OnboardedStore, name string, enabled bool) error {
	if o, ok := onb.Get(name); ok {
		if o.Host == "" {
			return fmt.Errorf("managed-only service (no route) — auto-update needs a routed onboarded service")
		}
		if enabled {
			self, err := dc.serviceContainsSelfByName(ctx, name)
			if err != nil {
				return fmt.Errorf("could not verify service identity: %v: %w", err, errAutoUpdateSelf)
			}
			if self {
				return errAutoUpdateSelf
			}
		}
		return onb.SetAutoUpdate(name, enabled)
	}
	return dc.setAutoUpdateLabel(ctx, name, enabled)
}

// writeAutoUpdateErr maps runServiceAutoUpdateSet's error onto the response:
// errAutoUpdateSelf (self=true OR the identity check itself errored, both
// wrapped in this sentinel) -> 403, a real refusal; everything else -> 400
// (preserves the existing plain-400 behavior for all other error cases, e.g.
// managed-only or a downstream Docker failure unrelated to identity).
func writeAutoUpdateErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errAutoUpdateSelf) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// writeOnboardErr maps onboardContainer's (and, transitively,
// checkOnboardTarget's) error onto the response: errOnboardRefused ->
// 403 (destructive action correctly refused — the dashboard's own container
// or a fixed infra container, or an identity check that failed to run),
// everything else -> 400 (preserves the existing plain-400 behavior for all
// other error cases: invalid input, container not found, HostConfig
// unknowns, etc).
func writeOnboardErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errOnboardRefused) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// errServiceRemovalSelf is returned by runServiceOffboard and runServiceDelete
// when removing a service would remove or reroute away from the dashboard's
// own container — either because the service demonstrably IS the dashboard,
// or because a Docker error means that can't be ruled out. Both cases are
// wrapped in this same sentinel (via %w), mirroring errAutoUpdateSelf, for
// two independent reasons depending on which function raised it:
//
//   - runServiceOffboard, onboarded path: offboardContainer's final step
//     (store.Remove) is a pure store write with no live Docker re-read
//     afterward — same hazard class as onb.SetAutoUpdate. The outer
//     per-request self-guard (dc.serviceContainsSelfByName, checked before
//     subpath dispatch) fails OPEN on a Docker error by design, so this
//     independent check must fail CLOSED instead.
//   - runServiceDelete, label-managed path: dc.deleteService DOES re-read
//     live container state itself, so the outer guard's fail-open behavior
//     is less obviously a gap here — but the consequence is irreversible
//     container destruction (unlike stop/scale, there's no way back), so an
//     independent fail-closed check is added anyway.
//
// A single shared sentinel is used for both offboard and delete rather than
// two: both are "you are about to remove or disconnect the dashboard's own
// service" refusals with identical caller-facing semantics (403, same
// message shape), and every call site already knows which action it's
// performing without needing to distinguish by error identity.
var errServiceRemovalSelf = errors.New("refusing to offboard or delete the dashboard's own service — could destroy or reroute away from it with no way back")

// runServiceOffboard is the offboard logic shared by the local /offboard and
// DELETE-on-an-onboarded-service handlers and peers.go's peer branch.
// Onboarded services go through offboardContainer (see errServiceRemovalSelf
// for why the self-check is required here); label-managed services go
// through offboardLabelManaged, which re-reads live state itself and so
// needs no additional check.
func runServiceOffboard(ctx context.Context, dc *dockerClient, onb *OnboardedStore, routesPath, name string) error {
	if _, ok := onb.Get(name); ok {
		self, err := dc.serviceContainsSelfByName(ctx, name)
		if err != nil {
			return fmt.Errorf("could not verify service identity: %v: %w", err, errServiceRemovalSelf)
		}
		if self {
			return errServiceRemovalSelf
		}
		return dc.offboardContainer(ctx, name, onb, routesPath)
	}
	return dc.offboardLabelManaged(ctx, name)
}

// runServiceDelete is the DELETE logic shared by the local DELETE handler
// and peers.go's peer branch. Onboarded services are delegated to
// runServiceOffboard (DELETE-on-onboarded has always meant "offboard", not
// "destroy the user's original container" — see offboardContainer's doc
// comment); membersActed is 0 for that path since offboardContainer doesn't
// report a member count. Label-managed services go through dc.deleteService,
// which — despite re-reading live state itself — performs irreversible
// container destruction, so it gets its own independent fail-closed
// self-check rather than relying solely on the outer per-request guard.
func runServiceDelete(ctx context.Context, dc *dockerClient, onb *OnboardedStore, routesPath, name string) (membersActed int, err error) {
	if _, ok := onb.Get(name); ok {
		return 0, runServiceOffboard(ctx, dc, onb, routesPath, name)
	}
	self, err := dc.serviceContainsSelfByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("could not verify service identity: %v: %w", err, errServiceRemovalSelf)
	}
	if self {
		return 0, errServiceRemovalSelf
	}
	return dc.deleteService(ctx, name)
}

// writeServiceRemovalErr maps runServiceOffboard's error onto the response:
// errServiceRemovalSelf -> 403, a real refusal; everything else falls back
// to whichever status the pre-extraction handler used for that branch —
// badRequestOnFail=true for offboardLabelManaged's failures (always 400,
// preserved as-is), false for offboardContainer's (always 500 via
// httpx.WriteErr, preserved as-is). This is NOT used for runServiceDelete's
// label-managed failures — see writeServiceDeleteErr, which additionally
// carries membersActed.
func writeServiceRemovalErr(w http.ResponseWriter, err error, badRequestOnFail bool) {
	if errors.Is(err, errServiceRemovalSelf) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if badRequestOnFail {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpx.WriteErr(w, err)
}

// writeServiceDeleteErr maps runServiceDelete's label-managed-path error onto
// the response. errServiceRemovalSelf -> 403 (membersActed is always 0 on
// that path, since the self-check runs before any Docker mutation — no
// partial-teardown count to report). Everything else -> 400 with a JSON body
// carrying both the error and membersActed, a deliberate deviation from the
// pre-existing plain-500 (httpx.WriteErr) that deleteService's failures used
// to produce: a bare 500 can't carry membersActed, and mapPeerMutationErr
// only relays a peer's response body verbatim for 400/404/409 (401/403 are
// collapsed to a generic 502, everything else — including a bare 500 — to an
// opaque "peer mutation outcome unknown" 502) — so 400 is also what's needed
// for a forwarded partial-teardown count to survive the hop back to the
// local caller.
func writeServiceDeleteErr(w http.ResponseWriter, membersActed int, err error) {
	if errors.Is(err, errServiceRemovalSelf) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "members_acted": membersActed})
}

// runServiceCheckImage is the "check now" logic shared by the local
// /api/services/{name}/check handler and peers.go's peer branch. Reproduces
// the pre-extraction handler's three response shapes exactly:
//   - service not found: (nil, http.StatusNotFound, nil)
//   - found, no staged canary: (*imageStatus, http.StatusOK, nil), or
//     (errMsg string, http.StatusBadGateway, nil) if the live check failed
//   - found, staged canary: (map[string]any{"live":..,"canary":..},
//     http.StatusOK, nil), or (errMsg string, http.StatusBadGateway, nil) if
//     either check failed
//
// A non-nil err is reserved for findService itself failing (mirrors the
// original handler's httpx.WriteErr branch) — callers should relay it the
// same way. Auditing is NOT done here — it happens at the call sites, since
// the original local handler audits unconditionally once the service is
// found (even on the eventual 502 path), and the peer branch's audit
// convention (success-only) differs; extracting it here would force one
// behavior onto both.
func runServiceCheckImage(ctx context.Context, dc *dockerClient, ic *imageChecker, onb *OnboardedStore, name string) (payload any, status int, err error) {
	svc, ok, err := findService(ctx, dc, name)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, http.StatusNotFound, nil
	}

	// A staged canary can live in the labeled-container view (svc.CanaryImage,
	// set by dc.listServices from the c-prefixed container's proxy.canary
	// label) or, for onboarded services, only in the onboarded store — the
	// canary clones onboardedBaseEnv creates carry no proxy.* labels, so
	// dc.listServices never sees them. Fall back to the store so an onboarded
	// canary isn't silently skipped.
	canaryImage := svc.CanaryImage
	if canaryImage == "" && onb != nil {
		if o, ok := onb.Get(name); ok {
			canaryImage = o.CanaryImage
		}
	}

	ic.Check(ctx, svc.Image)
	live := ic.Get(svc.Image)

	if canaryImage == "" {
		if live.Err != "" {
			return live.Err, http.StatusBadGateway, nil
		}
		return live, http.StatusOK, nil
	}

	ic.Check(ctx, canaryImage)
	canary := ic.Get(canaryImage)
	if live.Err != "" || canary.Err != "" {
		msg := ""
		if live.Err != "" {
			msg = "live " + svc.Image + ": " + live.Err
		}
		if canary.Err != "" {
			if msg != "" {
				msg += "; "
			}
			msg += "canary " + canaryImage + ": " + canary.Err
		}
		return msg, http.StatusBadGateway, nil
	}
	return map[string]any{"live": live, "canary": canary}, http.StatusOK, nil
}

// forwardServiceMutation relays one mutating /api/services/{name}/<sub>
// request to the peer identified by host, translating (parts, method) onto
// its /peer/services/{name}/<sub> counterpart — the write-mesh sibling of
// forwardImageMutation above. Covers scale, stop, start, replicas/{member}/
// {stop,start}, autoupdate, check, replace, stage, promote, canary
// (discard), offboard, and delete — every mutating service action now
// forwards. The only thing still genuinely local-only among service writes
// is onboard, which lives under the separate /api/discovery/ path and is out
// of scope here. DELETE has no subpath (parts is length 1, unlike every
// other action here), and its outgoing body is always {"confirm": name}
// constructed server-side below — never read from the incoming request —
// so a forwarded delete can't be sent without confirmation and can't have
// its confirmation spoofed by whatever the original caller happened to send.
func forwardServiceMutation(w http.ResponseWriter, req *http.Request, host string, registry *PeerRegistry, name string, parts []string, actor string) {
	var method, peerPath string
	switch {
	case len(parts) == 2 && parts[1] == "scale" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/scale"
	case len(parts) == 2 && parts[1] == "stop" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/stop"
	case len(parts) == 2 && parts[1] == "start" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/start"
	case len(parts) == 2 && strings.HasPrefix(parts[1], "replicas/") && req.Method == http.MethodPost:
		sub := strings.TrimPrefix(parts[1], "replicas/")
		memberParts := strings.SplitN(sub, "/", 2)
		if len(memberParts) != 2 || (memberParts[1] != "stop" && memberParts[1] != "start") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		method = http.MethodPost
		peerPath = "/peer/services/" + url.PathEscape(name) + "/replicas/" + url.PathEscape(memberParts[0]) + "/" + memberParts[1]
	case len(parts) == 2 && parts[1] == "autoupdate" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/autoupdate"
	case len(parts) == 2 && parts[1] == "singleton" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/singleton"
	case len(parts) == 2 && parts[1] == "check" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/check"
	case len(parts) == 2 && parts[1] == "replace" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/replace"
	case len(parts) == 2 && parts[1] == "stage" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/stage"
	case len(parts) == 2 && parts[1] == "promote" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/promote"
	case len(parts) == 2 && parts[1] == "canary" && req.Method == http.MethodDelete:
		method, peerPath = http.MethodDelete, "/peer/services/"+url.PathEscape(name)+"/canary"
	case len(parts) == 2 && parts[1] == "offboard" && req.Method == http.MethodPost:
		method, peerPath = http.MethodPost, "/peer/services/"+url.PathEscape(name)+"/offboard"
	case len(parts) == 1 && req.Method == http.MethodDelete:
		method, peerPath = http.MethodDelete, "/peer/services/"+url.PathEscape(name)
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if registry == nil {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerURL, ok := registry.URLForIdentity(host)
	if !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if peerSecret == "" {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	var reqBody []byte
	var err error
	if method == http.MethodDelete && len(parts) == 1 {
		// Server-side confirm, never trusted from the incoming request — see
		// the doc comment above.
		reqBody, err = json.Marshal(map[string]string{"confirm": name})
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
	} else {
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	code, respBody, mutErr := peerMutate(req.Context(), client, peerURL, peerSecret, method, peerPath,
		10*time.Second, bytes.NewReader(reqBody), nil, mintForwardedActor(req, actor))
	if mutErr != nil {
		mapPeerMutationErr(w, 0, []byte(mutErr.Error()))
		return
	}
	if code < 200 || code >= 300 {
		mapPeerMutationErr(w, code, respBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(respBody)
}

// forwardOnboardMutation relays one POST /api/discovery/{name}/onboard
// request to the peer identified by host, onto its /peer/discovery/{name}/
// onboard counterpart — onboard's own write-mesh forwarder, kept separate
// from forwardServiceMutation since onboard lives under a different mux
// prefix (see peers.go's peerDiscoveryMutateHandler doc comment for why).
//
// The request body (OnboardRequest: Host, Port, Path, Strip, Replicas — no
// image, no container ID, no labels) is forwarded VERBATIM, unlike delete's
// server-constructed confirm body. That's safe here because the peer
// independently resolves the target BY NAME against its own live Docker
// daemon and re-validates via checkOnboardTarget before mutating anything —
// it never trusts the requester's view of state. Same principle every prior
// phase's forwarding relies on.
func forwardOnboardMutation(w http.ResponseWriter, req *http.Request, host string, registry *PeerRegistry, name string, actor string) {
	if registry == nil {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerURL, ok := registry.URLForIdentity(host)
	if !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerSecret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if peerSecret == "" {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		httpx.WriteErr(w, err)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	code, respBody, mutErr := peerMutate(req.Context(), client, peerURL, peerSecret, http.MethodPost, "/peer/discovery/"+url.PathEscape(name)+"/onboard",
		10*time.Second, bytes.NewReader(reqBody), nil, mintForwardedActor(req, actor))
	if mutErr != nil {
		mapPeerMutationErr(w, 0, []byte(mutErr.Error()))
		return
	}
	if code < 200 || code >= 300 {
		mapPeerMutationErr(w, code, respBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(respBody)
}

// writeCFErr maps a Cloudflare failure onto the response. Cloudflare's own
// 401/403 must NOT be reflected: 401 and 403 are the dashboard's own auth
// vocabulary (the UI's api() helper treats them as "session expired" and "2FA
// required"), so a token that isn't scoped for one zone would pop an auth
// dialog on every 5s poll. It's an upstream credential problem — 502.
func writeCFErr(w http.ResponseWriter, domain string, err error) {
	switch code := cfStatus(err); code {
	case 0:
		httpx.WriteErr(w, err)
	case http.StatusUnauthorized, http.StatusForbidden:
		http.Error(w, "token lacks permission for zone "+domain, http.StatusBadGateway)
	default:
		http.Error(w, err.Error(), code)
	}
}

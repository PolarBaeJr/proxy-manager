package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

func monitorURLFromEnv() string { return os.Getenv("MONITOR_URL") }
func proxyURLFromEnv() string   { return os.Getenv("PROXY_URL") }

func newDashboardMux(dc *dockerClient, cf *cloudflareClient, auth *AuthStore, rl *rateLimiter, ic *imageChecker, routesConfigPath string, pm *passkeyManager, onb *OnboardedStore, rs *ReleasesStore, prefs *PrefsStore, ih *ImageHistoryStore) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// Public health endpoint — no auth, sanitized output. Safe to expose to
	// Uptime Kuma / Pingdom / Statuspage / curl scripts. Does NOT leak host names,
	// route details, or traffic counts. Returns only per-binary up/down state.
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		overall := "up"
		targets := []map[string]any{}
		if monitorURLFromEnv() != "" {
			client := http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get(monitorURLFromEnv() + "/api/overview")
			if err == nil {
				defer resp.Body.Close()
				var o struct {
					Health  string `json:"health"`
					Targets []struct {
						Name   string `json:"name"`
						Health string `json:"health"`
					} `json:"targets"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&o); err == nil {
					// Recompute overall from non-absent targets so the public
					// health endpoint isn't poisoned by services the user
					// hasn't deployed (e.g. edge with profile off).
					anyDegraded := false
					for _, t := range o.Targets {
						if t.Health == "absent" {
							continue
						}
						targets = append(targets, map[string]any{"name": t.Name, "health": t.Health})
						if t.Health != "up" {
							anyDegraded = true
						}
					}
					if anyDegraded {
						overall = "degraded"
					}
				}
			} else {
				overall = "degraded"
			}
		}
		status := http.StatusOK
		if overall != "up" {
			status = http.StatusServiceUnavailable
		}
		httpx.WriteJSON(w, status, map[string]any{
			"status":   overall,
			"targets":  targets,
			"checked_at": time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Host CPU / memory / disk for the header widget.
	mux.HandleFunc("/api/stats", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, GetStats())
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
			fwd("/api/target/" + sub)(w, req)
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
				var body struct{ Label string `json:"label"` }
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
				svcs, err := dc.listServices(req.Context())
				if err != nil {
					httpx.WriteErr(w, err)
					return
				}
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
						// Opt-in from either source (label or dashboard toggle) wins.
						svcs[i].AutoUpdate = svcs[i].AutoUpdate || o.AutoUpdate
						if svcs[i].PreviousImage == "" {
							svcs[i].PreviousImage = o.PreviousImage
						}
						if svcs[i].CanaryImage == "" {
							svcs[i].CanaryImage = o.CanaryImage
							svcs[i].CanaryReplicas = o.CanaryReplicas
						}
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
					if st := ic.Get(svcs[i].Image); st != nil && st.UpdateAvailable {
						svcs[i].UpdateAvailable = true
					}
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
		info, _ := auth.sessionFrom(req)
		if len(parts) == 2 && parts[1] == "scale" && req.Method == "POST" {
			var body struct{ Replicas int }
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			// Onboarded services have a separate scale path that clones via
			// the saved template image+env and rewrites routes.json instead
			// of relying on label-based discovery.
			if _, ok := onb.Get(name); ok {
				if err := dc.scaleOnboarded(req.Context(), name, body.Replicas, onb, routesConfigPath); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.scaleService(req.Context(), name, body.Replicas); err != nil {
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
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.replaceService(req.Context(), name, body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
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
			o, ok := onb.Get(name)
			if !ok {
				http.Error(w, "not an onboarded service — set the proxy.autoupdate label in compose for label-managed services", http.StatusNotFound)
				return
			}
			if o.Host == "" {
				http.Error(w, "managed-only service (no route) — auto-update needs a routed onboarded service", http.StatusBadRequest)
				return
			}
			if err := onb.SetAutoUpdate(name, body.Enabled); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.autoupdate_set", name+" => "+strconv.FormatBool(body.Enabled))
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": body.Enabled})
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
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				proxyRefresh(proxyURLFromEnv())
			} else if err := dc.stageCanary(req.Context(), name, body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
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
		// ---- Stop / Start (per-service or per-replica) ----
		// Stopping retains all container config — `docker start` brings it
		// back instantly. First stop of a labeled-but-not-onboarded service
		// also snapshots it into the onboarded store so the full lifecycle
		// (stage/promote/replace/rollback) becomes available. Auto-onboard
		// is best-effort: a snapshot failure is logged but doesn't block
		// the user's stop/start action.
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
			if !svc.Onboarded {
				if err := promoteToOnboarded(req.Context(), dc, onb, svc); err != nil {
					log.Printf("auto-onboard %s failed (continuing): %v", name, err)
				} else {
					audit(req, sessionUser(info), "service.auto_onboard", name)
				}
			}
			act := parts[1]
			var acted int
			if act == "stop" {
				acted, err = stopServiceMembers(req.Context(), dc, svc)
			} else {
				acted, err = startServiceMembers(req.Context(), dc, svc)
			}
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			proxyRefresh(proxyURLFromEnv())
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
			if !svc.Onboarded {
				if err := promoteToOnboarded(req.Context(), dc, onb, svc); err != nil {
					log.Printf("auto-onboard %s failed (continuing): %v", name, err)
				} else {
					audit(req, sessionUser(info), "service.auto_onboard", name)
				}
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
		if req.Method == "DELETE" {
			// Onboarded services: tear down the clones + route, leave the
			// original container alone (the user started it).
			if _, ok := onb.Get(name); ok {
				if err := dc.offboardContainer(req.Context(), name, onb, routesConfigPath); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				proxyRefresh(proxyURLFromEnv())
				audit(req, sessionUser(info), "service.offboard", name)
				httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "offboarded"})
				return
			}
			if err := dc.deleteService(req.Context(), name); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.delete", name)
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
			return
		}
		http.NotFound(w, req)
	}))

	// ---- Cloudflare ----
	mux.HandleFunc("/api/cf/enabled", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": cf != nil, "domain": cfDomain(cf)})
	}))

	// ---- Container logs (read-only; auth-gated) ----
	registerLogRoutes(mux, dc, auth)

	// ---- Discovery: list containers NOT routed by the proxy (auth-gated) ----
	registerDiscoveryRoutes(mux, dc, auth, onb, routesConfigPath)

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
		var body OnboardRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if err := dc.onboardContainer(req.Context(), name, body, onb, routesConfigPath); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		proxyRefresh(proxyURLFromEnv())
		audit(req, user, "service.onboard", name)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "onboarded", "name": name})
	}))

	// ---- Passkeys / WebAuthn (when PASSKEY_RP_ID is set or default localhost) ----
	registerPasskeyRoutes(mux, auth, pm, rl)

	// ---- Proxy access log (read-only; auth-gated) ----
	if px := proxyURLFromEnv(); px != "" {
		mux.HandleFunc("/api/access", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
			url := px + "/access"
			if q := req.URL.RawQuery; q != "" {
				url += "?" + q
			}
			resp, err := http.Get(url)
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
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
				recs, err := cf.List(req.Context())
				if err != nil {
					httpx.WriteErr(w, err)
					return
				}
				httpx.WriteJSON(w, http.StatusOK, recs)
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				var body CreateDNSRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					httpx.WriteErr(w, err)
					return
				}
				rec, err := cf.Create(req.Context(), body)
				if err != nil {
					httpx.WriteErr(w, err)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "dns.create", body.Name)
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
		if id == "" {
			http.NotFound(w, req)
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
			rec, err := cf.Update(req.Context(), id, body)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(info), "dns.update", id)
			httpx.WriteJSON(w, http.StatusOK, rec)
		case "DELETE":
			if err := cf.Delete(req.Context(), id); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(info), "dns.delete", id)
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
		httpx.WriteJSON(w, http.StatusOK, info)
	}))
	mux.HandleFunc("/api/images/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		actor := sessionUser(sessionFromReq(auth, req))
		sub := strings.TrimPrefix(req.URL.Path, "/api/images/")
		// Resolve a managed service's image base for mark/unmark (same base
		// keying the ReleasesStore uses for infra services).
		resolveBase := func(service string) (string, bool) {
			svcs, err := dc.listServices(req.Context())
			if err != nil {
				return "", false
			}
			for _, s := range mergedServices(svcs, onb.List()) {
				if s.Name == service && s.Image != "" {
					base, _ := splitImageRef(s.Image)
					return base, true
				}
			}
			return "", false
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
			deleted := []string{}
			failed := []map[string]string{}
			var reclaimed int64
			doneIDs := map[string]bool{}
			doneTokens := map[string]bool{}
			for _, si := range info.Services {
				if body.Service != "" && si.Service != body.Service {
					continue
				}
				var metas []imgMeta
				protRefs := map[string]bool{}
				protIDs := map[string]bool{}
				for _, e := range si.Entries {
					if !e.OnDisk {
						continue
					}
					var ls time.Time
					if e.LastSeen > 0 {
						ls = time.Unix(e.LastSeen, 0)
					}
					metas = append(metas, imgMeta{
						Ref: e.Ref, ID: e.fullID, Tagged: e.tagged,
						SizeBytes: e.SizeBytes, LastSeen: ls, Created: e.created,
					})
					if e.Protected {
						protRefs[e.Ref] = true
						if e.fullID != "" {
							protIDs[e.fullID] = true
						}
					}
				}
				sizeByToken := map[string]imgMeta{}
				for _, m := range metas {
					if m.Tagged {
						sizeByToken[m.Ref] = m
					} else {
						sizeByToken[m.ID] = m
					}
				}
				for _, token := range imagesToPrune(metas, protRefs, protIDs, body.KeepN) {
					if doneTokens[token] {
						continue
					}
					doneTokens[token] = true
					if err := dc.removeImage(req.Context(), token, false); err != nil {
						failed = append(failed, map[string]string{"token": token, "error": err.Error()})
						continue
					}
					deleted = append(deleted, token)
					if m, ok := sizeByToken[token]; ok && m.ID != "" && !doneIDs[m.ID] {
						doneIDs[m.ID] = true
						reclaimed += m.SizeBytes
					}
					audit(req, actor, "image.delete", token)
				}
			}
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

func cfDomain(cf *cloudflareClient) string {
	if cf == nil {
		return ""
	}
	return cf.domain
}


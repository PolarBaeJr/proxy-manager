package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func monitorURLFromEnv() string { return os.Getenv("MONITOR_URL") }

func newDashboardMux(dc *dockerClient, cf *cloudflareClient, auth *AuthStore, rl *rateLimiter, ic *imageChecker, routesConfigPath string) http.Handler {
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
			resp, err := http.Get(monitorURLFromEnv() + "/api/overview")
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
					if o.Health != "" {
						overall = o.Health
					}
					for _, t := range o.Targets {
						targets = append(targets, map[string]any{"name": t.Name, "health": t.Health})
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
		writeJSON(w, status, map[string]any{
			"status":   overall,
			"targets":  targets,
			"checked_at": time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Host CPU / memory / disk for the header widget.
	mux.HandleFunc("/api/stats", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, GetStats())
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
		writeJSON(w, http.StatusOK, resp)
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
			writeErr(w, err)
			return
		}
		secret, uri, err := auth.BeginSetup(body.Username, body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
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
			writeErr(w, err)
			return
		}
		if err := auth.ConfirmPending(body.Username, body.Code); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		audit(req, body.Username, "user.setup_confirmed", body.Username)
		writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed", "username": body.Username})
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
			writeErr(w, err)
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
		var elev time.Time
		if body.Code != "" && auth.VerifyTOTP(body.Username, body.Code) {
			elev = time.Now().Add(elevatedLifetime)
		}
		setSessionCookie(w, auth.newCookie(body.Username, elev))
		audit(req, body.Username, "auth.login_ok", "")
		writeJSON(w, http.StatusOK, map[string]any{"username": body.Username, "elevated_until": elev.Unix()})
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
			writeErr(w, err)
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
		writeJSON(w, http.StatusOK, map[string]any{"elevated_until": elev.Unix()})
	}))

	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, req *http.Request) {
		if info, ok := auth.sessionFrom(req); ok {
			audit(req, info.Username, "auth.logout", "")
		}
		clearSessionCookie(w)
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
	})

	// ---- Users ----
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, auth.ListUsers())
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				var body struct{ Username, Password string }
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					writeErr(w, err)
					return
				}
				secret, uri, err := auth.BeginCreateUser(body.Username, body.Password)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "user.begin_create", body.Username)
				writeJSON(w, http.StatusOK, map[string]string{"username": body.Username, "totp_secret": secret, "otpauth_uri": uri})
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
				writeJSON(w, http.StatusOK, auth.ListTokens(info.Username))
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
					writeErr(w, err)
					return
				}
				audit(req, info.Username, "user.token_create", t.ID)
				writeJSON(w, http.StatusOK, map[string]any{
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
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}))

	mux.HandleFunc("/api/users/confirm", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ Username, Code string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeErr(w, err)
			return
		}
		if err := auth.ConfirmPending(body.Username, body.Code); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, _ := auth.sessionFrom(req)
		audit(req, sessionUser(info), "user.confirm_create", body.Username)
		writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed", "username": body.Username})
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
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}))

	// ---- Routes (view via dashboard's own docker discovery; no dep on proxy) ----
	mux.HandleFunc("/api/routes", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		routes, err := dc.listRoutes(req.Context(), routesConfigPath)
		if err != nil {
			writeErr(w, err)
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
		writeJSON(w, http.StatusOK, out)
	}))

	// ---- Services ----
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
				svcs, err := dc.listServices(req.Context())
				if err != nil {
					writeErr(w, err)
					return
				}
				// Enrich with image-checker results.
				for i := range svcs {
					if st := ic.Get(svcs[i].Image); st != nil && st.UpdateAvailable {
						svcs[i].UpdateAvailable = true
					}
				}
				writeJSON(w, http.StatusOK, svcs)
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				var body CreateServiceRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					writeErr(w, err)
					return
				}
				if err := dc.createService(req.Context(), body); err != nil {
					writeErr(w, err)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "service.create", body.Name)
				writeJSON(w, http.StatusOK, map[string]string{"status": "created"})
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
				writeErr(w, err)
				return
			}
			if err := dc.scaleService(req.Context(), name, body.Replicas); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.scale", name)
			writeJSON(w, http.StatusOK, map[string]any{"status": "scaled", "replicas": body.Replicas})
			return
		}
		if len(parts) == 2 && parts[1] == "replace" && req.Method == "POST" {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeErr(w, err)
				return
			}
			if err := dc.replaceService(req.Context(), name, body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.replace", name+" => "+body.Image)
			writeJSON(w, http.StatusOK, map[string]string{"status": "replaced", "image": body.Image})
			return
		}
		if len(parts) == 2 && parts[1] == "stage" && req.Method == "POST" {
			var body ReplaceServiceRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeErr(w, err)
				return
			}
			if err := dc.stageCanary(req.Context(), name, body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.stage", name+" => "+body.Image)
			writeJSON(w, http.StatusOK, map[string]string{"status": "staged"})
			return
		}
		if len(parts) == 2 && parts[1] == "promote" && req.Method == "POST" {
			if err := dc.promoteCanary(req.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.promote", name)
			writeJSON(w, http.StatusOK, map[string]string{"status": "promoted"})
			return
		}
		if len(parts) == 2 && parts[1] == "canary" && req.Method == "DELETE" {
			if err := dc.discardCanary(req.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			audit(req, sessionUser(info), "service.discard_canary", name)
			writeJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
			return
		}
		if req.Method == "DELETE" {
			if err := dc.deleteService(req.Context(), name); err != nil {
				writeErr(w, err)
				return
			}
			audit(req, sessionUser(info), "service.delete", name)
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
			return
		}
		http.NotFound(w, req)
	}))

	// ---- Cloudflare ----
	mux.HandleFunc("/api/cf/enabled", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": cf != nil, "domain": cfDomain(cf)})
	}))

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
					writeErr(w, err)
					return
				}
				writeJSON(w, http.StatusOK, recs)
			})(w, req)
		case "POST":
			auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
				var body CreateDNSRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					writeErr(w, err)
					return
				}
				rec, err := cf.Create(req.Context(), body)
				if err != nil {
					writeErr(w, err)
					return
				}
				info, _ := auth.sessionFrom(req)
				audit(req, sessionUser(info), "dns.create", body.Name)
				writeJSON(w, http.StatusOK, rec)
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
				writeErr(w, err)
				return
			}
			rec, err := cf.Update(req.Context(), id, body)
			if err != nil {
				writeErr(w, err)
				return
			}
			audit(req, sessionUser(info), "dns.update", id)
			writeJSON(w, http.StatusOK, rec)
		case "DELETE":
			if err := cf.Delete(req.Context(), id); err != nil {
				writeErr(w, err)
				return
			}
			audit(req, sessionUser(info), "dns.delete", id)
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	return mux
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// Maintenance mode — a per-host 503 switch owned by nginx, surfaced here.
//
// The mechanism lives entirely in nginx and is already deployed: every server
// block includes snippets/maintenance.conf, which does
//
//	if (-f /etc/nginx/maint.d/$host) { return 503; }
//
// at server level. The test runs per request, so creating or deleting the file
// takes effect immediately — there is NO nginx reload, and no proxy or
// routes.json involvement. A `geo $maint_bypass` block lets loopback,
// Tailscale (100.64.0.0/10) and the LAN (192.168.1.0/24) through, so the
// operator keeps seeing the real site while the public gets the 503 page.
// /usr/local/bin/maint is the shell equivalent of what this file does.
//
// The dashboard's entire job is therefore to create and remove empty marker
// files in a bind-mounted directory. Their contents are never read: nginx only
// stats them. nginx lowercases $host, so a flag file must be lowercase or it
// silently does nothing — hence every path goes through validMaintHost, which
// is lowercase-only, after normalization.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

const defaultMaintDir = "/etc/maint.d"

// maintStore is the bind-mounted flag directory. A nil *maintStore means the
// feature isn't configured; every method degrades to an error rather than
// panicking, so the mux can register the routes unconditionally.
type maintStore struct {
	dir string
}

// newMaintFromEnv resolves MAINT_DIR (default /etc/maint.d). Returns nil when
// the directory is absent or isn't a directory, along with lines the caller
// should log.
//
// Deliberately does NOT MkdirAll: on a host without the bind mount that would
// create the directory inside the container's ephemeral layer, and the feature
// would report itself "configured" while every flag it writes is read by
// nothing and the public site stays up.
func newMaintFromEnv(getenv func(string) string) (*maintStore, []string) {
	dir := strings.TrimSpace(getenv("MAINT_DIR"))
	if dir == "" {
		dir = defaultMaintDir
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, []string{"⚠ maintenance mode disabled: " + dir + " not available: " + err.Error()}
	}
	if !st.IsDir() {
		return nil, []string{"⚠ maintenance mode disabled: " + dir + " is not a directory"}
	}
	return &maintStore{dir: dir}, nil
}

// path is the ONLY place a caller-supplied host becomes a filesystem path. It
// normalizes, validates against the filename allowlist, and re-checks that the
// joined result is still a direct child of the flag directory. On any failure
// it returns an empty path so a caller that ignores the error can't act on it.
func (m *maintStore) path(host string) (string, error) {
	if m == nil {
		return "", os.ErrInvalid
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if !validMaintHost(h) {
		return "", os.ErrInvalid
	}
	p := filepath.Join(m.dir, h)
	if filepath.Dir(p) != filepath.Clean(m.dir) {
		return "", os.ErrInvalid
	}
	return p, nil
}

// List returns the hosts currently in maintenance, sorted. Entries that aren't
// plain files or don't match the allowlist (e.g. hand-made junk) are skipped.
func (m *maintStore) List() ([]string, error) {
	if m == nil {
		return nil, os.ErrInvalid
	}
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if !validMaintHost(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func (m *maintStore) IsOn(host string) (bool, error) {
	p, err := m.path(host)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// On creates the flag file. Idempotent. 0644 because nginx only stats it.
func (m *maintStore) On(host string) error {
	p, err := m.path(host)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Off removes the flag file. Idempotent. The host must match an existing
// directory entry exactly — clearing a flag is de-escalating, so it's bounded
// by what's actually there rather than by any route list.
func (m *maintStore) Off(host string) error {
	if m == nil {
		return os.ErrInvalid
	}
	h := strings.ToLower(strings.TrimSpace(host))
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || e.Name() != h {
			continue
		}
		p, err := m.path(h)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return nil
}

func registerMaintenanceRoutes(mux *http.ServeMux, dc *dockerClient, auth *AuthStore, m *maintStore, onb *OnboardedStore, routesConfigPath string) {
	mux.HandleFunc("/api/maintenance", auth.requireAuth(func(w http.ResponseWriter, _ *http.Request) {
		hosts := []string{}
		if m != nil {
			got, err := m.List()
			if err != nil {
				// The UI polls this every 5s; a transient read error must not
				// throw the whole Routes tab into an error state.
				log.Printf("maintenance: list %q: %v", m.dir, err)
			} else {
				hosts = got
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"configured": m != nil, "hosts": hosts})
	}))

	mux.HandleFunc("/api/maintenance/", auth.requireElevated(func(w http.ResponseWriter, req *http.Request) {
		if m == nil {
			http.Error(w, "maintenance dir not configured", http.StatusServiceUnavailable)
			return
		}
		host := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(req.URL.Path, "/api/maintenance/")))
		if !validMaintHost(host) {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}
		info, _ := auth.sessionFrom(req)
		switch req.Method {
		case "POST":
			// Second, independent defense: turning a host OFF the internet is
			// escalating, so it's allowed only for a host the proxy actually
			// serves. Never let an arbitrary string create a file.
			known, err := maintKnownHosts(req, dc, onb, routesConfigPath)
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if !known[host] {
				http.Error(w, "unknown host", http.StatusNotFound)
				return
			}
			if err := m.On(host); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(info), "maintenance.on", host)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"host": host, "maintenance": true})
		case "DELETE":
			// Deliberately NOT checked against the route set: label-discovered
			// hosts vanish from listRoutes once their containers stop, which is
			// exactly when a stale flag most needs clearing.
			if err := m.Off(host); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			audit(req, sessionUser(info), "maintenance.off", host)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"host": host, "maintenance": false})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

// maintKnownHosts is the lowercased set of hosts the proxy currently routes:
// label + static routes, plus onboarded services (whose containers may be
// stopped). An error here fails the request closed rather than permitting.
func maintKnownHosts(req *http.Request, dc *dockerClient, onb *OnboardedStore, routesConfigPath string) (map[string]bool, error) {
	routes, err := dc.listRoutes(req.Context(), routesConfigPath)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, r := range routes {
		if r.Host != "" {
			set[strings.ToLower(r.Host)] = true
		}
	}
	for _, o := range onb.List() {
		// Managed-only services carry no host — nothing to put in maintenance.
		if o.Host != "" {
			set[strings.ToLower(o.Host)] = true
		}
	}
	return set, nil
}

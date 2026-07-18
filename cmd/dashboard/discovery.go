// Discovery: list containers that AREN'T routed by the proxy. Lets the user
// see what's running locally that they could add to the proxy, without
// touching anything — the "Add" action just shows them the labels to paste
// into their docker-compose.yml. We never recreate compose-managed
// containers from the dashboard because that breaks compose's tracking.

package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

// dockerContainerPorts extends the basic container shape with exposed ports.
// Used only by the discovery endpoint — the existing dockerContainer in
// docker.go stays minimal to avoid churning a lot of call sites.
type dockerContainerPorts struct {
	dockerContainer
	Ports []struct {
		IP          string `json:"IP,omitempty"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort,omitempty"`
		Type        string `json:"Type,omitempty"`
	} `json:"Ports,omitempty"`
}

// discoveryItem is the JSON shape returned to the UI for one unmanaged container.
type discoveryItem struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Port    int    `json:"port"`              // best-guess internal port; 0 if unknown
	Ports   []int  `json:"ports,omitempty"`   // all distinct internal ports
	Path    string `json:"path,omitempty"`    // proxy.path label, if present
	Project string `json:"project,omitempty"` // docker-compose project, if any
	Service string `json:"service,omitempty"` // docker-compose service, if any
}

// Infra containers we never offer for routing — they ARE the proxy itself.
var infraContainerNames = map[string]bool{
	"proxy": true, "dashboard": true, "monitor": true, "edge": true,
}

func (c *dockerClient) listUnmanaged(ctx context.Context, exclude map[string]bool) ([]discoveryItem, error) {
	body, err := c.get(ctx, "/containers/json?all=false")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var all []dockerContainerPorts
	if err := json.NewDecoder(body).Decode(&all); err != nil {
		return nil, err
	}
	out := make([]discoveryItem, 0, len(all))
	for _, ct := range all {
		// Skip anything already routed.
		if ct.Labels[labelEnable] == "true" {
			continue
		}
		name := ct.name()
		if infraContainerNames[name] {
			continue
		}
		// Skip containers already adopted (managed-only or routed) and the
		// dashboard's own clone containers.
		if exclude[name] || strings.HasPrefix(name, "goproxy-onb-") {
			continue
		}
		// Distinct internal ports, smallest first (3000 before 5432 etc).
		seen := map[int]bool{}
		var ports []int
		for _, p := range ct.Ports {
			if p.PrivatePort == 0 || seen[p.PrivatePort] {
				continue
			}
			seen[p.PrivatePort] = true
			ports = append(ports, p.PrivatePort)
		}
		sort.Ints(ports)
		best := 0
		if len(ports) > 0 {
			best = ports[0]
		}
		// Only surface the proxy.path label as a pre-fill if it's a valid
		// route path. An unvalidated label reaches an inline onclick handler in
		// the UI, where esc()'s HTML-entity encoding does NOT prevent JS-string
		// breakout — so a hostile container could inject script. Drop anything
		// that doesn't pass validRoutePath.
		labelPathVal := ct.Labels[labelPath]
		if !validRoutePath(labelPathVal) {
			labelPathVal = ""
		}
		out = append(out, discoveryItem{
			Name:    name,
			Image:   ct.Image,
			State:   ct.State,
			Port:    best,
			Ports:   ports,
			Path:    labelPathVal,
			Project: ct.Labels["com.docker.compose.project"],
			Service: ct.Labels["com.docker.compose.service"],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// routedContainerNames returns the set of bare docker-service names that appear
// as static backends in routes.json. A backend like "http://auth:8096" yields
// "auth" — that container is already routed (just not via labels), so Discovery
// must not offer to onboard it. Backends pointing at host.docker.internal or an
// IP are host services, not edge-network containers, and are deliberately
// skipped so they never suppress anything in Discovery.
func routedContainerNames(routesJSON []byte) map[string]bool {
	out := map[string]bool{}
	var f routesFile
	if err := json.Unmarshal(routesJSON, &f); err != nil {
		return out
	}
	for _, r := range f.Routes {
		for _, b := range r.Backends {
			u, err := url.Parse(b)
			if err != nil {
				continue
			}
			host := u.Hostname()
			if host == "" || strings.Contains(host, ".") || net.ParseIP(host) != nil {
				continue
			}
			out[host] = true
		}
	}
	return out
}

func registerDiscoveryRoutes(mux *http.ServeMux, dc *dockerClient, auth *AuthStore, onb *OnboardedStore, routesConfigPath string) {
	mux.HandleFunc("/api/discovery", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		exclude := map[string]bool{}
		for _, o := range onb.List() {
			exclude[o.Name] = true
		}
		// Also exclude containers already used as a static backend in
		// routes.json — they're routed, just not via labels. If routes.json
		// is unreadable, proceed with just the onboarded exclusions.
		if b, err := os.ReadFile(routesConfigPath); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("discovery: read routes.json %q: %v", routesConfigPath, err)
			}
		} else {
			for name := range routedContainerNames(b) {
				exclude[name] = true
			}
		}
		items, err := dc.listUnmanaged(req.Context(), exclude)
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		// Strip docker-compose noise the UI doesn't need (e.g. project paths).
		for i := range items {
			items[i].Image = trimSha(items[i].Image)
		}
		httpx.WriteJSON(w, http.StatusOK, items)
	}))
}

// skippedItem is one container the batch-onboard skipped, with a human reason.
type skippedItem struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// batchOnboardTargets selects the containers of a compose project to adopt as
// managed-only. It returns the names to onboard (sorted, deterministic) and the
// ones skipped with a reason: already onboarded, or an invalid container name.
func batchOnboardTargets(items []discoveryItem, project string, alreadyOnboarded func(string) bool) (targets []string, skipped []skippedItem) {
	targets = []string{}
	skipped = []skippedItem{}
	for _, it := range items {
		if it.Project != project {
			continue
		}
		if alreadyOnboarded(it.Name) {
			skipped = append(skipped, skippedItem{Name: it.Name, Reason: "already onboarded"})
			continue
		}
		if !validServiceName(it.Name) {
			skipped = append(skipped, skippedItem{Name: it.Name, Reason: "invalid container name"})
			continue
		}
		targets = append(targets, it.Name)
	}
	sort.Strings(targets)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Name < skipped[j].Name })
	return targets, skipped
}

// trimSha hides the "@sha256:…" suffix some images carry — visual clutter for
// the UI's image column, irrelevant to onboarding.
func trimSha(s string) string {
	if i := strings.Index(s, "@sha256:"); i >= 0 {
		return s[:i]
	}
	return s
}

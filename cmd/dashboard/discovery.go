// Discovery: list containers that AREN'T routed by the proxy. Lets the user
// see what's running locally that they could add to the proxy, without
// touching anything — the "Add" action just shows them the labels to paste
// into their docker-compose.yml. We never recreate compose-managed
// containers from the dashboard because that breaks compose's tracking.

package main

import (
	"context"
	"encoding/json"
	"net/http"
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
	Project string `json:"project,omitempty"` // docker-compose project, if any
	Service string `json:"service,omitempty"` // docker-compose service, if any
}

// Infra containers we never offer for routing — they ARE the proxy itself.
var infraContainerNames = map[string]bool{
	"proxy": true, "dashboard": true, "monitor": true, "edge": true,
}

func (c *dockerClient) listUnmanaged(ctx context.Context) ([]discoveryItem, error) {
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
		out = append(out, discoveryItem{
			Name:    name,
			Image:   ct.Image,
			State:   ct.State,
			Port:    best,
			Ports:   ports,
			Project: ct.Labels["com.docker.compose.project"],
			Service: ct.Labels["com.docker.compose.service"],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func registerDiscoveryRoutes(mux *http.ServeMux, dc *dockerClient, auth *AuthStore) {
	mux.HandleFunc("/api/discovery", auth.requireAuth(func(w http.ResponseWriter, req *http.Request) {
		items, err := dc.listUnmanaged(req.Context())
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

// trimSha hides the "@sha256:…" suffix some images carry — visual clutter for
// the UI's image column, irrelevant to onboarding.
func trimSha(s string) string {
	if i := strings.Index(s, "@sha256:"); i >= 0 {
		return s[:i]
	}
	return s
}

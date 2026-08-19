package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ctNet is like docker_test.go's ct helper but with a network/IP, needed for
// listRoutes' backend resolution (which requires a reachable IP).
func ctNet(name, image, state string, labels map[string]string, ip string) map[string]any {
	return map[string]any{
		"Id":     name + "-id",
		"Names":  []string{"/" + name},
		"Image":  image,
		"State":  state,
		"Labels": labels,
		"NetworkSettings": map[string]any{
			"Networks": map[string]any{managedNetwork: map[string]any{"IPAddress": ip}},
		},
	}
}

func writeRoutesJSON(t *testing.T, routes ...map[string]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routes.json")
	b, err := json.Marshal(map[string]any{"routes": routes})
	if err != nil {
		t.Fatalf("marshal routes.json: %v", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write routes.json: %v", err)
	}
	return p
}

func findRouteView(views []RouteView, host, path string) *RouteView {
	for i := range views {
		if views[i].Host == host && views[i].Path == path {
			return &views[i]
		}
	}
	return nil
}

// TestListRoutesServiceFieldResolvesBackends mirrors
// cmd/proxy/router_test.go's equivalent for the dashboard's own Routes view:
// a routes.json entry with a Service field (no literal backends) must pick
// up backends from label-managed containers carrying that proxy.service
// label, and must NOT double-count a label-managed group's own service
// label as a resolution request.
func TestListRoutesServiceFieldResolvesBackends(t *testing.T) {
	routesPath := writeRoutesJSON(t, map[string]any{
		"host": "svc.example.com", "path": "/admin", "service": "auth",
	})
	dc := fakeDocker(t, ctJSON(
		ctNet("auth-1", "img:1", "running",
			map[string]string{labelEnable: "true", labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth"}, "172.20.0.11"),
		ctNet("auth-2", "img:1", "running",
			map[string]string{labelEnable: "true", labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth"}, "172.20.0.12"),
		ctNet("auth-canary-1", "img:2", "running",
			map[string]string{labelEnable: "true", labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth", labelCanary: "true"}, "172.20.0.13"),
	))

	views, err := dc.listRoutes(context.Background(), routesPath)
	if err != nil {
		t.Fatalf("listRoutes: %v", err)
	}

	svc := findRouteView(views, "svc.example.com", "/admin")
	if svc == nil || len(svc.Backends) != 2 {
		t.Fatalf("service-resolved view = %+v, want exactly the 2 non-canary auth backends", svc)
	}

	own := findRouteView(views, "auth.internal.example.com", "")
	if own == nil || len(own.Backends) != 3 {
		t.Fatalf("own label-managed view = %+v, want all 3 replicas (including canary) present", own)
	}
}

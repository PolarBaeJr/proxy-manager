package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSetAutoUpdateLabelFlipsOnlyTheLabel proves setAutoUpdateLabel recreates
// the service with the SAME image/env/mounts and only the proxy.autoupdate
// label value changed — the lightweight path label-managed services get
// instead of the old "not an onboarded service" 404.
func TestSetAutoUpdateLabelFlipsOnlyTheLabel(t *testing.T) {
	var createdLabels map[string]string
	var createdEnv []string
	var createdMounts []mountSpec
	var sawCreate bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{
					labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
				},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{
				"Config": map[string]any{"Env": []string{"FOO=bar"}},
				"HostConfig": map[string]any{
					"Mounts": []mountSpec{{Type: "bind", Source: "/host/data", Target: "/app/data"}},
				},
			})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			var body createBody
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if createdLabels == nil {
				createdLabels = body.Labels
				createdEnv = body.Env
				createdMounts = body.HostConfig.Mounts
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	if err := dc.setAutoUpdateLabel(context.Background(), "app", true); err != nil {
		t.Fatalf("setAutoUpdateLabel: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container created")
	}
	if createdLabels[labelAutoUpdate] != "true" {
		t.Errorf("labelAutoUpdate = %q, want true", createdLabels[labelAutoUpdate])
	}
	if createdLabels[labelHost] != "app.example" || createdLabels[labelService] != "app" {
		t.Errorf("proxy.* labels lost: %v", createdLabels)
	}
	if len(createdEnv) != 1 || createdEnv[0] != "FOO=bar" {
		t.Errorf("env = %v, want unchanged [FOO=bar]", createdEnv)
	}
	if len(createdMounts) != 1 || createdMounts[0].Source != "/host/data" {
		t.Errorf("mounts = %+v, want the original mount carried forward", createdMounts)
	}
}

// TestSetAutoUpdateLabelDisableRemovesLabel proves disabling drops the label
// entirely rather than setting it to "false" (matches createService/
// replaceService's existing all-or-nothing label conventions).
func TestSetAutoUpdateLabelDisableRemovesLabel(t *testing.T) {
	var createdLabels map[string]string
	var sawCreate bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{
					labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
					labelAutoUpdate: "true",
				},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			var body createBody
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if createdLabels == nil {
				createdLabels = body.Labels
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	if err := dc.setAutoUpdateLabel(context.Background(), "app", false); err != nil {
		t.Fatalf("setAutoUpdateLabel: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container created")
	}
	if v, ok := createdLabels[labelAutoUpdate]; ok {
		t.Errorf("labelAutoUpdate = %q, want removed entirely", v)
	}
}

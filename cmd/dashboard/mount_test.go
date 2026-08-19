package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestReplaceServiceCarriesMountsForward is the regression test for the bug
// fixed in Phase 0: replaceService (and its siblings) used to clone a
// container via Image/Labels/Env only, silently dropping any bind mount or
// named volume on every Replace — including an autoupdate-triggered one.
func TestReplaceServiceCarriesMountsForward(t *testing.T) {
	wantMounts := []mountSpec{
		{Type: "bind", Source: "/host/data", Target: "/app/data", ReadOnly: false},
	}
	var createdMounts []mountSpec
	var sawCreate bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{
				"Config":     map[string]any{"Env": []string{}},
				"HostConfig": map[string]any{"Mounts": wantMounts},
			})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			var body createBody
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if createdMounts == nil {
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

	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err != nil {
		t.Fatalf("replaceService: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container created")
	}
	if len(createdMounts) != 1 || createdMounts[0] != wantMounts[0] {
		t.Fatalf("Mounts = %+v, want %+v — the mount was dropped on replace", createdMounts, wantMounts)
	}
}

// TestInspectCloneSpec confirms inspectCloneSpec reads HostConfig.Mounts
// from a container's inspect JSON, the same shape a real docker inspect
// returns for a bind mount and a named volume.
func TestInspectCloneSpec(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/host/data", "Target": "/app/data", "ReadOnly": false},
					{"Type": "volume", "Source": "app-data", "Target": "/var/lib/app", "ReadOnly": true},
				},
			},
		})
	}))
	spec, err := dc.inspectCloneSpec(context.Background(), "any-id")
	if err != nil {
		t.Fatalf("inspectCloneSpec: %v", err)
	}
	want := []mountSpec{
		{Type: "bind", Source: "/host/data", Target: "/app/data", ReadOnly: false},
		{Type: "volume", Source: "app-data", Target: "/var/lib/app", ReadOnly: true},
	}
	if len(spec.Mounts) != len(want) {
		t.Fatalf("Mounts = %+v, want %+v", spec.Mounts, want)
	}
	for i := range want {
		if spec.Mounts[i] != want[i] {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, spec.Mounts[i], want[i])
		}
	}
}

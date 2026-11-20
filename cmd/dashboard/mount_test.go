package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
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

// TestInspectCloneSpecExtraHosts confirms inspectCloneSpec reads
// HostConfig.ExtraHosts verbatim and in inspect order — the fixture is
// deliberately unsorted, since order decides which entry wins for a hostname
// listed twice — and that a null ExtraHosts decodes to nothing.
func TestInspectCloneSpecExtraHosts(t *testing.T) {
	want := []string{"b.internal:10.0.0.2", "host.docker.internal:host-gateway", "a.internal:10.0.0.1"}
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/none/") {
			w.Write([]byte(`{"HostConfig":{"ExtraHosts":null}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"HostConfig": map[string]any{"ExtraHosts": want},
		})
	}))
	spec, err := dc.inspectCloneSpec(context.Background(), "any-id")
	if err != nil {
		t.Fatalf("inspectCloneSpec: %v", err)
	}
	if !reflect.DeepEqual(spec.ExtraHosts, want) {
		t.Fatalf("ExtraHosts = %v, want %v (same order)", spec.ExtraHosts, want)
	}

	spec, err = dc.inspectCloneSpec(context.Background(), "none")
	if err != nil {
		t.Fatalf("inspectCloneSpec (null ExtraHosts): %v", err)
	}
	if len(spec.ExtraHosts) != 0 {
		t.Fatalf("ExtraHosts = %v, want none for a null inspect value", spec.ExtraHosts)
	}
}

// replaceCreateBodies runs replaceService for a one-replica "app" whose
// template inspect carries the given HostConfig, and returns the raw body of
// every /containers/create the daemon stub saw.
func replaceCreateBodies(t *testing.T, tplHostConfig map[string]any) [][]byte {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte

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
				"HostConfig": tplHostConfig,
			})
		case strings.Contains(r.URL.Path, "/containers/create"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, b)
			mu.Unlock()
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
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no container created")
	}
	return bodies
}

// TestReplaceServiceCarriesExtraHostsForward is the regression test for
// compose `extra_hosts` being refused (and, before the refusal, dropped) on
// recreate: the replacement must be created with the template's ExtraHosts,
// in the same order, under Docker's own wire name.
func TestReplaceServiceCarriesExtraHostsForward(t *testing.T) {
	want := []string{"host.docker.internal:host-gateway", "db.internal:10.0.0.5"}
	bodies := replaceCreateBodies(t, map[string]any{"ExtraHosts": want})

	wire := `"ExtraHosts":["host.docker.internal:host-gateway","db.internal:10.0.0.5"]`
	for i, b := range bodies {
		if !strings.Contains(string(b), wire) {
			t.Fatalf("create body %d = %s, want it to contain %s", i, b, wire)
		}
		var body struct {
			HostConfig struct {
				ExtraHosts []string
			}
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatalf("create body %d: %v", i, err)
		}
		if !reflect.DeepEqual(body.HostConfig.ExtraHosts, want) {
			t.Fatalf("create body %d HostConfig.ExtraHosts = %v, want %v — extra_hosts was dropped or reordered on replace", i, body.HostConfig.ExtraHosts, want)
		}
	}
}

// TestReplaceServiceOmitsExtraHostsWhenTemplateHasNone pins the omitempty:
// a template with no extra_hosts must produce a create payload with no
// ExtraHosts key at all, exactly as before the field existed.
func TestReplaceServiceOmitsExtraHostsWhenTemplateHasNone(t *testing.T) {
	for i, b := range replaceCreateBodies(t, map[string]any{}) {
		if strings.Contains(string(b), `"ExtraHosts"`) {
			t.Fatalf("create body %d contains an ExtraHosts key for a template without one: %s", i, b)
		}
	}
}

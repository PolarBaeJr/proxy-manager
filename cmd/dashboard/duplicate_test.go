package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRunServiceDuplicateRejectsInfraName proves the infra-name denylist is
// checked before anything else — no Docker/registry calls needed to reject.
func TestRunServiceDuplicateRejectsInfraName(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected docker call: %s", r.URL.Path)
	}))
	_, err := runServiceDuplicate(context.Background(), dc, nil, nil, "", "proxy", DuplicateServiceRequest{Target: "peer-b"}, "")
	if err == nil || !strings.Contains(err.Error(), "infrastructure") {
		t.Fatalf("err = %v, want an infra-container refusal", err)
	}
}

// TestRunServiceDuplicateNotFound proves an unknown service name maps to
// errDuplicateNotFound.
func TestRunServiceDuplicateNotFound(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	_, err := runServiceDuplicate(context.Background(), dc, nil, nil, "", "ghost", DuplicateServiceRequest{Target: "peer-b"}, "")
	if !errors.Is(err, errDuplicateNotFound) {
		t.Fatalf("err = %v, want errDuplicateNotFound", err)
	}
}

// TestRunServiceDuplicateUnknownTarget proves an unresolvable target host is
// rejected with a clear error, both with a nil registry and with a registry
// that simply has no matching peer identity.
func TestRunServiceDuplicateUnknownTarget(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
		}})
	}))

	t.Run("nil registry", func(t *testing.T) {
		_, err := runServiceDuplicate(context.Background(), dc, nil, nil, "", "app", DuplicateServiceRequest{Target: "peer-b"}, "")
		if err == nil || !strings.Contains(err.Error(), "peer mesh not configured") {
			t.Fatalf("err = %v, want a peer-mesh-not-configured error", err)
		}
	})

	t.Run("unknown identity", func(t *testing.T) {
		reg := newPeerRegistry(nil, "s3cret", "dashboard-a", "dev", 0, nil)
		_, err := runServiceDuplicate(context.Background(), dc, reg, nil, "", "app", DuplicateServiceRequest{Target: "peer-ghost"}, "")
		if err == nil || !strings.Contains(err.Error(), "unknown target host") {
			t.Fatalf("err = %v, want an unknown-target-host error", err)
		}
	})
}

// TestRunServiceDuplicateRejectsBindMount proves a source service with a
// bind mount is refused rather than silently duplicated onto a host where
// the bind source can't possibly exist.
func TestRunServiceDuplicateRejectsBindMount(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{
				"Config": map[string]any{"Env": []string{}},
				"HostConfig": map[string]any{
					"Mounts": []map[string]any{{"Type": "bind", "Source": "/host/data", "Target": "/app/data"}},
				},
			})
		default:
			w.Write([]byte("{}"))
		}
	}))
	reg := newPeerRegistry(nil, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "peer-b", "dev", true)
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	_, err := runServiceDuplicate(context.Background(), dc, reg, nil, filepath.Join(t.TempDir(), "routes.json"), "app", DuplicateServiceRequest{Target: "peer-b"}, "")
	if err == nil || !strings.Contains(err.Error(), "bind mount") {
		t.Fatalf("err = %v, want a bind-mount refusal", err)
	}
}

// TestRunServiceDuplicateRequiresPeerSecret proves a target that resolves
// fine still fails clearly when DASHBOARD_PEER_SECRET isn't configured.
func TestRunServiceDuplicateRequiresPeerSecret(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
		}})
	}))
	reg := newPeerRegistry(nil, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "peer-b", "dev", true)
	t.Setenv("DASHBOARD_PEER_SECRET", "")

	_, err := runServiceDuplicate(context.Background(), dc, reg, nil, filepath.Join(t.TempDir(), "routes.json"), "app", DuplicateServiceRequest{Target: "peer-b"}, "")
	if err == nil || !strings.Contains(err.Error(), "DASHBOARD_PEER_SECRET") {
		t.Fatalf("err = %v, want a DASHBOARD_PEER_SECRET error", err)
	}
}

// TestPeerDuplicateRequestHasNoRawLabelPassthrough guards the "labels are
// built server-side from typed fields only" property structurally: there is
// no map field a caller could stuff arbitrary proxy.* labels into.
func TestPeerDuplicateRequestHasNoRawLabelPassthrough(t *testing.T) {
	rt := reflect.TypeOf(peerDuplicateRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() == reflect.Map {
			t.Fatalf("peerDuplicateRequest.%s is a %s — labels must be built server-side from typed fields only, never a raw passthrough map", f.Name, f.Type)
		}
	}
}

// TestPeerDuplicateHandlerRejectsNameConflict proves the receiving peer 409s
// when a container by that name already exists locally, rather than
// silently colliding or overwriting it.
func TestPeerDuplicateHandlerRejectsNameConflict(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			json.NewEncoder(w).Encode([]dockerContainer{{ID: "existing-id", Names: []string{"/app"}, State: "running"}})
			return
		}
		w.Write([]byte("{}"))
	}))
	h := peerDuplicateHandler("s3cret", "peer-b", dc, true)

	body, _ := json.Marshal(peerDuplicateRequest{
		Name: "app", Image: "ghcr.io/org/app:v1", Host: "app.example.com", Port: 8080, PublishPort: 18080,
	})
	req := httptest.NewRequest(http.MethodPost, "/peer/duplicate", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

// TestPeerDuplicateHandlerDisabledWithoutWrites proves the endpoint 404s
// (not 401/403) when writes aren't enabled — same convention as every other
// write-mesh handler in peers.go.
func TestPeerDuplicateHandlerDisabledWithoutWrites(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected docker call: %s", r.URL.Path)
	}))
	h := peerDuplicateHandler("s3cret", "peer-b", dc, false)
	req := httptest.NewRequest(http.MethodPost, "/peer/duplicate", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with writes disabled", rec.Code)
	}
}

// TestPeerDuplicateHandlerPublishesContainerPortNotHostPort proves the
// created container's ExposedPorts/PortBindings key off req.Port (the
// container-internal listen port) with HostPort set to req.PublishPort — not
// the other way around, which would silently misroute traffic whenever the
// two differ.
func TestPeerDuplicateHandlerPublishesContainerPortNotHostPort(t *testing.T) {
	var createBody map[string]any
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			json.NewDecoder(r.Body).Decode(&createBody)
			json.NewEncoder(w).Encode(map[string]string{"Id": "newid"})
		default:
			w.Write([]byte("{}"))
		}
	}))
	h := peerDuplicateHandler("s3cret", "peer-b", dc, true)

	body, _ := json.Marshal(peerDuplicateRequest{
		Name: "app", Image: "ghcr.io/org/app:v1", Host: "app.example.com", Port: 8080, PublishPort: 18080,
	})
	req := httptest.NewRequest(http.MethodPost, "/peer/duplicate", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	exposed, _ := createBody["ExposedPorts"].(map[string]any)
	if _, ok := exposed["8080/tcp"]; !ok {
		t.Fatalf("ExposedPorts = %v, want key 8080/tcp (container-internal port)", exposed)
	}
	hc, _ := createBody["HostConfig"].(map[string]any)
	bindings, _ := hc["PortBindings"].(map[string]any)
	binding, ok := bindings["8080/tcp"]
	if !ok {
		t.Fatalf("PortBindings = %v, want key 8080/tcp", bindings)
	}
	entries, _ := binding.([]any)
	if len(entries) != 1 {
		t.Fatalf("PortBindings[8080/tcp] = %v, want one entry", binding)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["HostPort"] != "18080" {
		t.Fatalf("HostPort = %v, want 18080 (the published port)", entry["HostPort"])
	}
}

// TestUpsertDuplicateRouteAppendsAndPreservesFields proves a second
// duplicate of the same service to a different backend appends rather than
// overwrites, and that auth/ratelimit_rpm round-trip through the file.
func TestUpsertDuplicateRouteAppendsAndPreservesFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := upsertDuplicateRoute(path, "app", "app.example.com", "", false, "app", "http://10.0.0.5:8080", "/healthz", true, []string{"alice"}, "basic", true, 60); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := upsertDuplicateRoute(path, "app", "app.example.com", "", false, "app", "http://10.0.0.6:8080", "/healthz", true, []string{"alice"}, "basic", true, 60); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	f, err := readRoutesFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(f.Routes) != 1 {
		t.Fatalf("routes = %d, want 1 (append, not a second entry)", len(f.Routes))
	}
	r := f.Routes[0]
	if len(r.Backends) != 2 || r.Backends[0] != "http://10.0.0.5:8080" || r.Backends[1] != "http://10.0.0.6:8080" {
		t.Fatalf("backends = %v, want both appended in order", r.Backends)
	}
	if r.DuplicateOf != "app" {
		t.Errorf("duplicate_of = %q, want app", r.DuplicateOf)
	}
	if r.Service != "app" {
		t.Errorf("service = %q, want app (label-managed backfill)", r.Service)
	}
	if !r.Auth || r.AuthMode != "basic" || !r.RateLimit || r.RateRPM != 60 {
		t.Errorf("auth/ratelimit fields not preserved: %+v", r)
	}
}

// TestRunServiceDuplicateOnboardedGuardViaStore proves the onboarded guard
// fires from the OnboardedStore even when Service.Onboarded (set only by
// buildManagedServices, which findService doesn't call) is false.
func TestRunServiceDuplicateOnboardedGuardViaStore(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
		}})
	}))
	onb := &OnboardedStore{items: []OnboardedService{{Name: "app"}}}
	_, err := runServiceDuplicate(context.Background(), dc, nil, onb, "", "app", DuplicateServiceRequest{Target: "peer-b"}, "")
	if err == nil || !strings.Contains(err.Error(), "onboarded") {
		t.Fatalf("err = %v, want an onboarded-service refusal", err)
	}
}

// TestRunServiceDuplicateSeparatesPortAndPublishPort proves the
// container-internal listen port (Port, becomes proxy.port on the peer) and
// the host-side published port (PublishPort, becomes the PortBindings
// HostPort and the routes.json backend port) are NOT collapsed into the same
// value when a caller requests a different publish port than the service's
// own internal port.
func TestRunServiceDuplicateSeparatesPortAndPublishPort(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{
				"Config":     map[string]any{"Env": []string{}},
				"HostConfig": map[string]any{"Mounts": []map[string]any{}},
			})
		default:
			w.Write([]byte("{}"))
		}
	}))

	var gotReq peerDuplicateRequest
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode peer request: %v", err)
		}
		json.NewEncoder(w).Encode(peerDuplicateResponse{Status: "created", Name: gotReq.Name, Port: gotReq.PublishPort})
	}))
	defer peer.Close()

	reg := newPeerRegistry(nil, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peer.URL, true, "peer-b", "dev", true)
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	routesPath := filepath.Join(t.TempDir(), "routes.json")
	resp, err := runServiceDuplicate(context.Background(), dc, reg, nil, routesPath, "app", DuplicateServiceRequest{Target: "peer-b", PublishPort: 18080}, "")
	if err != nil {
		t.Fatalf("runServiceDuplicate: %v", err)
	}

	if gotReq.Port != 8080 {
		t.Errorf("peer request Port = %d, want 8080 (container-internal, unchanged from the source service)", gotReq.Port)
	}
	if gotReq.PublishPort != 18080 {
		t.Errorf("peer request PublishPort = %d, want 18080 (the requested host-side port)", gotReq.PublishPort)
	}
	if resp.Port != 18080 {
		t.Errorf("response Port = %d, want 18080", resp.Port)
	}

	f, err := readRoutesFile(routesPath)
	if err != nil {
		t.Fatalf("read routes: %v", err)
	}
	if len(f.Routes) != 1 || len(f.Routes[0].Backends) != 1 || !strings.HasSuffix(f.Routes[0].Backends[0], ":18080") {
		t.Fatalf("routes = %+v, want a single backend on :18080", f.Routes)
	}
}

// TestUpsertDuplicateRouteIndependentOfOnboardedMarker proves a
// duplicate_of-tagged entry is invisible to removeOnboardedRoute/
// upsertOnboardedRoute, and vice versa — the two markers must never collide
// even when they share the same host+path.
func TestUpsertDuplicateRouteIndependentOfOnboardedMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := upsertOnboardedRoute(path, "app", "app.example.com", "", false, []string{"http://app:8080"}); err != nil {
		t.Fatalf("onboarded seed: %v", err)
	}
	if err := upsertDuplicateRoute(path, "app", "app.example.com", "", false, "app", "http://10.0.0.5:8080", "", false, nil, "", false, 0); err != nil {
		t.Fatalf("duplicate upsert: %v", err)
	}
	f, err := readRoutesFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(f.Routes) != 2 {
		t.Fatalf("routes = %d, want 2 (separate onboarded + duplicate entries): %+v", len(f.Routes), f.Routes)
	}
	if err := removeOnboardedRoute(path, "app"); err != nil {
		t.Fatalf("remove onboarded: %v", err)
	}
	f2, err := readRoutesFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if len(f2.Routes) != 1 || f2.Routes[0].DuplicateOf != "app" {
		t.Fatalf("after removeOnboardedRoute, routes = %+v, want only the duplicate entry left", f2.Routes)
	}
}

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// onboardFakeContainer is a stateful fake container used by the
// onboardContainer stub daemon below — richer than onboarded_test.go's
// fakeContainer (needs Labels and a HostConfig for inspectHostConfigUnknowns).
type onboardFakeContainer struct {
	id, name, image, state string
	env                    []string
	labels                 map[string]string
	hostConfig             map[string]any
}

// newOnboardFakeDockerServer stands in for the daemon across the full
// onboardContainer journey: listAll (name filter) → inspectHostConfigUnknowns
// → inspectEnv → inspectCloneSpec → N x (create, start) → stop/remove
// original.
func newOnboardFakeDockerServer(t *testing.T, seed *onboardFakeContainer) *dockerClient {
	t.Helper()
	var mu sync.Mutex
	containers := map[string]*onboardFakeContainer{seed.id: seed}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/"+dockerAPI)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && path == "/containers/json":
			var nameSubstr string
			if fq := r.URL.Query().Get("filters"); fq != "" {
				var f struct {
					Name []string `json:"name"`
				}
				_ = json.Unmarshal([]byte(fq), &f)
				if len(f.Name) > 0 {
					nameSubstr = f.Name[0]
				}
			}
			var out []map[string]any
			for _, c := range containers {
				if nameSubstr != "" && !strings.Contains(c.name, nameSubstr) {
					continue
				}
				out = append(out, map[string]any{
					"Id": c.id, "Names": []string{"/" + c.name}, "Image": c.image,
					"State": c.state, "Labels": c.labels,
				})
			}
			b, _ := json.Marshal(out)
			_, _ = w.Write(b)
		case r.Method == "GET" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			c, ok := containers[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			hc := c.hostConfig
			if hc == nil {
				hc = map[string]any{"NetworkMode": "edge"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Image":      "sha256:fakeimg",
				"Config":     map[string]any{"Env": c.env},
				"HostConfig": hc,
			})
		case r.Method == "GET" && strings.HasPrefix(path, "/images/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{}})
		case r.Method == "POST" && path == "/containers/create":
			name := r.URL.Query().Get("name")
			var body createBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := name + "-id"
			nc := &onboardFakeContainer{id: id, name: name, image: body.Image, env: body.Env, labels: body.Labels, state: "created"}
			containers[id] = nc
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case r.Method == "POST" && strings.HasSuffix(path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/start")
			if c, ok := containers[id]; ok {
				c.state = "running"
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && strings.HasSuffix(path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/stop")
			if c, ok := containers[id]; ok {
				c.state = "exited"
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			delete(containers, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return &dockerClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}
}

// TestOnboardContainerRelabelsAndRecreates is the end-to-end regression test
// for the Phase 3 rework: onboarding must relabel-and-recreate the container
// (N fresh label-managed replicas) rather than side-track it into
// OnboardedStore, and must remove the original once the replacements are up.
func TestOnboardContainerRelabelsAndRecreates(t *testing.T) {
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1",
		env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	})

	err := dc.onboardContainer(context.Background(), "myapp", OnboardRequest{
		Host: "myapp.example.org", Port: 8080, Replicas: 2,
	})
	if err != nil {
		t.Fatalf("onboardContainer: %v", err)
	}

	all, err := dc.listAll(context.Background(), "")
	if err != nil {
		t.Fatalf("listAll: %v", err)
	}
	var live []dockerContainer
	for _, c := range all {
		if c.Labels[labelService] == "myapp" {
			live = append(live, c)
		}
	}
	if len(live) != 2 {
		t.Fatalf("live label-managed replicas = %d, want 2: %+v", len(live), live)
	}
	for _, c := range live {
		if c.Labels[labelHost] != "myapp.example.org" {
			t.Errorf("labelHost = %q, want myapp.example.org", c.Labels[labelHost])
		}
		if c.Labels[labelPort] != "8080" {
			t.Errorf("labelPort = %q, want 8080", c.Labels[labelPort])
		}
		if c.Labels[labelEnable] != "true" {
			t.Errorf("labelEnable = %q, want true", c.Labels[labelEnable])
		}
	}
	// The original container must be gone.
	for _, c := range all {
		if c.name() == "myapp" && c.ID == "orig-id" {
			t.Fatalf("original container %q still present after onboarding", c.name())
		}
	}
}

// TestOnboardContainerRefusesHostConfigUnknowns proves Phase 1's fail-closed
// check gates onboarding: a container with e.g. PortBindings set must be
// refused, naming the field, with NO new containers created and the original
// left untouched.
func TestOnboardContainerRefusesHostConfigUnknowns(t *testing.T) {
	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1",
		env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
		hostConfig: map[string]any{
			"NetworkMode":  "edge",
			"PortBindings": map[string]any{"80/tcp": []map[string]any{{"HostPort": "8080"}}},
		},
	})

	err := dc.onboardContainer(context.Background(), "myapp", OnboardRequest{
		Host: "myapp.example.org", Port: 8080,
	})
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !strings.Contains(err.Error(), "PortBindings") {
		t.Fatalf("error = %v, want it to name PortBindings", err)
	}

	all, err := dc.listAll(context.Background(), "")
	if err != nil {
		t.Fatalf("listAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("containers = %d, want exactly the untouched original: %+v", len(all), all)
	}
	if all[0].ID != "orig-id" || all[0].State != "running" {
		t.Fatalf("original container mutated: %+v", all[0])
	}
}

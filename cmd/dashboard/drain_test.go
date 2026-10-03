package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// drainDocker is a fake daemon for the drain helpers: it records every
// mutating call as "METHOD /path?query" (API version stripped), lists
// containers for GETs, and lets a test script stop/DELETE responses.
type drainDocker struct {
	mu         sync.Mutex
	calls      []string
	containers []dockerContainer
	stopDelay  time.Duration
	stopStatus int
	rmStatus   int // for the non-force DELETE
	connStatus int
	created    []map[string]any
	onStop     func(uri string)
}

func (d *drainDocker) client(t *testing.T) *dockerClient {
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/"+dockerAPI)
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(d.containers)
			return
		case r.Method == http.MethodPost && strings.HasPrefix(uri, "/containers/create"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.mu.Lock()
			d.created = append(d.created, body)
			d.mu.Unlock()
			_, _ = io.WriteString(w, `{"Id":"new1"}`)
			return
		}
		d.mu.Lock()
		d.calls = append(d.calls, r.Method+" "+uri)
		stopDelay, stopStatus, rmStatus, connStatus := d.stopDelay, d.stopStatus, d.rmStatus, d.connStatus
		d.mu.Unlock()
		switch {
		case strings.Contains(uri, "/stop"):
			if d.onStop != nil {
				d.onStop(uri)
			}
			time.Sleep(stopDelay)
			if stopStatus != 0 {
				http.Error(w, "stop failed", stopStatus)
				return
			}
		case r.Method == http.MethodDelete && !strings.Contains(uri, "force=true") && rmStatus != 0:
			http.Error(w, "conflict", rmStatus)
			return
		case strings.Contains(uri, "/connect") && connStatus != 0:
			http.Error(w, "connect failed", connStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (d *drainDocker) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func drainCt(id, name string, labels map[string]string) dockerContainer {
	return dockerContainer{ID: id, Names: []string{"/" + name}, State: "running", Labels: labels}
}

func TestDrainSeconds(t *testing.T) {
	for _, c := range []struct {
		labels map[string]string
		want   int
	}{
		{nil, 30},
		{map[string]string{labelDrain: "7"}, 7},
		{map[string]string{labelDrain: " 12 "}, 12},
		{map[string]string{labelDrain: "0"}, 0},
		{map[string]string{labelDrain: "999"}, 300},
		{map[string]string{labelDrain: "-5"}, 0},
		{map[string]string{labelDrain: "soon"}, 30},
		{map[string]string{labelDrain: ""}, 30},
	} {
		if got := drainSeconds(c.labels); got != c.want {
			t.Errorf("drainSeconds(%v) = %d, want %d", c.labels, got, c.want)
		}
	}
}

func TestDrainStopRemoveStopsThenRemovesWithoutForce(t *testing.T) {
	d := &drainDocker{}
	dc := d.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a caller that already gave up must not cut the drain short
	if err := dc.drainStopRemove(ctx, drainCt("c1", "goproxy-app-1", map[string]string{labelDrain: "7"})); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /containers/c1/stop?t=7", "DELETE /containers/c1"}
	if got := d.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestDrainStopRemoveConflictFallsBackToForce(t *testing.T) {
	d := &drainDocker{rmStatus: http.StatusConflict}
	if err := d.client(t).drainStopRemove(context.Background(), drainCt("c1", "goproxy-app-1", nil)); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /containers/c1/stop?t=30", "DELETE /containers/c1", "DELETE /containers/c1?force=true"}
	if got := d.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestDrainStopRemoveStopFailureForceRemoves(t *testing.T) {
	d := &drainDocker{stopStatus: http.StatusInternalServerError}
	if err := d.client(t).drainStopRemove(context.Background(), drainCt("c1", "goproxy-app-1", nil)); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /containers/c1/stop?t=30", "DELETE /containers/c1?force=true"}
	if got := d.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// A rollback of a never-served container stays a plain force remove.
func TestCreateRollbackStillForceRemovesWithoutStop(t *testing.T) {
	d := &drainDocker{connStatus: http.StatusInternalServerError}
	_, err := d.client(t).createContainer(context.Background(), "goproxy-app-9", createBody{
		Image: "img", ExtraNetworks: []networkAttachment{{Name: "other"}},
	})
	if err == nil {
		t.Fatal("expected the connect failure to surface")
	}
	want := []string{"POST /networks/other/connect", "DELETE /containers/new1?force=true"}
	if got := d.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestCreateContainerStopTimeoutFromDrainLabel(t *testing.T) {
	d := &drainDocker{}
	dc := d.client(t)
	if _, err := dc.createContainer(context.Background(), "a", createBody{Image: "img", Labels: map[string]string{labelDrain: "999"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.createContainer(context.Background(), "b", createBody{Image: "img", Labels: map[string]string{labelService: "x"}}); err != nil {
		t.Fatal(err)
	}
	if got := d.created[0]["StopTimeout"]; got != float64(300) {
		t.Fatalf("labeled StopTimeout = %v, want 300 (clamped)", got)
	}
	if _, ok := d.created[1]["StopTimeout"]; ok {
		t.Fatal("an unlabeled container must keep Docker's default stop timeout")
	}
}

func TestScaleDownDrainsBeforeRemove(t *testing.T) {
	labels := map[string]string{labelService: "app", labelDrain: "12"}
	d := &drainDocker{containers: []dockerContainer{
		drainCt("c1", "goproxy-app-1", labels),
		drainCt("c2", "goproxy-app-2", labels),
	}}
	if err := d.client(t).scaleService(context.Background(), "app", 1); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /containers/c2/stop?t=12", "DELETE /containers/c2"}
	if got := d.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// Whole-service retires drain their members in parallel: three 200ms stops
// take ~200ms, not 600ms.
func TestParallelTeardownsOverlap(t *testing.T) {
	labels := map[string]string{labelService: "app", labelCanary: "true", labelDrain: "5"}
	three := []dockerContainer{
		drainCt("c1", "goproxy-app-c1", labels),
		drainCt("c2", "goproxy-app-c2", labels),
		drainCt("c3", "goproxy-app-c3", labels),
	}
	run := func(name string, f func(*dockerClient) error) {
		t.Run(name, func(t *testing.T) {
			d := &drainDocker{containers: three, stopDelay: 200 * time.Millisecond}
			start := time.Now()
			if err := f(d.client(t)); err != nil {
				t.Fatal(err)
			}
			if el := time.Since(start); el > 450*time.Millisecond {
				t.Fatalf("took %s — members were drained one after another", el)
			}
			calls := d.got()
			for _, id := range []string{"c1", "c2", "c3"} {
				stop, del := indexOf(calls, "POST /containers/"+id+"/stop?t=5"), indexOf(calls, "DELETE /containers/"+id)
				if stop < 0 || del < 0 || stop > del {
					t.Fatalf("%s: want stop?t=5 before a non-force DELETE, calls = %v", id, calls)
				}
			}
		})
	}
	run("discardCanary", func(dc *dockerClient) error { return dc.discardCanary(context.Background(), "app") })
	run("deleteService", func(dc *dockerClient) error {
		n, err := dc.deleteService(context.Background(), "app")
		if err == nil && n != 3 {
			t.Errorf("membersActed = %d, want 3", n)
		}
		return err
	})
}

func TestStopServiceMembersUsesDrainAndRunsParallel(t *testing.T) {
	d := &drainDocker{stopDelay: 200 * time.Millisecond}
	svc := Service{
		Members: []dockerContainer{
			drainCt("c1", "a-1", map[string]string{labelDrain: "4"}),
			drainCt("c2", "a-2", nil),
		},
		MemberSummaries: []ServiceMember{{Name: "a-1", ID: "c1", State: "running"}, {Name: "a-2", ID: "c2", State: "running"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	n, err := stopServiceMembers(ctx, d.client(t), svc)
	if err != nil || n != 2 {
		t.Fatalf("stopServiceMembers = %d, %v", n, err)
	}
	if el := time.Since(start); el > 350*time.Millisecond {
		t.Fatalf("took %s — members were stopped one after another", el)
	}
	calls := d.got()
	if indexOf(calls, "POST /containers/c1/stop?t=4") < 0 || indexOf(calls, "POST /containers/c2/stop?t=30") < 0 || len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// Legacy onboarded clones are static routes.json backends with no container
// ID for the proxy's kill-event drain, so the route must already exclude a
// retiring clone — and the proxy have reloaded it — by the time it is stopped.
func TestOnboardedDeroutesBeforeStop(t *testing.T) {
	old := onboardedDerouteDelay
	onboardedDerouteDelay = 0
	t.Cleanup(func() { onboardedDerouteDelay = old })
	refreshed := 0
	var mu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refreshed++
		mu.Unlock()
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("PROXY_URL", proxy.URL)

	routes := filepath.Join(t.TempDir(), "routes.json")
	store := newTestOnboardedStore(t)
	if err := store.Put(OnboardedService{Name: "app", Host: "app.example", Port: 80, Image: "img", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	d := &drainDocker{containers: []dockerContainer{
		drainCt("k1", "goproxy-onb-app-1", nil),
		drainCt("k2", "goproxy-onb-app-2", nil),
	}}
	var atStop string
	var refreshedAtStop int
	d.onStop = func(string) {
		b, _ := os.ReadFile(routes)
		atStop = string(b)
		mu.Lock()
		refreshedAtStop = refreshed
		mu.Unlock()
	}
	if err := d.client(t).scaleOnboarded(context.Background(), "app", 1, store, routes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(atStop, "goproxy-onb-app-1") || strings.Contains(atStop, "goproxy-onb-app-2") {
		t.Fatalf("routes.json when the clone was stopped = %s, want app-2 already gone", atStop)
	}
	if refreshedAtStop == 0 {
		t.Fatal("the proxy was not told to reload before the clone was stopped")
	}
	if got := d.got(); !reflect.DeepEqual(got, []string{"POST /containers/k2/stop?t=30", "DELETE /containers/k2"}) {
		t.Fatalf("calls = %v", got)
	}
}

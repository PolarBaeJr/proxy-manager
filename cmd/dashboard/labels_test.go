package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/labels"
)

// fakeLabelStore is an in-memory labelStore with the Lua script's
// semantics: request_id replay before CAS, wipe refusal without AllowInit,
// CAS on the per-service version, global counter init/INCR.
type fakeLabelStore struct {
	mu           sync.Mutex
	recs         map[string]labelRecord
	reqIDs       map[string]map[string]uint64
	global       uint64
	globalExists bool
	down         bool
	everLoaded   bool
	writes       []labelWrite
	snap         atomic.Pointer[labelSnapshot]
}

func newFakeLabelStore() *fakeLabelStore {
	s := &fakeLabelStore{recs: map[string]labelRecord{}, reqIDs: map[string]map[string]uint64{}, everLoaded: true}
	s.rebuildLocked()
	return s
}

// adopt seeds svc as already adopted at version 1.
func (s *fakeLabelStore) adopt(svc string, m map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[svc] = labelRecord{Version: 1, Labels: m}
	s.global++
	s.globalExists = true
	s.rebuildLocked()
}

func (s *fakeLabelStore) rebuildLocked() {
	if !s.globalExists && s.snap.Load() != nil && len(s.snap.Load().Services) > 0 {
		return // wiped: keep the last snapshot, like the real store
	}
	svcs := map[string]map[string]string{}
	for k, r := range s.recs {
		svcs[k] = r.Labels
	}
	s.snap.Store(&labelSnapshot{Version: s.global, Services: svcs})
}

func (s *fakeLabelStore) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

func (s *fakeLabelStore) rec(svc string) (labelRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[svc]
	return r, ok
}

func (s *fakeLabelStore) writeLog() []labelWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]labelWrite(nil), s.writes...)
}

func (s *fakeLabelStore) Get(_ context.Context, svc string) (labelRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return labelRecord{}, false, errors.New("dial tcp: connection refused")
	}
	r, ok := s.recs[svc]
	return r, ok, nil
}

func (s *fakeLabelStore) Write(_ context.Context, w labelWrite) (labelWriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return labelWriteResult{}, fmt.Errorf("%w: dial tcp: connection refused", errLabelsUnavailable)
	}
	if v, ok := s.reqIDs[w.Service][w.RequestID]; ok && w.RequestID != "" {
		return labelWriteResult{Version: v, Replayed: true}, nil
	}
	if !s.globalExists && !w.AllowInit {
		return labelWriteResult{}, errLabelsWiped
	}
	cur := s.recs[w.Service].Version
	if cur != w.IfVersion {
		return labelWriteResult{}, errLabelsVersionConflict{Current: cur}
	}
	m := map[string]string{}
	for k, v := range w.Labels {
		m[k] = v
	}
	prev := s.recs[w.Service].Labels
	s.recs[w.Service] = labelRecord{Version: cur + 1, Labels: m, Prev: prev, Imported: w.Imported, UpdatedBy: w.Actor}
	if w.RequestID != "" {
		if s.reqIDs[w.Service] == nil {
			s.reqIDs[w.Service] = map[string]uint64{}
		}
		s.reqIDs[w.Service][w.RequestID] = cur + 1
	}
	if s.globalExists {
		s.global++
	} else {
		s.global, s.globalExists = 1, true
	}
	s.writes = append(s.writes, w)
	return labelWriteResult{Version: cur + 1, Global: s.global}, nil
}

func (s *fakeLabelStore) Index(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errors.New("down")
	}
	var out []string
	for k := range s.recs {
		out = append(out, k)
	}
	return out, nil
}

func (s *fakeLabelStore) GlobalVersion(context.Context) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return 0, false, errors.New("dial tcp: connection refused")
	}
	return s.global, s.globalExists, nil
}

func (s *fakeLabelStore) Snapshot() *labelSnapshot { return s.snap.Load() }

func (s *fakeLabelStore) EverLoaded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everLoaded
}

func (s *fakeLabelStore) RedisOK() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.down
}

func (s *fakeLabelStore) Refresh(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.down {
		s.rebuildLocked()
	}
}

// proxyLabelsStub serves a proxy's GET /labels with body (and 200 for
// everything else, e.g. /refresh), and points PROXY_URL at it.
func proxyLabelsStub(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/labels" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PROXY_URL", srv.URL)
}

// proxyUnreachable points PROXY_URL at a closed port.
func proxyUnreachable(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	t.Setenv("PROXY_URL", u)
}

const proxyLabelsEmpty = `{"version":0,"source":"none","redis_ok":false,"services":{}}`

func labelsMux(t *testing.T, dc *dockerClient, reg *PeerRegistry) http.Handler {
	t.Helper()
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	return newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, newTestOnboardedStore(t), nil, nil, nil, nil, nil, reg, nil, nil, newRollingOpManager(dc))
}

func captureAudit(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	prev := auditF
	if err := openAuditLog(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auditF = prev })
	return func() string {
		b, _ := os.ReadFile(path)
		return string(b)
	}
}

func withLabels(dc *dockerClient, s *fakeLabelStore, writes bool) {
	dc.labels = s
	dc.labelsWrites = writes
}

func addMember(f *overlapFake, id, name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = dockerContainer{ID: id, Names: []string{"/" + name}, Image: "ghcr.io/org/app:v1", State: "running", Status: "Up 1 hour", Labels: labels}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
}

func TestEffectiveLabelsOverlayAndCopy(t *testing.T) {
	dc := &dockerClient{}
	raw := map[string]string{labelService: "app", labelWeight: "3", labelHealth: "/h", labelHost: "a.example", "proxy.ab.variant": "b"}
	got := dc.effectiveLabels(raw)
	if !reflect.DeepEqual(got, raw) {
		t.Fatalf("no store: %v, want a copy of raw", got)
	}
	got[labelWeight] = "9"
	if raw[labelWeight] != "3" {
		t.Fatal("effectiveLabels returned the raw map, not a copy")
	}
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelWeight: "5", labelCacheTTLForTest: "5s"})
	withLabels(dc, s, false)
	got = dc.effectiveLabels(raw)
	want := map[string]string{labelService: "app", labelWeight: "5", labelCacheTTLForTest: "5s", labelHost: "a.example", "proxy.ab.variant": "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effective = %v, want %v (health unset, identity/ab untouched)", got, want)
	}
	if raw[labelWeight] != "3" || raw[labelHealth] != "/h" {
		t.Errorf("raw mutated: %v", raw)
	}
	if other := dc.effectiveLabels(map[string]string{labelService: "other", labelWeight: "2"}); other[labelWeight] != "2" {
		t.Errorf("unadopted service changed: %v", other)
	}
	if !dc.labelsAdopted("app") || dc.labelsAdopted("other") {
		t.Error("labelsAdopted wrong")
	}
	if got := dc.effectiveDrainSeconds(map[string]string{labelService: "app", labelDrain: "9"}); got != defaultDrainSeconds {
		t.Errorf("effectiveDrainSeconds = %d, want the default (adopted map has no drain)", got)
	}
}

const labelCacheTTLForTest = "proxy.cache"

func TestListServicesUsesEffectiveLabels(t *testing.T) {
	raw := overlapLabels(map[string]string{labelUnscalable: "", labelOverlap: ""})
	f, dc := newOverlapFake(t, raw)
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelUnscalable: "true", labelOverlap: "true", labelWeight: "5", labelAutoUpdate: "true", labelGroup: "grp"})
	withLabels(dc, s, false)
	svcs, err := dc.listServices(context.Background())
	if err != nil || len(svcs) != 1 {
		t.Fatalf("listServices = %v, %v", svcs, err)
	}
	sv := svcs[0]
	if !sv.Unscalable || !sv.Overlap || sv.Weight != 5 || !sv.AutoUpdate || sv.Group != "grp" || sv.Labels[labelWeight] != "5" {
		t.Errorf("service = %+v, want the central values", sv)
	}
	if _, ok := sv.Members[0].Labels[labelUnscalable]; ok {
		t.Errorf("member labels = %v, want raw (no overlay baked in)", sv.Members[0].Labels)
	}
	if _, ok := f.list()[0].Labels[labelWeight]; ok {
		t.Error("raw container labels mutated")
	}
	if err := dc.guardUnscalable(context.Background(), "app", 2); err == nil {
		t.Error("guardUnscalable ignored a central proxy.unscalable")
	}
}

func TestDrainStopRemoveUsesEffectiveDrain(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelDrain: "40"}))
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelDrain: "7"})
	withLabels(dc, s, false)
	if err := dc.drainStopRemove(context.Background(), f.list()[0]); err != nil {
		t.Fatal(err)
	}
	if got := f.got(); len(got) == 0 || got[0] != "stop old?t=7" {
		t.Errorf("calls = %v, want stop with the central drain 7", got)
	}
	svc := Service{Members: []dockerContainer{{ID: "x", Labels: map[string]string{labelService: "app", labelDrain: "40"}}}}
	if got := memberDrainSeconds(dc, svc, "x"); got != 7 {
		t.Errorf("memberDrainSeconds = %d, want 7", got)
	}
}

// The badminton case: proxy.health never reached the containers, the owner
// sets it centrally, and overlap's health gate must use it — with no
// recreate to bake it in, and without copying it into the new container.
func TestOverlapGateUsesCentralHealth(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer probe.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(probe.URL, "http://"))

	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelPort: port}))
	f.setInspect(strings.Replace(overlapInspectHealthy, `,"Healthcheck":{"Test":["CMD","true"]}`, "", 1))
	f.setNewStatus("Up 1 second")
	f.newIP = "127.0.0.1"
	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err == nil {
		t.Fatal("without central labels the overlap replace should be refused (no health signal)")
	}
	f.assertNoCreate(t)

	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelUnscalable: "true", labelOverlap: "true", labelHealth: "/ready"})
	withLabels(dc, s, false)
	if hasHealthSignal(nil, f.list()[0].Labels) {
		t.Fatal("raw labels unexpectedly carry a health signal")
	}
	if !hasHealthSignal(nil, dc.effectiveLabels(f.list()[0].Labels)) {
		t.Fatal("effective labels have no health signal")
	}
	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err != nil {
		t.Fatalf("replaceService: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	if len(got) == 0 || got[0] != "/ready" {
		t.Errorf("probe paths = %v, want the central /ready", got)
	}
	created := f.createdBodies()
	if len(created) != 1 {
		t.Fatalf("created = %d, want 1", len(created))
	}
	if _, ok := created[0].Labels[labelHealth]; ok {
		t.Errorf("new container labels = %v, want raw (proxy.health not baked in)", created[0].Labels)
	}
}

// runReplicaRestart reads proxy.overlap from the central labels too.
func TestReplicaRestartUsesCentralOverlap(t *testing.T) {
	_, dc := newOverlapFake(t, overlapLabels(map[string]string{labelOverlap: ""}))
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelUnscalable: "true", labelOverlap: "true"})
	withLabels(dc, s, false)
	rom := newRollingOpManager(dc)
	rec := doJSONReq(overlapMux(t, dc, rom), "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"mode":"overlap"`) {
		t.Fatalf("restart = %d %s, want 202 overlap from the central labels", rec.Code, rec.Body)
	}
	waitRollingTerminal(t, rom, "app")
}

func TestLabelsAPIFirstAdoptImports(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelWeight: "3", labelOverlap: ""}))
	addMember(f, "m2", "app-2", overlapLabels(map[string]string{labelWeight: "3", labelOverlap: ""}))
	proxyLabelsStub(t, proxyLabelsEmpty)
	s := newFakeLabelStore()
	withLabels(dc, s, true)
	logged := captureAudit(t)
	mux := labelsMux(t, dc, nil)

	rec := doJSONReq(mux, "GET", "/api/services/app/labels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	var view labelsView
	decodeBody(t, rec, &view)
	if view.Managed || view.ImportPreview[labelWeight] != "3" || view.ImportPreview[labelUnscalable] != "true" || len(view.Conflicts) != 0 {
		t.Fatalf("view = %+v, want an unadopted preview with weight 3", view)
	}
	if !containsString(view.ReadonlyKeys, labelHost) {
		t.Errorf("readonly_keys = %v, want proxy.host", view.ReadonlyKeys)
	}

	rec = doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":0,"set":{"proxy.cache":"5s"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	var res labelsSetResponse
	decodeBody(t, rec, &res)
	if res.Version != 1 || !res.Imported || !reflect.DeepEqual(res.ChangedKeys, []string{"proxy.cache"}) {
		t.Errorf("response = %+v", res)
	}
	r, _ := s.rec("app")
	want := map[string]string{labelWeight: "3", labelUnscalable: "true", "proxy.cache": "5s"}
	if !reflect.DeepEqual(r.Labels, want) {
		t.Errorf("stored = %v, want %v", r.Labels, want)
	}
	if r.Imported["local"][labelWeight] != "3" {
		t.Errorf("imported meta = %v", r.Imported)
	}
	if f.createdBodies() != nil {
		t.Error("a label write created containers")
	}
	a := logged()
	if !strings.Contains(a, `"action":"service.labels_set"`) || !strings.Contains(a, "proxy.cache") || strings.Contains(a, "5s") {
		t.Errorf("audit = %s, want service.labels_set with key names only", a)
	}

	rec = doJSONReq(mux, "GET", "/api/services/app/labels", "")
	decodeBody(t, rec, &view)
	if !view.Managed || view.Version != 1 || view.Effective["proxy.cache"] != "5s" || len(view.Drift["local"]) == 0 {
		t.Errorf("view after adopt = %+v, want managed v1 with drift for proxy.cache", view)
	}
}

func TestLabelsAPIConflictsAndVersion(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelWeight: "3"}))
	addMember(f, "m2", "app-2", overlapLabels(map[string]string{labelWeight: "4"}))
	proxyLabelsStub(t, proxyLabelsEmpty)
	s := newFakeLabelStore()
	withLabels(dc, s, true)
	mux := labelsMux(t, dc, nil)

	rec := doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":0}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"conflicts"`) || !strings.Contains(rec.Body.String(), labelWeight) {
		t.Fatalf("POST = %d %s, want 409 with the weight conflict", rec.Code, rec.Body)
	}
	if len(s.writeLog()) != 0 {
		t.Fatal("conflicting import wrote")
	}
	rec = doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":0,"resolve_conflicts":{"proxy.weight":"2"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolved POST = %d %s", rec.Code, rec.Body)
	}
	if r, _ := s.rec("app"); r.Labels[labelWeight] != "2" {
		t.Errorf("weight = %q, want the resolution 2", r.Labels[labelWeight])
	}

	rec = doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":0,"set":{"proxy.weight":"6"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"current_version":1`) {
		t.Fatalf("stale POST = %d %s, want 409 current_version 1", rec.Code, rec.Body)
	}
	rec = doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":1,"request_id":"r1","set":{"proxy.weight":"6"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	rec = doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":1,"request_id":"r1","set":{"proxy.weight":"6"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replayed":true`) {
		t.Fatalf("replay = %d %s, want 200 replayed", rec.Code, rec.Body)
	}
	for _, body := range []string{
		`{"if_version":2,"set":{"proxy.host":"x.example"}}`,
		`{"if_version":2,"set":{"proxy.auth":"false"}}`,
		`{"if_version":2,"set":{"proxy.weight":"500"}}`,
		`{"if_version":2,"set":{"proxy.weight":"2"},"unset":["proxy.weight"]}`,
	} {
		if rec := doJSONReq(mux, "POST", "/api/services/app/labels", body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	for _, name := range []string{"index", "version"} {
		if rec := doJSONReq(mux, "POST", "/api/services/"+name+"/labels", `{"if_version":0}`); rec.Code != http.StatusBadRequest {
			t.Errorf("reserved %q = %d, want 400", name, rec.Code)
		}
	}
}

func TestLabelsAPIRefusals(t *testing.T) {
	t.Run("self", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		withLabels(dc, newFakeLabelStore(), true)
		prev := selfHostname
		selfHostname = func() (string, error) { return "old", nil }
		t.Cleanup(func() { selfHostname = prev })
		rec := doJSONReq(labelsMux(t, dc, nil), "POST", "/api/services/app/labels", `{"if_version":0}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST = %d %s, want 403", rec.Code, rec.Body)
		}
		f.assertNoCreate(t)
	})
	t.Run("docker error", func(t *testing.T) {
		dc := dockerStubAlwaysErrors(t)
		s := newFakeLabelStore()
		withLabels(dc, s, true)
		rec := doJSONReq(labelsMux(t, dc, nil), "POST", "/api/services/app/labels", `{"if_version":0}`)
		if rec.Code != http.StatusForbidden || len(s.writeLog()) != 0 {
			t.Fatalf("POST = %d %s, want 403 and no write", rec.Code, rec.Body)
		}
	})
	t.Run("store disabled", func(t *testing.T) {
		_, dc := newOverlapFake(t, overlapLabels(nil))
		rec := doJSONReq(labelsMux(t, dc, nil), "POST", "/api/services/app/labels", `{"if_version":0}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("POST = %d %s, want 503", rec.Code, rec.Body)
		}
	})
	t.Run("redis down", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		s := newFakeLabelStore()
		s.adopt("app", map[string]string{})
		s.setDown(true)
		withLabels(dc, s, true)
		rec := doJSONReq(labelsMux(t, dc, nil), "POST", "/api/services/app/labels", `{"if_version":1,"set":{"proxy.weight":"2"}}`)
		if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != errLabelsUnavailable.Error() {
			t.Fatalf("POST = %d %q, want 503 %q", rec.Code, rec.Body, errLabelsUnavailable)
		}
		if err := dc.setWeightLabel(context.Background(), "app", 3); err == nil {
			t.Fatal("setWeightLabel on an adopted service with Redis down succeeded")
		}
		f.assertNoCreate(t)
	})
	t.Run("writes disabled", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		s := newFakeLabelStore()
		s.adopt("app", map[string]string{})
		withLabels(dc, s, false)
		rec := doJSONReq(labelsMux(t, dc, nil), "POST", "/api/services/app/labels", `{"if_version":1,"set":{"proxy.weight":"2"}}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "LABELS_WRITES") {
			t.Fatalf("POST = %d %s, want 409 LABELS_WRITES", rec.Code, rec.Body)
		}
		if err := dc.setAutoUpdateLabel(context.Background(), "app", true); err == nil || !strings.Contains(err.Error(), "LABELS_WRITES") {
			t.Fatalf("setAutoUpdateLabel err = %v, want the LABELS_WRITES refusal", err)
		}
		f.assertNoCreate(t)
	})
	t.Run("weight while canary staged", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		addMember(f, "c1", "app-canary-1", overlapLabels(map[string]string{labelCanary: "true"}))
		s := newFakeLabelStore()
		s.adopt("app", map[string]string{})
		withLabels(dc, s, true)
		mux := labelsMux(t, dc, nil)
		if rec := doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":1,"set":{"proxy.weight":"2"}}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "canary") {
			t.Fatalf("weight POST = %d %s, want refused for the staged canary", rec.Code, rec.Body)
		}
		if rec := doJSONReq(mux, "POST", "/api/services/app/labels", `{"if_version":1,"set":{"proxy.cache":"5s"}}`); rec.Code != http.StatusOK {
			t.Fatalf("non-weight POST = %d %s, want 200 (no rollout guard)", rec.Code, rec.Body)
		}
	})
}

// A setter on an adopted service writes Redis instead of recreating.
func TestRedirectLabelSetterNoRecreate(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelAutoUpdate: "true"})
	withLabels(dc, s, true)
	logged := captureAudit(t)
	ctx := context.Background()
	if err := dc.setWeightLabel(ctx, "app", 3); err != nil {
		t.Fatal(err)
	}
	if err := dc.setAutoUpdateLabel(ctx, "app", false); err != nil {
		t.Fatal(err)
	}
	if err := dc.setUnscalableLabel(ctx, "app", true); err != nil {
		t.Fatal(err)
	}
	if err := dc.setWeightLabel(ctx, "app", 1); err != nil {
		t.Fatal(err)
	}
	f.assertNoCreate(t)
	r, _ := s.rec("app")
	if want := map[string]string{labelUnscalable: "true"}; !reflect.DeepEqual(r.Labels, want) || r.Version != 5 {
		t.Errorf("stored = v%d %v, want v5 %v", r.Version, r.Labels, want)
	}
	if a := logged(); !strings.Contains(a, "(via setWeightLabel)") || !strings.Contains(a, "(via setAutoUpdateLabel)") {
		t.Errorf("audit = %s, want the via-setter marks", a)
	}

	// Unadopted: the setter still recreates.
	_, dc2 := newOverlapFake(t, overlapLabels(map[string]string{labelOverlap: ""}))
	withLabels(dc2, newFakeLabelStore(), true)
	if handled, err := dc2.redirectLabelSetter(ctx, "app", "x", labelWeight, "2"); handled || err != nil {
		t.Errorf("unadopted redirect = %v, %v, want not handled", handled, err)
	}
}

// Adopt strictness (central env) keeps reading RAW labels: a StopTimeout
// matching the container's own proxy.drain is not a refused field even
// when the central drain differs.
func TestAdoptStrictnessReadsRawLabels(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Image":"","Config":{"StopTimeout":10,"Labels":{"proxy.service":"app","proxy.drain":"10"}},"HostConfig":{}}`))
	}))
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{labelDrain: "7"})
	withLabels(dc, s, false)
	facts, err := dc.inspectAdoptStrict(context.Background(), "c1", "json-file")
	if err != nil {
		t.Fatal(err)
	}
	if containsString(facts.refused, "Config.StopTimeout") {
		t.Errorf("refused = %v, want StopTimeout judged against the raw proxy.drain", facts.refused)
	}
}

func TestLabelsWipeSafety(t *testing.T) {
	wiped := func(snapshot map[string]map[string]string) *fakeLabelStore {
		s := newFakeLabelStore()
		s.snap.Store(&labelSnapshot{Version: 9, Services: snapshot, Wiped: true})
		return s
	}
	post := func(t *testing.T, dc *dockerClient) *httptest.ResponseRecorder {
		return doJSONReq(labelsMux(t, dc, nil), "POST", "/api/services/app/labels", `{"if_version":0,"set":{"proxy.cache":"5s"}}`)
	}
	t.Run("reseed from proxy", func(t *testing.T) {
		_, dc := newOverlapFake(t, overlapLabels(nil))
		s := wiped(map[string]map[string]string{"stale": {labelWeight: "9"}})
		withLabels(dc, s, true)
		proxyLabelsStub(t, `{"version":7,"source":"disk","redis_ok":true,"services":{"other":{"proxy.weight":"4"},"third":{}}}`)
		logged := captureAudit(t)
		if rec := post(t, dc); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "reseeded 2 service(s) from the proxy") {
			t.Fatalf("POST = %d %s", rec.Code, rec.Body)
		}
		if r, ok := s.rec("other"); !ok || r.Labels[labelWeight] != "4" {
			t.Errorf("other = %+v, want reseeded weight 4", r)
		}
		if _, ok := s.rec("third"); !ok {
			t.Error("third (adopted with no keys) not reseeded")
		}
		if _, ok := s.rec("stale"); ok {
			t.Error("reseeded from the snapshot although the proxy answered")
		}
		if r, _ := s.rec("app"); r.Labels["proxy.cache"] != "5s" {
			t.Errorf("app = %+v", r)
		}
		if !strings.Contains(logged(), `"action":"labels_reseed_from_proxy"`) {
			t.Errorf("audit = %s", logged())
		}
	})
	t.Run("reseed from snapshot", func(t *testing.T) {
		_, dc := newOverlapFake(t, overlapLabels(nil))
		s := wiped(map[string]map[string]string{"other": {labelWeight: "4"}})
		withLabels(dc, s, true)
		proxyUnreachable(t)
		logged := captureAudit(t)
		if rec := post(t, dc); rec.Code != http.StatusOK {
			t.Fatalf("POST = %d %s", rec.Code, rec.Body)
		}
		if r, ok := s.rec("other"); !ok || r.Labels[labelWeight] != "4" {
			t.Errorf("other = %+v, want reseeded from the snapshot", r)
		}
		if !strings.Contains(logged(), `"action":"labels_reseed_from_snapshot"`) {
			t.Errorf("audit = %s", logged())
		}
	})
	t.Run("genuine first write", func(t *testing.T) {
		_, dc := newOverlapFake(t, overlapLabels(nil))
		s := newFakeLabelStore()
		withLabels(dc, s, true)
		proxyLabelsStub(t, proxyLabelsEmpty)
		if rec := post(t, dc); rec.Code != http.StatusOK {
			t.Fatalf("POST = %d %s", rec.Code, rec.Body)
		}
		if w := s.writeLog(); len(w) != 1 || w[0].Service != "app" || !w[0].AllowInit {
			t.Errorf("writes = %+v, want exactly app with AllowInit", w)
		}
	})
	t.Run("unverifiable refuses", func(t *testing.T) {
		_, dc := newOverlapFake(t, overlapLabels(nil))
		s := newFakeLabelStore()
		withLabels(dc, s, true)
		proxyUnreachable(t)
		if rec := post(t, dc); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("POST = %d %s, want 503", rec.Code, rec.Body)
		}
		if len(s.writeLog()) != 0 {
			t.Error("wrote although nothing could vouch for the other services")
		}
	})
	t.Run("only the global counter lost", func(t *testing.T) {
		_, dc := newOverlapFake(t, overlapLabels(nil))
		s := newFakeLabelStore()
		s.adopt("other", map[string]string{labelWeight: "4"})
		s.mu.Lock()
		s.globalExists, s.global = false, 0
		s.mu.Unlock()
		s.snap.Store(&labelSnapshot{Version: 1, Services: map[string]map[string]string{"other": {labelWeight: "4"}}, Wiped: true})
		withLabels(dc, s, true)
		proxyLabelsStub(t, `{"version":1,"source":"disk","redis_ok":false,"services":{"other":{"proxy.weight":"4"}}}`)
		if rec := post(t, dc); rec.Code != http.StatusOK {
			t.Fatalf("POST = %d %s, want 200 (every meta survived)", rec.Code, rec.Body)
		}
		if _, exists, _ := s.GlobalVersion(context.Background()); !exists {
			t.Error("global counter still missing after the write")
		}
		if r, _ := s.rec("other"); r.Version != 1 || r.Labels[labelWeight] != "4" {
			t.Errorf("other = %+v, want untouched v1", r)
		}
	})
	t.Run("redirect after wipe reseeds then writes", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		s := wiped(map[string]map[string]string{"app": {labelWeight: "2"}, "other": {labelWeight: "4"}})
		withLabels(dc, s, true)
		proxyUnreachable(t)
		if err := dc.setWeightLabel(context.Background(), "app", 3); err != nil {
			t.Fatal(err)
		}
		f.assertNoCreate(t)
		if r, _ := s.rec("app"); r.Labels[labelWeight] != "3" {
			t.Errorf("app = %+v, want weight 3 after reseed", r)
		}
		if _, ok := s.rec("other"); !ok {
			t.Error("other not reseeded")
		}
	})
}

func TestAutoUpdateSkipsWhileLabelsNeverLoaded(t *testing.T) {
	old := autoUpdateGap
	autoUpdateGap = 0
	t.Cleanup(func() { autoUpdateGap = old })
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelAutoUpdate: "true", labelOverlap: ""}))
	s := newFakeLabelStore()
	s.everLoaded = false
	withLabels(dc, s, false)
	ic := newImageChecker(dc)
	ic.statuses["ghcr.io/org/app:v1"] = &imageStatus{Image: "ghcr.io/org/app:v1", UpdateAvailable: true}
	au := newAutoUpdater(dc, ic, newTestOnboardedStore(t), "", "", newAutoUpdateBlockStore(), nil, nil)
	au.runOnce(context.Background())
	f.assertNoCreate(t)
	if au.failures["app"] != 0 {
		t.Errorf("failures = %d, want the skip to cost nothing", au.failures["app"])
	}
	svcs, _ := dc.listServices(context.Background())
	if r := autoUpdateSkipReason(context.Background(), dc, svcs[0], ic.statuses["ghcr.io/org/app:v1"]); !strings.Contains(r, "central labels") {
		t.Errorf("skip reason = %q", r)
	}

	s.mu.Lock()
	s.everLoaded = true
	s.mu.Unlock()
	au.runOnce(context.Background())
	if len(f.createdBodies()) == 0 {
		t.Error("once loaded, the update never ran")
	}
}

func TestLabelsHostForwardingAndPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	s := newFakeLabelStore()
	s.adopt("app", map[string]string{})
	_, peerDC := newOverlapFake(t, overlapLabels(nil))
	withLabels(peerDC, s, true)
	proxyLabelsStub(t, proxyLabelsEmpty)
	logged := captureAudit(t)

	var gotPaths []string
	var mu sync.Mutex
	writes := true
	peerH := peerLabelsHandler("s3cret", peerDC, nil, true)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if !writes {
			peerLabelsHandler("s3cret", peerDC, nil, false).ServeHTTP(w, r)
			return
		}
		peerH.ServeHTTP(w, r)
	}))
	defer peerSrv.Close()
	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "host-a", "v", time.Second, nil)
	reg.recordResult(peerSrv.URL, true, "peer-b", "v", true, nil)

	_, localDC := newOverlapFake(t, overlapLabels(nil))
	withLabels(localDC, s, true)
	mux := labelsMux(t, localDC, reg)

	rec := doJSONReq(mux, "POST", "/api/services/app/labels?host=peer-b", `{"if_version":1,"set":{"proxy.sticky":"true"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forwarded POST = %d %s", rec.Code, rec.Body)
	}
	if r, _ := s.rec("app"); r.Labels["proxy.sticky"] != "true" {
		t.Errorf("stored = %v", r.Labels)
	}
	mu.Lock()
	if len(gotPaths) == 0 || gotPaths[0] != "POST /peer/labels/app" {
		t.Errorf("peer saw %v", gotPaths)
	}
	mu.Unlock()
	if a := logged(); !strings.Contains(a, `"user":"peer-mesh"`) || !strings.Contains(a, "(via forwarded)") {
		t.Errorf("audit = %s, want the peer-mesh actor marked forwarded", a)
	}

	rec = doJSONReq(mux, "GET", "/api/services/app/labels", "")
	var view labelsView
	decodeBody(t, rec, &view)
	if _, ok := view.ContainerLabels["peer-b"]; !ok || view.Applied["host-a"] == nil {
		t.Errorf("view = %+v, want both hosts", view)
	}

	writes = false
	rec = doJSONReq(mux, "POST", "/api/services/app/labels?host=peer-b", `{"if_version":2,"set":{"proxy.sticky":"false"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "does not support central labels") {
		t.Fatalf("POST to a peer without -peer-writes = %d %s, want 409", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("GET", "/peer/labels/app", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	peerLabelsHandler("s3cret", peerDC, nil, false).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("peer GET without writes = %d, want 200", w.Code)
	}
	req = httptest.NewRequest("GET", "/peer/labels/app", nil)
	w = httptest.NewRecorder()
	peerLabelsHandler("s3cret", peerDC, nil, true).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("peer GET without bearer = %d, want 401", w.Code)
	}
}

func TestMCPLabelsToolGating(t *testing.T) {
	names := func(writes, peer bool) map[string]bool {
		c, _ := stubDash(t, 200, `{}`)
		s := NewServer("t", "v")
		registerMCPTools(s, c, writes, peer)
		out := map[string]bool{}
		for _, tool := range s.toolList() {
			out[tool.Name] = true
		}
		return out
	}
	for _, c := range []struct{ writes, peer, want bool }{{false, false, false}, {true, false, false}, {false, true, false}, {true, true, true}} {
		got := names(c.writes, c.peer)
		if !got["get_service_labels"] {
			t.Errorf("writes=%v peer=%v: get_service_labels missing", c.writes, c.peer)
		}
		if got["set_service_labels"] != c.want {
			t.Errorf("writes=%v peer=%v: set_service_labels registered=%v, want %v", c.writes, c.peer, got["set_service_labels"], c.want)
		}
	}

	c, calls := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)
	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_service_labels","arguments":{"service":"app","host":"peer-b"}}}`)
	if r := res["result"].(map[string]any); r["isError"] != true || len(*calls) != 0 {
		t.Errorf("host without peer writes: %v, calls %v — want refused before any call", res, *calls)
	}

	c, calls = stubDash(t, 200, `{"version":1}`)
	s = NewServer("t", "v")
	registerMCPTools(s, c, true, true)
	res, _ = rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_service_labels","arguments":{"service":"app","if_version":0,"set":{"proxy.health":"/healthz"},"unset":["proxy.weight"],"request_id":"r1","allow_loosen":true}}}`)
	if r := res["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("set_service_labels errored: %v", r["content"])
	}
	if len(*calls) != 1 || (*calls)[0] != "POST /api/services/app/labels" {
		t.Errorf("calls = %v", *calls)
	}
	for _, tool := range s.toolList() {
		if tool.Name == "set_service_labels" {
			for _, want := range []string{"proxy.health", "proxy.overlap", "~5s", "applied", "proxy.auth", "if_version 0"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("description lacks %q", want)
				}
			}
		}
	}
}

// Readers on the hot path race the snapshot swap.
func TestEffectiveLabelsRace(t *testing.T) {
	s := newFakeLabelStore()
	dc := &dockerClient{}
	withLabels(dc, s, false)
	raw := map[string]string{labelService: "app", labelWeight: "2"}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = dc.effectiveLabels(raw)
				_ = dc.labelsAdopted("app")
			}
		}()
	}
	for j := 0; j < 50; j++ {
		s.adopt("app", map[string]string{labelWeight: fmt.Sprint(j%9 + 1)})
	}
	wg.Wait()
	if raw[labelWeight] != "2" {
		t.Error("raw mutated")
	}
}

func TestComputeLabelsImportInvalidValueIsConflict(t *testing.T) {
	imp := computeLabelsImport([]labelsHostView{{Host: "a", Members: []labelsMemberView{{Name: "m", Labels: map[string]string{labelWeight: "500", labelSticky: "true"}}}}})
	if imp.Preview[labels.Sticky] != "true" {
		t.Errorf("preview = %v", imp.Preview)
	}
	if len(imp.Conflicts) != 1 || imp.Conflicts[0].Key != labelWeight || imp.Conflicts[0].Reason == "" {
		t.Errorf("conflicts = %+v, want the invalid weight", imp.Conflicts)
	}
}

const labelSticky = "proxy.sticky"

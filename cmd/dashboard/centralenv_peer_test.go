package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// peerDo sends one request to a /peer/central-env/ handler.
func peerDo(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPeerCentralEnvAuthGates(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "s3cret")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	disabled := newTestCentralEnv(t, "dashboard-a")
	disabled.enabled = false

	cases := []struct {
		name    string
		handler http.Handler
		method  string
		path    string
		bearer  string
		want    int
	}{
		{"no secret", peerCentralEnvHandler("", h.ce, h.dc, true), "GET", "/peer/central-env/app?for=dashboard-b", "", http.StatusNotFound},
		{"feature off", peerCentralEnvHandler("s3cret", disabled, h.dc, true), "GET", "/peer/central-env/app?for=dashboard-b", "s3cret", http.StatusNotFound},
		{"nil resolver", peerCentralEnvHandler("s3cret", nil, h.dc, true), "GET", "/peer/central-env/app/status", "s3cret", http.StatusNotFound},
		{"env needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "GET", "/peer/central-env/app?for=dashboard-b", "s3cret", http.StatusNotFound},
		{"set needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "POST", "/peer/central-env/app/set", "s3cret", http.StatusNotFound},
		{"notify needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "POST", "/peer/central-env/app/notify", "s3cret", http.StatusNotFound},
		{"status without writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "GET", "/peer/central-env/app/status", "s3cret", http.StatusOK},
		{"wrong bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "GET", "/peer/central-env/app?for=dashboard-b", "nope", http.StatusUnauthorized},
		{"no bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "GET", "/peer/central-env/app/status", "", http.StatusUnauthorized},
		{"unknown action", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "GET", "/peer/central-env/app/adopt", "s3cret", http.StatusNotFound},
		{"adopt needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "POST", "/peer/central-env/app/adopt", "s3cret", http.StatusNotFound},
		{"release-request needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "POST", "/peer/central-env/app/release-request", "s3cret", http.StatusNotFound},
		{"adopt wrong bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "POST", "/peer/central-env/app/adopt", "nope", http.StatusUnauthorized},
		{"release-request no bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "POST", "/peer/central-env/app/release-request", "", http.StatusUnauthorized},
		{"view without writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "GET", "/peer/central-env/app/view", "s3cret", http.StatusOK},
		{"view no bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "GET", "/peer/central-env/app/view", "", http.StatusUnauthorized},
		{"view feature off", peerCentralEnvHandler("s3cret", disabled, h.dc, true), "GET", "/peer/central-env/app/view", "s3cret", http.StatusNotFound},
		{"set-request needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "POST", "/peer/central-env/app/set-request", "s3cret", http.StatusNotFound},
		{"set-request wrong bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "POST", "/peer/central-env/app/set-request", "nope", http.StatusUnauthorized},
		{"sync-request needs writes", peerCentralEnvHandler("s3cret", h.ce, h.dc, false), "POST", "/peer/central-env/app/sync-request", "s3cret", http.StatusNotFound},
		{"sync-request wrong bearer", peerCentralEnvHandler("s3cret", h.ce, h.dc, true), "POST", "/peer/central-env/app/sync-request", "nope", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := peerDo(t, c.handler, c.method, c.path, c.bearer, ""); rec.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestPeerCentralEnvGetOriginOnly: the value-bearing GET answers only on the
// origin, resolves the ASKING host's overrides, and carries the origin
// template's healthcheck.
func TestPeerCentralEnvGetOriginOnly(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "s3cret")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "central", "B": "base"},
		map[string]map[string]string{"dashboard-b": {"B": "over-b"}}, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=central", "B=base"}, cenvTemplateHealth)
	handler := peerCentralEnvHandler("s3cret", h.ce, h.dc, true)

	rec := peerDo(t, handler, "GET", "/peer/central-env/app?for=dashboard-b", "s3cret", "")
	var got peerCentralEnvResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got.Origin != "dashboard-a" || got.Version != 1 || strings.Join(got.Env, ",") != "A=central,B=over-b" || !reflect2(got.Healthcheck, cenvTemplateHealth) {
		t.Fatalf("got %+v", got)
	}
	if rec := peerDo(t, handler, "GET", "/peer/central-env/app", "s3cret", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing for= → %d", rec.Code)
	}

	peer := newSyncHost(t, "dashboard-b", nil, "s3cret")
	peer.ce.cache.Put("app", "dashboard-a", 1, []string{"A=central"})
	if rec := peerDo(t, peerCentralEnvHandler("s3cret", peer.ce, peer.dc, true), "GET", "/peer/central-env/app?for=dashboard-c", "s3cret", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-origin GET = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPeerCentralEnvSet(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "s3cret")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	handler := peerCentralEnvHandler("s3cret", h.ce, h.dc, true)

	rec := peerDo(t, handler, "POST", "/peer/central-env/app/set", "s3cret", `{"if_version":7,"set":{"A":"2"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"current_version":1`) {
		t.Fatalf("stale if_version = %d %s", rec.Code, rec.Body.String())
	}
	rec = peerDo(t, handler, "POST", "/peer/central-env/app/set", "s3cret", `{"request_id":"r1","if_version":1,"set":{"A":"2","NEW":"x"},"host_overrides":{"dashboard-b":{"B":"y"}}}`)
	var res centralEnvSetResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Version != 2 || strings.Join(res.ChangedKeys, ",") != "A,B,NEW" {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	rec = peerDo(t, handler, "POST", "/peer/central-env/app/set", "s3cret", `{"request_id":"r1","if_version":1,"set":{"A":"2","NEW":"x"}}`)
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || !res.Replayed || res.Version != 2 {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
	}
	rec = peerDo(t, handler, "POST", "/peer/central-env/app/set", "s3cret", `{"if_version":2,"set":{"T":"ref:MISSING"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unresolvable ref = %d %s", rec.Code, rec.Body.String())
	}
	rec = peerDo(t, handler, "POST", "/peer/central-env/app/set", "s3cret", `{"if_version":2,"unset":["NEW"],"unset_host_overrides":{"dashboard-b":["B"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unset = %d %s", rec.Code, rec.Body.String())
	}
	got, _, _ := h.ce.store.Get("app")
	if got.Version != 3 || len(got.Base) != 1 || got.Base["A"] != "2" || len(got.Overrides) != 0 {
		t.Fatalf("record = %+v", got)
	}
	peer := newSyncHost(t, "dashboard-b", nil, "s3cret")
	if rec := peerDo(t, peerCentralEnvHandler("s3cret", peer.ce, peer.dc, true), "POST", "/peer/central-env/app/set", "s3cret", `{"if_version":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("set on non-origin = %d", rec.Code)
	}
}

// TestPeerCentralEnvNotify: 409 on the origin itself, 200 no-op when this
// host is already at (or past) the version, 202 + a local job otherwise.
func TestPeerCentralEnvNotify(t *testing.T) {
	withFastSync(t)
	origin := newSyncHost(t, "dashboard-a", nil, "s3cret")
	origin.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	if rec := peerDo(t, peerCentralEnvHandler("s3cret", origin.ce, origin.dc, true), "POST", "/peer/central-env/app/notify", "s3cret", `{"origin":"dashboard-z","version":2}`); rec.Code != http.StatusConflict {
		t.Fatalf("notify on origin = %d", rec.Code)
	}

	h := newSyncHost(t, "dashboard-b", nil, "s3cret")
	h.ce.cache.Put("app", "dashboard-a", 1, []string{"A=1"})
	h.f.seedMember("b1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.ce.fetchFromOrigin = func(ctx context.Context, o, svc, id string) (centralEnvFetched, error) {
		if o != "dashboard-a" || id != "dashboard-b" {
			return centralEnvFetched{}, errors.New("wrong origin/identity")
		}
		return centralEnvFetched{Env: []string{"A=2"}, Version: 2}, nil
	}
	handler := peerCentralEnvHandler("s3cret", h.ce, h.dc, true)
	if rec := peerDo(t, handler, "POST", "/peer/central-env/app/notify", "s3cret", `{"origin":"dashboard-a","version":1}`); rec.Code != http.StatusOK {
		t.Fatalf("converged notify = %d", rec.Code)
	}
	if rec := peerDo(t, handler, "POST", "/peer/central-env/app/notify", "s3cret", `{"origin":"dashboard-z","version":5}`); rec.Code != http.StatusConflict {
		t.Fatalf("notify from a different origin = %d", rec.Code)
	}
	if rec := peerDo(t, handler, "POST", "/peer/central-env/app/notify", "s3cret", `{"origin":"dashboard-a","version":2}`); rec.Code != http.StatusAccepted {
		t.Fatalf("lagging notify = %d %s", rec.Code, rec.Body.String())
	}
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged || j.Role != envSyncRolePeer || j.Target != 2 {
		t.Fatalf("job = %+v", j)
	}
	for n, mv := range h.f.members() {
		if mv.version != "2" || !envHas(mv.env, "A=2") {
			t.Fatalf("%s = %+v", n, mv)
		}
	}
	rec := peerDo(t, handler, "GET", "/peer/central-env/app/status", "s3cret", "")
	var st peerCentralEnvStatus
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Role != envSyncRolePeer || st.Origin != "dashboard-a" || st.Version != 2 || !st.atLeast(2) || st.Job == nil {
		t.Fatalf("status = %+v", st)
	}
}

func TestPeerCentralEnvRelease(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-b", nil, "s3cret")
	h.ce.cache.Put("app", "dashboard-a", 1, []string{"A=1"})
	handler := peerCentralEnvHandler("s3cret", h.ce, h.dc, true)
	// No mesh here, so the origin can't confirm it is releasing: refused,
	// cache kept (the accepted path is TestMeshPeerReleaseNeedsOriginReleasing).
	if rec := peerDo(t, handler, "POST", "/peer/central-env/app/release", "s3cret", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("release = %d", rec.Code)
	}
	if _, ok := h.ce.cache.Get("app"); !ok {
		t.Fatal("cache dropped without the origin's confirmation")
	}
	origin := newSyncHost(t, "dashboard-a", nil, "s3cret")
	origin.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	if rec := peerDo(t, peerCentralEnvHandler("s3cret", origin.ce, origin.dc, true), "POST", "/peer/central-env/app/release", "s3cret", ""); rec.Code != http.StatusConflict {
		t.Fatalf("release on origin = %d", rec.Code)
	}
}

// TestPeerCentralEnvAdoptAndReleaseRequest: the forwarded-adopt and
// forwarded-release endpoints act on this host only — as the new origin,
// or as the origin being asked to release — and never answer 404 for
// anything but "unsupported".
func TestPeerCentralEnvAdoptAndReleaseRequest(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "s3cret")
	h.f.seedAdoptee("c1", "stack-app-1", []string{"X=x-val"}, cenvTemplateHealth, nil)
	handler := peerCentralEnvHandler("s3cret", h.ce, h.dc, true)

	if rec := peerDo(t, handler, "POST", "/peer/central-env/app/adopt", "s3cret", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d %s", rec.Code, rec.Body.String())
	}
	if rec := peerDo(t, handler, "POST", "/peer/central-env/proxy/adopt", "s3cret", "{}"); rec.Code != http.StatusForbidden {
		t.Fatalf("infra adopt = %d %s", rec.Code, rec.Body.String())
	}
	peer := newSyncHost(t, "dashboard-b", nil, "s3cret")
	peer.ce.cache.Put("app", "dashboard-a", 1, []string{"X=x-val"})
	peerHandler := peerCentralEnvHandler("s3cret", peer.ce, peer.dc, true)
	if rec := peerDo(t, peerHandler, "POST", "/peer/central-env/app/adopt", "s3cret", "{}"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "centrally managed from dashboard-a") {
		t.Fatalf("adopt of another origin's service = %d %s", rec.Code, rec.Body.String())
	}
	if rec := peerDo(t, peerHandler, "POST", "/peer/central-env/app/release-request", "s3cret", ""); rec.Code != http.StatusConflict {
		t.Fatalf("release-request with only a cache = %d %s", rec.Code, rec.Body.String())
	}
	noSync := peerCentralEnvHandler("s3cret", newTestCentralEnv(t, "dashboard-a"), h.dc, true)
	for _, action := range []string{"adopt", "release-request"} {
		if rec := peerDo(t, noSync, "POST", "/peer/central-env/app/"+action, "s3cret", "{}"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without sync = %d %s", action, rec.Code, rec.Body.String())
		}
	}

	rec := peerDo(t, handler, "POST", "/peer/central-env/app/adopt", "s3cret", "{}")
	var rep centralEnvAdoptReport
	json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != http.StatusOK || rep.Origin != "dashboard-a" || rep.Fingerprint == "" || h.ce.store.Has("app") {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
	}
	rec = peerDo(t, handler, "POST", "/peer/central-env/app/adopt", "s3cret", `{"dry_run":false,"ack_compose":true,"request_id":"r-peer","fingerprint":"`+rep.Fingerprint+`"}`)
	if rec.Code != http.StatusAccepted || !h.ce.store.HasRequestID("app", "r-peer") {
		t.Fatalf("execute = %d %s", rec.Code, rec.Body.String())
	}
	h.waitJob(t, "app")

	rec = peerDo(t, handler, "POST", "/peer/central-env/app/release-request", "s3cret", "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"releasing"`) {
		t.Fatalf("release-request = %d %s", rec.Code, rec.Body.String())
	}
	if j := h.waitJob(t, "app"); j.Status != envSyncStatusConverged || h.ce.store.Has("app") {
		t.Fatalf("release job = %+v", j)
	}
	if rec := peerDo(t, handler, "POST", "/peer/central-env/app/release-request", "s3cret", ""); rec.Code != http.StatusConflict {
		t.Fatalf("release-request on an unmanaged service = %d %s", rec.Code, rec.Body.String())
	}
}

// TestOriginFetcherOverMesh: newOriginFetcher resolves the origin by
// identity through the registry and decodes the origin's answer.
func TestOriginFetcherOverMesh(t *testing.T) {
	withFastSync(t)
	origin := newSyncHost(t, "dashboard-a", nil, "s3cret")
	origin.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, map[string]map[string]string{"dashboard-b": {"A": "b"}}, "test", "")
	srv := httptest.NewServer(peerCentralEnvHandler("s3cret", origin.ce, origin.dc, true))
	t.Cleanup(srv.Close)
	reg := newTestPeerRegistryFeatures("dashboard-b", srv.URL, "dashboard-a", true, []string{centralEnvFeature})

	got, err := newOriginFetcher(reg, "s3cret")(context.Background(), "dashboard-a", "app", "dashboard-b")
	if err != nil || got.Version != 1 || strings.Join(got.Env, ",") != "A=b" {
		t.Fatalf("fetch = %+v, %v", got, err)
	}
	if _, err := newOriginFetcher(reg, "s3cret")(context.Background(), "dashboard-x", "app", "dashboard-b"); err == nil {
		t.Fatal("an unknown origin identity was fetched from")
	}
	if _, err := newOriginFetcher(reg, "wrong")(context.Background(), "dashboard-a", "app", "dashboard-b"); err == nil {
		t.Fatal("a wrong secret fetched")
	}
}

// TestNonOriginRecreateCarriesOriginHealthcheck: a non-origin replica whose
// template has no healthcheck is created with the origin's (fetched, then
// cached), so it's health-gated the same way.
func TestNonOriginRecreateCarriesOriginHealthcheck(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-b", nil, "s3cret")
	h.f.seedMember("b1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, nil)
	h.ce.fetchFromOrigin = func(context.Context, string, string, string) (centralEnvFetched, error) {
		return centralEnvFetched{Env: []string{"A=1"}, Version: 1, Healthcheck: cenvTemplateHealth}, nil
	}
	if err := h.dc.scaleService(context.Background(), "app", 2); err != nil {
		t.Fatal(err)
	}
	c := h.f.createsSnapshot()
	if len(c) != 1 || !reflect2(c[0].body.Healthcheck, cenvTemplateHealth) {
		t.Fatalf("scaled replica healthcheck = %+v", c)
	}
	// Origin now unreachable: the cached healthcheck still applies.
	h.ce.fetchFromOrigin = func(context.Context, string, string, string) (centralEnvFetched, error) {
		return centralEnvFetched{}, errors.New("down")
	}
	if err := h.dc.scaleService(context.Background(), "app", 3); err != nil {
		t.Fatal(err)
	}
	c = h.f.createsSnapshot()
	if len(c) != 2 || !reflect2(c[1].body.Healthcheck, cenvTemplateHealth) {
		t.Fatalf("stale-cache replica healthcheck = %+v", c)
	}
	if entry, _ := h.ce.cache.Get("app"); !reflect2(entry.Healthcheck, cenvTemplateHealth) {
		t.Fatalf("cache healthcheck = %+v", entry.Healthcheck)
	}
}

// TestPeerSpreadLiveBranchAppliesHealthcheck: a repeat spread to a host that
// already runs the service scales it up with the shipped healthcheck when
// the local template has none.
func TestPeerSpreadLiveBranchAppliesHealthcheck(t *testing.T) {
	withFastSync(t)
	f := newCenvFakeDocker()
	f.seedMember("b1", "goproxy-app-1", "", 0, []string{"A=1"}, nil)
	dc := f.client(t)
	srv := httptest.NewServer(peerSpreadHandler("s3cret", "dashboard-b", dc, true))
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(peerSpreadRequest{Service: "app", Image: "ghcr.io/org/app:v1", Host: "app.example", Port: 8080, Replicas: 2, Healthcheck: cenvTemplateHealth, Env: []string{"A=1"}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	c := f.createsSnapshot()
	if len(c) != 1 || !reflect2(c[0].body.Healthcheck, cenvTemplateHealth) {
		t.Fatalf("live-branch replica = %+v", c)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Adopt with a PEER as the origin (?host=), and release sent to a
// non-origin — both forwarded over the peer mesh.

// forwardedAdoptDryRun is a dry run on via's API that makes target the
// origin.
func forwardedAdoptDryRun(t *testing.T, via *meshNode, target string) centralEnvAdoptReport {
	t.Helper()
	rec := apiDo(t, via.mux, "POST", "/api/services/app/env/adopt?host="+target, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forwarded dry run = %d %s", rec.Code, rec.Body.String())
	}
	var rep centralEnvAdoptReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// adoptExecBody acks everything rep requires and imports X (dashboard-a's
// only peer-only key in adoptMesh) as base; extra is spliced in.
func adoptExecBody(rep centralEnvAdoptReport, fingerprint, extra string) string {
	body := `{"dry_run":false`
	if fingerprint != "" {
		body += `,"fingerprint":"` + fingerprint + `"`
	}
	for _, a := range rep.RequiredAcks {
		body += `,"` + a + `":true`
	}
	return body + `,"import":{"dashboard-a":{"keys":["X"],"as":"base"}}` + extra + `}`
}

func assertNothingAdopted(t *testing.T, a, b *meshNode) {
	t.Helper()
	if a.ce.store.Has("app") || b.ce.store.Has("app") || len(a.f.createsSnapshot()) != 0 || len(b.f.createsSnapshot()) != 0 {
		t.Fatal("something was adopted")
	}
}

// TestMeshAdoptForwardedDryRunReportsPeerAsOrigin: a dry run on A naming B
// runs on B — B is the report's origin, A is the peer diffed against it —
// and changes nothing anywhere. Naming A itself is just a local dry run.
func TestMeshAdoptForwardedDryRunReportsPeerAsOrigin(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forwarded dry run = %d %s", rec.Code, rec.Body.String())
	}
	for _, v := range []string{"x-val", "y-val", "z-val", "from-a", "from-b", "shared-val"} {
		if strings.Contains(rec.Body.String(), v) {
			t.Fatalf("forwarded dry run leaked a value: %s", rec.Body.String())
		}
	}
	var rep centralEnvAdoptReport
	json.Unmarshal(rec.Body.Bytes(), &rep)
	d := rep.Env.Hosts["dashboard-a"]
	if rep.Origin != "dashboard-b" || d == nil || !reflect.DeepEqual(d.PeerOnly, []string{"X"}) || !reflect.DeepEqual(d.OriginOnly, []string{"Y", "Z"}) || !reflect.DeepEqual(d.Differs, []string{"DIFF"}) {
		t.Fatalf("origin=%s diff=%+v", rep.Origin, d)
	}
	if !reflect.DeepEqual(rep.Unresolved["dashboard-a"], []string{"X"}) || !containsString(rep.RequiredAcks, adoptAckNoHealth) || rep.Fingerprint == "" {
		t.Fatalf("unresolved=%v acks=%v", rep.Unresolved, rep.RequiredAcks)
	}
	if b.adoptCalls.Load() != 1 {
		t.Fatalf("B saw %d forwarded adopts, want 1", b.adoptCalls.Load())
	}
	assertNothingAdopted(t, a, b)

	local := forwardedAdoptDryRun(t, a, "dashboard-a")
	if local.Origin != "dashboard-a" || b.adoptCalls.Load() != 1 || a.adoptCalls.Load() != 0 {
		t.Fatalf("?host=self origin=%s b calls=%d a calls=%d", local.Origin, b.adoptCalls.Load(), a.adoptCalls.Load())
	}
	if d := local.Env.Hosts["dashboard-b"]; d == nil || !reflect.DeepEqual(d.PeerOnly, []string{"Y", "Z"}) {
		t.Fatalf("?host=self diff = %+v", d)
	}
}

// TestMeshAdoptForwardedExecuteMakesPeerOrigin: executed from A, the adopt
// lands on B — B holds the record, A caches it, and every replica on both
// hosts is stamped from B. A request_id is minted on A before forwarding,
// so replaying it replays on B.
func TestMeshAdoptForwardedExecuteMakesPeerOrigin(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	rep := forwardedAdoptDryRun(t, a, "dashboard-b")
	body := adoptExecBody(rep, rep.Fingerprint, "")
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", body)
	var res centralEnvAdoptResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusAccepted || res.Origin != "dashboard-b" || res.RequestID == "" || !b.ce.store.HasRequestID("app", res.RequestID) {
		t.Fatalf("forwarded execute = %d %s", rec.Code, rec.Body.String())
	}
	if j := b.waitJob(t, "app"); j.Status != envSyncStatusConverged {
		t.Fatalf("B job = %+v", j)
	}
	a.waitJob(t, "app")
	if a.ce.store.Has("app") || !b.ce.store.Has("app") {
		t.Fatal("the record is not on B alone")
	}
	if c, ok := a.ce.cache.Get("app"); !ok || c.Origin != "dashboard-b" {
		t.Fatalf("A cache = %+v", c)
	}
	for _, n := range []*meshNode{a, b} {
		members := n.f.appMembers()
		if len(members) != 1 {
			t.Fatalf("%s members = %v", n.identity, members)
		}
		for name, mb := range members {
			env := append([]string(nil), mb.env...)
			sort.Strings(env)
			if name == "stack-app-1" || mb.labels[labelEnvOrigin] != "dashboard-b" || hasComposeLabel(mb.labels) || !envHas(env, "X=x-val") || !envHas(env, "Y=y-val") {
				t.Fatalf("%s %s = %+v", n.identity, name, mb)
			}
		}
	}

	creates := len(a.f.createsSnapshot()) + len(b.f.createsSnapshot())
	replay := adoptExecBody(rep, rep.Fingerprint, `,"request_id":"`+res.RequestID+`"`)
	rec = apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", replay)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"replayed":true`) {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
	}
	b.waitJob(t, "app")
	a.waitJob(t, "app")
	if len(a.f.createsSnapshot())+len(b.f.createsSnapshot()) != creates {
		t.Fatal("a replayed forwarded adopt rolled again")
	}
}

// TestMeshAdoptForwardedRefusalRelayed: B's own 409 (with its fresh report
// and the request_id) reaches the caller as-is.
func TestMeshAdoptForwardedRefusalRelayed(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	rep := forwardedAdoptDryRun(t, a, "dashboard-b")
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", adoptExecBody(rep, "0000", ""))
	var refused struct {
		Error     string                `json:"error"`
		RequestID string                `json:"request_id"`
		Report    centralEnvAdoptReport `json:"report"`
	}
	json.Unmarshal(rec.Body.Bytes(), &refused)
	if rec.Code != http.StatusConflict || refused.Report.Origin != "dashboard-b" || refused.RequestID == "" || !strings.Contains(refused.Error, "changed since the dry run") {
		t.Fatalf("bad fingerprint = %d %s", rec.Code, rec.Body.String())
	}
	rec = apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", adoptExecBody(rep, "", ""))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "fingerprint is required") {
		t.Fatalf("no fingerprint = %d %s", rec.Code, rec.Body.String())
	}
	assertNothingAdopted(t, a, b)
}

// TestMeshAdoptForwardedTimeoutRetrySameRequestID: B adopts but answers too
// late → A says "outcome unknown" (504) with the request_id; retrying with
// it replays on B instead of adopting again.
func TestMeshAdoptForwardedTimeoutRetrySameRequestID(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	rep := forwardedAdoptDryRun(t, a, "dashboard-b")
	centralEnvAdoptForwardTimeout = 50 * time.Millisecond
	b.adoptDelay.Store(int64(300 * time.Millisecond))

	body := adoptExecBody(rep, rep.Fingerprint, `,"request_id":"rid-a"`)
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", body)
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), "rid-a") {
		t.Fatalf("slow origin = %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "the late adopt to land on B", func() bool { return b.ce.store.Has("app") })
	b.adoptDelay.Store(0)
	centralEnvAdoptForwardTimeout = 5 * time.Second
	rec = apiDo(t, a.mux, "POST", "/api/services/app/env/adopt?host=dashboard-b", body)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"replayed":true`) || !strings.Contains(rec.Body.String(), `"request_id":"rid-a"`) {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	if r, _, _ := b.ce.store.Get("app"); r.Version != 1 {
		t.Fatalf("B at v%d after a retried adopt — want exactly one", r.Version)
	}
	b.waitJob(t, "app")
	a.waitJob(t, "app")
}

func TestMeshAdoptForwardedErrors(t *testing.T) {
	withFastSync(t)
	for _, tc := range []struct {
		name  string
		svc   string
		host  string
		setup func(t *testing.T, a, b *meshNode)
		code  int
		want  string
	}{
		{name: "unknown host", host: "dashboard-z", code: http.StatusNotFound, want: "unknown host dashboard-z"},
		{name: "unreachable", setup: func(t *testing.T, a, b *meshNode) {
			b.stop()
			t.Cleanup(func() { b.start(t) })
		}, code: http.StatusServiceUnavailable, want: "dashboard-b is unreachable"},
		{name: "peer writes off", setup: func(t *testing.T, a, b *meshNode) {
			b.setPeerMux(b.peerMuxFor(b.ce, false))
		}, code: http.StatusConflict, want: "does not support forwarded adopt"},
		{name: "infra service", svc: "proxy", code: http.StatusConflict, want: "infrastructure service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := adoptMesh(t)
			if tc.setup != nil {
				tc.setup(t, a, b)
			}
			svc, host := tc.svc, tc.host
			if svc == "" {
				svc = "app"
			}
			if host == "" {
				host = "dashboard-b"
			}
			rec := apiDo(t, a.mux, "POST", "/api/services/"+svc+"/env/adopt?host="+host, `{}`)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("= %d %s, want %d %q", rec.Code, rec.Body.String(), tc.code, tc.want)
			}
			assertNothingAdopted(t, a, b)
		})
	}
}

// TestMeshReleaseFromPeerForwardsToOrigin: a release sent to B starts the
// release on A (the origin), which then unstamps both hosts. A ?host= that
// isn't the origin is refused, never redirected.
func TestMeshReleaseFromPeerForwardsToOrigin(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	stillManaged := func(what string) {
		t.Helper()
		if r, ok, _ := a.ce.store.Get("app"); !ok || r.State == centralEnvStateReleasing || a.m.busy("app") {
			t.Fatalf("%s: A's record changed: %+v", what, r)
		}
	}

	rec := apiDo(t, b.mux, "POST", "/api/services/app/env/release?host=dashboard-b", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "origin is dashboard-a, not dashboard-b") {
		t.Fatalf("release ?host=wrong origin = %d %s", rec.Code, rec.Body.String())
	}
	stillManaged("wrong ?host")

	a.setPeerMux(a.peerMuxFor(a.ce, false))
	rec = apiDo(t, b.mux, "POST", "/api/services/app/env/release", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "does not support forwarded release") {
		t.Fatalf("release with origin writes off = %d %s", rec.Code, rec.Body.String())
	}
	stillManaged("origin writes off")
	a.setPeerMux(a.peerMuxFor(a.ce, true))

	a.stop()
	rec = apiDo(t, b.mux, "POST", "/api/services/app/env/release", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "origin dashboard-a is unreachable") {
		t.Fatalf("release with origin down = %d %s", rec.Code, rec.Body.String())
	}
	a.start(t)
	stillManaged("origin down")

	before := a.releaseReqCalls.Load()
	rec = apiDo(t, b.mux, "POST", "/api/services/app/env/release?host=dashboard-a", "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"releasing"`) {
		t.Fatalf("release via B = %d %s", rec.Code, rec.Body.String())
	}
	if n := a.releaseReqCalls.Load() - before; n != 1 {
		t.Fatalf("A saw %d release-requests, want 1", n)
	}
	if j := a.waitJob(t, "app"); j.Status != envSyncStatusConverged {
		t.Fatalf("A release job = %+v", j)
	}
	b.waitJob(t, "app")
	if a.ce.store.Has("app") {
		t.Fatal("record not deleted")
	}
	if _, ok := b.ce.cache.Get("app"); ok {
		t.Fatal("B kept its cache")
	}
	for _, n := range []*meshNode{a, b} {
		for name, mb := range n.f.appMembers() {
			if mb.labels[labelEnvOrigin] != "" || mb.labels[labelEnvVersion] != "" {
				t.Fatalf("%s %s still stamped", n.identity, name)
			}
		}
	}
}

// TestMeshPeerEndpointsNeverForward: the peer /adopt and /release-request
// act on the host they land on — a ?host= there is ignored, and a
// release-request on a non-origin is refused rather than passed on.
func TestMeshPeerEndpointsNeverForward(t *testing.T) {
	withFastSync(t)
	t.Run("adopt", func(t *testing.T) {
		a, b := adoptMesh(t)
		rec := peerDo(t, b.peerMux, "POST", "/peer/central-env/app/adopt?host=dashboard-a", "s3cret", `{}`)
		var rep centralEnvAdoptReport
		json.Unmarshal(rec.Body.Bytes(), &rep)
		if rec.Code != http.StatusOK || rep.Origin != "dashboard-b" || a.adoptCalls.Load() != 0 {
			t.Fatalf("peer adopt = %d origin=%s a calls=%d", rec.Code, rep.Origin, a.adoptCalls.Load())
		}
	})
	t.Run("release-request", func(t *testing.T) {
		a, b, _ := newMesh(t, true)
		seedConverged(a, b)
		rec := peerDo(t, b.peerMux, "POST", "/peer/central-env/app/release-request", "s3cret", "")
		if rec.Code != http.StatusConflict || a.releaseReqCalls.Load() != 0 {
			t.Fatalf("release-request on non-origin = %d %s (a calls=%d)", rec.Code, rec.Body.String(), a.releaseReqCalls.Load())
		}
		if r, _, _ := a.ce.store.Get("app"); r.State == centralEnvStateReleasing {
			t.Fatal("origin started releasing")
		}
	})
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// deletionTracker records every DELETE /images/<token> path a dockerClient
// stub saw, guarded by a mutex — same explicit-synchronization convention as
// the rest of this package's test doubles (authredis_test.go,
// onboard_container_test.go).
type deletionTracker struct {
	mu   sync.Mutex
	hits []string
}

func (d *deletionTracker) record(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hits = append(d.hits, path)
}

func (d *deletionTracker) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.hits...)
}

// imagesWriteDockerStub is imagesDockerStub (peers_test.go) plus DELETE
// /images/<token> support (dc.removeImage) — same one-service ("app"),
// two-image fixture: v1 is running (container c1 references it, so it's
// protected) and v2 is unprotected/deletable.
func imagesWriteDockerStub(t *testing.T, deletes *deletionTracker) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/images/"):
			deletes.record(r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			json.NewEncoder(w).Encode([]dockerImage{
				{Id: "sha256:aaa111", RepoTags: []string{"ghcr.io/org/app:v1"}, Size: 100000000, Created: 2000},
				{Id: "sha256:bbb222", RepoTags: []string{"ghcr.io/org/app:v2"}, Size: 50000000, Created: 1000},
			})
		default:
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image: "ghcr.io/org/app:v1", ImageID: "sha256:aaa111",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
			}})
		}
	}))
}

// imagesWriteTestStores is the standard set of fresh on-disk-backed stores
// every test below needs — factored out purely to keep each test body short.
func imagesWriteTestStores(t *testing.T) (*ReleasesStore, *ImageHistoryStore, *OnboardedStore) {
	t.Helper()
	rs, err := loadReleasesStore(filepath.Join(t.TempDir(), "releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	ih, err := loadImageHistoryStore(filepath.Join(t.TempDir(), "image-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	return rs, ih, onb
}

func setInternalToken(t *testing.T) {
	t.Helper()
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })
}

// TestImagesMarkUnmarkForwardToPeer proves POST /api/images/mark?host=<peer>
// and DELETE /api/images/mark?host=<peer> reach the peer's own ReleasesStore
// (not this host's), and never touch the local docker daemon.
func TestImagesMarkUnmarkForwardToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerDeletes := &deletionTracker{}
	peerDC := imagesWriteDockerStub(t, peerDeletes)
	peerRS, peerIH, peerOnb := imagesWriteTestStores(t)
	peerSrv := httptest.NewServer(peerImagesMutateHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb, true))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)

	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	markReq := httptest.NewRequest(http.MethodPost, "/api/images/mark?host=dashboard-b",
		strings.NewReader(`{"service":"app","tag":"v2","label":"pin"}`))
	markReq.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, markReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark: status = %d, body %s", rec.Code, rec.Body.String())
	}

	all := peerRS.All()
	marks, ok := all["ghcr.io/org/app"]
	if !ok || len(marks) != 1 || marks[0].Tag != "v2" {
		t.Fatalf("peer ReleasesStore.All() = %+v, want a v2 mark for ghcr.io/org/app", all)
	}
	if got := localRS.All(); len(got) != 0 {
		t.Errorf("local ReleasesStore.All() = %+v, want empty — mark must land on the peer, not here", got)
	}

	unmarkReq := httptest.NewRequest(http.MethodDelete, "/api/images/mark?host=dashboard-b",
		strings.NewReader(`{"service":"app","tag":"v2"}`))
	unmarkReq.Header.Set("Authorization", "Bearer "+internalToken)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, unmarkReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmark: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := peerRS.All()["ghcr.io/org/app"]; len(got) != 0 {
		t.Errorf("peer ReleasesStore after unmark = %+v, want empty", got)
	}

	if localHit.Load() {
		t.Error("local docker stub was hit — mark/unmark?host=<peer> must not touch the local daemon")
	}
	if len(peerDeletes.all()) != 0 {
		t.Errorf("peer image-removal endpoint was hit by a mark/unmark request: %v", peerDeletes.all())
	}
}

// TestImagesDeleteForwardsToPeer proves DELETE /api/images/delete?host=<peer>
// deletes on the peer's own daemon (identified by service+ref, never a
// DeleteToken) and never touches the local one.
func TestImagesDeleteForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerDeletes := &deletionTracker{}
	peerDC := imagesWriteDockerStub(t, peerDeletes)
	peerRS, peerIH, peerOnb := imagesWriteTestStores(t)
	peerSrv := httptest.NewServer(peerImagesMutateHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb, true))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)

	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodDelete, "/api/images/delete?host=dashboard-b",
		strings.NewReader(`{"service":"app","ref":"ghcr.io/org/app:v2"}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	hits := peerDeletes.all()
	if len(hits) != 1 || !strings.Contains(hits[0], "v2") {
		t.Fatalf("peer deletion hits = %v, want exactly one containing v2", hits)
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — delete?host=<peer> must not touch the local daemon")
	}
}

// TestImagesDeletePeerRejectsUnsafeClaim proves the peer independently
// re-validates protection and refuses to delete an image it knows is
// protected (v1, referenced by a running container), even though the
// forwarding request names it as the target — the peer never trusts the
// requester's belief about what's safe.
func TestImagesDeletePeerRejectsUnsafeClaim(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerDeletes := &deletionTracker{}
	peerDC := imagesWriteDockerStub(t, peerDeletes)
	peerRS, peerIH, peerOnb := imagesWriteTestStores(t)
	peerSrv := httptest.NewServer(peerImagesMutateHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb, true))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodDelete, "/api/images/delete?host=dashboard-b",
		strings.NewReader(`{"service":"app","ref":"ghcr.io/org/app:v1"}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s, want %d (peer must reject a protected image)", rec.Code, rec.Body.String(), http.StatusConflict)
	}
	if len(peerDeletes.all()) != 0 {
		t.Errorf("peer image-removal endpoint was hit for a protected image: %v", peerDeletes.all())
	}
}

// TestImagesPruneForwardsToPeer proves POST /api/images/prune?host=<peer>
// prunes against the peer's own live Docker state (keeping the protected,
// running v1; deleting the unprotected v2) and never touches the local
// daemon.
func TestImagesPruneForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerDeletes := &deletionTracker{}
	peerDC := imagesWriteDockerStub(t, peerDeletes)
	peerRS, peerIH, peerOnb := imagesWriteTestStores(t)
	peerSrv := httptest.NewServer(peerImagesMutateHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb, true))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)

	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/images/prune?host=dashboard-b",
		strings.NewReader(`{"service":"app","keep_n":0}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Deleted        []string `json:"deleted"`
		Failed         []any    `json:"failed"`
		ReclaimedBytes int64    `json:"reclaimed_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Deleted) != 1 || !strings.Contains(out.Deleted[0], "v2") {
		t.Fatalf("deleted = %v, want exactly one entry containing v2 (v1 is protected)", out.Deleted)
	}
	if out.ReclaimedBytes != 50000000 {
		t.Errorf("reclaimed_bytes = %d, want %d", out.ReclaimedBytes, 50000000)
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — prune?host=<peer> must not touch the local daemon")
	}
}

// TestImagesMutationPeerWritesDisabled proves a peer with -peer-writes=false
// answers 404 to every /peer/images/* mutation — mark, unmark, delete, and
// prune alike — and that 404 is relayed through the full /api/images/*?host=
// request path (mapPeerMutationErr's documented "not found vs writes
// disabled" ambiguity).
func TestImagesMutationPeerWritesDisabled(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerDC := imagesWriteDockerStub(t, &deletionTracker{})
	peerRS, peerIH, peerOnb := imagesWriteTestStores(t)
	peerSrv := httptest.NewServer(peerImagesMutateHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb, false))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"mark", http.MethodPost, "/api/images/mark?host=dashboard-b", `{"service":"app","tag":"v2","label":"pin"}`},
		{"unmark", http.MethodDelete, "/api/images/mark?host=dashboard-b", `{"service":"app","tag":"v2"}`},
		{"delete", http.MethodDelete, "/api/images/delete?host=dashboard-b", `{"service":"app","ref":"ghcr.io/org/app:v2"}`},
		{"prune", http.MethodPost, "/api/images/prune?host=dashboard-b", `{"service":"app","keep_n":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+internalToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
			}
		})
	}
}

// TestImagesMutationUnknownHost proves a ?host= that doesn't match any known
// peer identity 404s without attempting to reach anything.
func TestImagesMutationUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev", true)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/images/prune?host=nonexistent-host",
		strings.NewReader(`{"service":"","keep_n":0}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestImagesMutationNoRegistry proves a ?host= with a nil registry 404s.
func TestImagesMutationNoRegistry(t *testing.T) {
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/images/prune?host=anything",
		strings.NewReader(`{"service":"","keep_n":0}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestImagesMutationPeerUnreachable proves an unreachable peer surfaces as
// 502, via peerMutate's transport-level error path through
// mapPeerMutationErr.
func TestImagesMutationPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(url, true, "dashboard-b", "dev", true)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/images/prune?host=dashboard-b",
		strings.NewReader(`{"service":"","keep_n":0}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
}

// TestImagesMutationPeerAuthRejected proves a peer's own 401 (mesh-secret
// mismatch) is mapped to 502, never relayed verbatim — a raw 401 here would
// incorrectly pop the requesting browser's own "session expired" handling.
func TestImagesMutationPeerAuthRejected(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "peer's own auth error body", http.StatusUnauthorized)
	}))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/images/prune?host=dashboard-b",
		strings.NewReader(`{"service":"","keep_n":0}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	if strings.Contains(rec.Body.String(), "peer's own auth error body") {
		t.Errorf("body = %q, must not contain the peer's own auth-failure body", rec.Body.String())
	}
}

// TestImagesMutationsLocalStillWork is the no-?host= regression test: mark,
// unmark, delete, and prune must all still work exactly as before the
// forwarding branch was added.
func TestImagesMutationsLocalStillWork(t *testing.T) {
	deletes := &deletionTracker{}
	dc := imagesWriteDockerStub(t, deletes)
	rs, ih, onb := imagesWriteTestStores(t)
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, rs, nil, ih, nil, nil, nil)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(http.MethodPost, "/api/images/mark", `{"service":"app","tag":"v2","label":"pin"}`); rec.Code != http.StatusOK {
		t.Fatalf("mark: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if marks := rs.All()["ghcr.io/org/app"]; len(marks) != 1 || marks[0].Tag != "v2" {
		t.Fatalf("ReleasesStore.All() = %+v, want a v2 mark", rs.All())
	}

	if rec := do(http.MethodDelete, "/api/images/mark", `{"service":"app","tag":"v2"}`); rec.Code != http.StatusOK {
		t.Fatalf("unmark: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rs.All()["ghcr.io/org/app"]; len(got) != 0 {
		t.Errorf("ReleasesStore after unmark = %+v, want empty", got)
	}

	if rec := do(http.MethodDelete, "/api/images/delete", `{"token":"ghcr.io/org/app:v2"}`); rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body %s", rec.Code, rec.Body.String())
	}
	hits := deletes.all()
	if len(hits) != 1 || !strings.Contains(hits[0], "v2") {
		t.Fatalf("deletion hits = %v, want exactly one containing v2", hits)
	}

	if rec := do(http.MethodDelete, "/api/images/delete", `{"token":"ghcr.io/org/app:v1"}`); rec.Code != http.StatusConflict {
		t.Fatalf("delete of protected v1: status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusConflict)
	}
}

// TestMintForwardedActorRoundTripsWithAuditUser proves the two halves of
// actor forwarding agree on header name, secret, and assertion format —
// mintForwardedActor's output is exactly what auditUser expects to verify.
// Every forwarding test above authenticates with internalToken, under which
// actor is always "" and mintForwardedActor never fires; without this test
// the entire forwarding path added to actor.go and api.go has zero executed
// coverage.
func TestMintForwardedActorRoundTripsWithAuditUser(t *testing.T) {
	withActorSecret(t, testActorSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/images/mark?host=dashboard-b", nil)
	assertion := mintForwardedActor(req, "alice")
	if assertion == "" {
		t.Fatal("mintForwardedActor returned empty for a known actor with a secret configured")
	}

	verifyReq := httptest.NewRequest(http.MethodPost, "/peer/images/mark", nil)
	verifyReq.Header.Set(actorHeader, assertion)
	if got := auditUser(verifyReq, "peer-mesh"); got != "alice (via peer-mesh)" {
		t.Fatalf("auditUser = %q, want %q", got, "alice (via peer-mesh)")
	}
}

// TestMintForwardedActorEmptyWhenUnknown proves an absent actor stays an
// absent header rather than becoming a placeholder assertion — the same
// property cmd/proxy/auth.go's stampActor guards.
func TestMintForwardedActorEmptyWhenUnknown(t *testing.T) {
	withActorSecret(t, testActorSecret)
	req := httptest.NewRequest(http.MethodPost, "/api/images/mark?host=dashboard-b", nil)
	if got := mintForwardedActor(req, ""); got != "" {
		t.Fatalf("mintForwardedActor(_, \"\") = %q, want empty", got)
	}
}

// TestImagesMutationForwardsActorAssertion proves a token-authenticated
// mutation reaches the peer carrying a verifiable X-Pmgr-Actor assertion
// naming the real user, end to end through forwardImageMutation and
// peerMutate — not mintForwardedActor/auditUser called directly in isolation.
func TestImagesMutationForwardsActorAssertion(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withActorSecret(t, testActorSecret)

	peerDC := imagesWriteDockerStub(t, &deletionTracker{})
	peerRS, peerIH, peerOnb := imagesWriteTestStores(t)
	inner := peerImagesMutateHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb, true)

	var headerMu sync.Mutex
	var gotHeader string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerMu.Lock()
		gotHeader = r.Header.Get(actorHeader)
		headerMu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localRS, localIH, localOnb := imagesWriteTestStores(t)
	ic := newImageChecker(localDC)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	raw, _, err := auth.CreateToken("alice", "ci")
	if err != nil {
		t.Fatal(err)
	}

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), ic, "", nil, localOnb, localRS, nil, localIH, nil, nil, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/images/mark?host=dashboard-b",
		strings.NewReader(`{"service":"app","tag":"v2","label":"pin"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	headerMu.Lock()
	header := gotHeader
	headerMu.Unlock()
	if header == "" {
		t.Fatal("peer never received an X-Pmgr-Actor header for a token-authenticated request")
	}
	claims, ok := sso.VerifyActor(header, testActorSecret)
	if !ok || claims.Username != "alice" {
		t.Fatalf("VerifyActor(header) = %+v, ok=%v, want Username=alice", claims, ok)
	}
}

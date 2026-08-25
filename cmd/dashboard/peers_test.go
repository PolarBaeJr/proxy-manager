package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPeerHandshakeHandlerValidSecret(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "dashboard-a", "42")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"peer":"dashboard-a"`) || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %q, want peer=dashboard-a ok=true", body)
	}
	if !strings.Contains(body, `"version":"42"`) {
		t.Fatalf("body = %q, want version=42", body)
	}
}

func TestPeerHandshakeHandlerWrongSecret(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "dashboard-a", "dev")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestPeerHandshakeHandlerSameLengthWrongSecret uses a bearer value the same
// length as the real secret so the handler's length short-circuit can't
// reject it — this is the case that must actually reach
// subtle.ConstantTimeCompare rather than a bare == comparison.
func TestPeerHandshakeHandlerSameLengthWrongSecret(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "dashboard-a", "dev")
	for _, bad := range []string{"Bearer s3creX", "Bearer X3cret"} {
		req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
		req.Header.Set("Authorization", bad)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Authorization=%q: status = %d, want %d", bad, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestPeerHandshakeHandlerMissingHeader(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "dashboard-a", "dev")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestPeerHandshakeHandlerEmptySecretDisabled mirrors cmd/proxy/peers.go's
// peerHandshakeHandler: an unconfigured secret hides the endpoint entirely
// (404) rather than accepting/rejecting bearer tokens.
func TestPeerHandshakeHandlerEmptySecretDisabled(t *testing.T) {
	h := peerHandshakeHandler("", "dashboard-a", "dev")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerHandshakeHandlerWrongMethod(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "dashboard-a", "dev")
	req := httptest.NewRequest(http.MethodGet, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestPeerRegistryRecordsSuccess(t *testing.T) {
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "dashboard-b", "dev"))
	defer srv.Close()

	reg := newPeerRegistry([]string{srv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.send(context.Background(), srv.URL)

	st := reg.Status()[srv.URL]
	if !st.OK {
		t.Fatal("expected OK status after a successful handshake")
	}
	if st.LastAttempt.IsZero() {
		t.Fatal("expected LastAttempt to be recorded")
	}
	if st.LastSuccess.IsZero() {
		t.Fatal("expected LastSuccess to be recorded")
	}
}

func TestPeerRegistryRecordsFailureOnWrongSecret(t *testing.T) {
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "dashboard-b", "dev"))
	defer srv.Close()

	reg := newPeerRegistry([]string{srv.URL}, "wrong-secret", "dashboard-a", "dev", 0, nil)
	reg.send(context.Background(), srv.URL)

	st := reg.Status()[srv.URL]
	if st.OK {
		t.Fatal("expected non-OK status after a rejected handshake")
	}
	if st.LastAttempt.IsZero() {
		t.Fatal("expected LastAttempt to be recorded even on failure")
	}
	if !st.LastSuccess.IsZero() {
		t.Fatal("expected LastSuccess to stay unset on failure")
	}
}

func TestPeerRegistryRecordsFailureOnUnreachablePeer(t *testing.T) {
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "dashboard-b", "dev"))
	url := srv.URL
	srv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.send(context.Background(), url)

	st := reg.Status()[url]
	if st.OK {
		t.Fatal("expected non-OK status for an unreachable peer")
	}
	if st.LastAttempt.IsZero() {
		t.Fatal("expected LastAttempt to be recorded even when unreachable")
	}
}

func TestPeerRegistrySendCapturesVersion(t *testing.T) {
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "dashboard-b", "77"))
	defer srv.Close()

	reg := newPeerRegistry([]string{srv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.send(context.Background(), srv.URL)

	st := reg.Status()[srv.URL]
	if st.Version != "77" {
		t.Fatalf("Version = %q, want %q", st.Version, "77")
	}
	if st.Identity != "dashboard-b" {
		t.Fatalf("Identity = %q, want %q", st.Identity, "dashboard-b")
	}
}

func TestParseVersionsDropsDevAndGarbage(t *testing.T) {
	got := parseVersions(map[string]string{"a": "1", "b": "dev", "c": "xyz", "d": "5"})
	want := map[int]bool{1: true, 5: true}
	if len(got) != len(want) {
		t.Fatalf("parseVersions = %v, want %v", got, want)
	}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("parseVersions = %v, unexpected value %d", got, v)
		}
	}
}

func TestMeshFloorFromEmpty(t *testing.T) {
	v, ok := meshFloorFrom(nil)
	if ok || v != 0 {
		t.Fatalf("meshFloorFrom(nil) = (%d, %v), want (0, false)", v, ok)
	}
}

func TestMeshFloorFromComputesMinimum(t *testing.T) {
	v, ok := meshFloorFrom([]int{5, 2, 9})
	if !ok || v != 2 {
		t.Fatalf("meshFloorFrom([5,2,9]) = (%d, %v), want (2, true)", v, ok)
	}
}

func TestPeerServiceStatusHandlerValidSecret(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
		}})
	}))
	h := peerServiceStatusHandler("s3cret", "dashboard-b", dc, "")
	req := httptest.NewRequest(http.MethodGet, "/peer/service-status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body peerServiceStatusResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Identity != "dashboard-b" {
		t.Errorf("Identity = %q, want %q", body.Identity, "dashboard-b")
	}
	if len(body.Status.Groups) != 1 || body.Status.Groups[0].Machine != "" {
		t.Errorf("Status = %+v, want one group with Machine unset (peer never tags its own local groups)", body.Status)
	}
}

func TestPeerServiceStatusHandlerWrongSecret(t *testing.T) {
	h := peerServiceStatusHandler("s3cret", "dashboard-b", nil, "")
	req := httptest.NewRequest(http.MethodGet, "/peer/service-status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerServiceStatusHandlerEmptySecretDisabled(t *testing.T) {
	h := peerServiceStatusHandler("", "dashboard-b", nil, "")
	req := httptest.NewRequest(http.MethodGet, "/peer/service-status", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerServiceStatusHandlerWrongMethod(t *testing.T) {
	h := peerServiceStatusHandler("s3cret", "dashboard-b", nil, "")
	req := httptest.NewRequest(http.MethodPost, "/peer/service-status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestFetchPeerServiceStatusTagsMachineAndMerges(t *testing.T) {
	dcB := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-b-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "svc-b", labelHost: "b.example", labelPort: "80"},
		}})
	}))
	srvB := httptest.NewServer(peerServiceStatusHandler("s3cret", "dashboard-b", dcB, ""))
	defer srvB.Close()

	reg := newPeerRegistry([]string{srvB.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	groups := fetchPeerServiceStatus(context.Background(), reg, "s3cret")
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want 1", groups)
	}
	if groups[0].Machine != "dashboard-b" {
		t.Errorf("Machine = %q, want %q", groups[0].Machine, "dashboard-b")
	}
}

func TestFetchPeerServiceStatusSkipsUnreachablePeer(t *testing.T) {
	srv := httptest.NewServer(peerServiceStatusHandler("s3cret", "dashboard-b", nil, ""))
	url := srv.URL
	srv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	groups := fetchPeerServiceStatus(context.Background(), reg, "s3cret")
	if groups != nil {
		t.Fatalf("groups = %+v, want nil for an unreachable peer", groups)
	}
}

func TestFetchPeerServiceStatusNoPeersConfigured(t *testing.T) {
	reg := newPeerRegistry(nil, "s3cret", "dashboard-a", "dev", 0, nil)
	if got := fetchPeerServiceStatus(context.Background(), reg, "s3cret"); got != nil {
		t.Fatalf("groups = %+v, want nil with no peers configured", got)
	}
}

func TestPeerServicesHandlerValidSecret(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
		}})
	}))
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	h := peerServicesHandler("s3cret", "dashboard-b", dc, onb, ic)
	req := httptest.NewRequest(http.MethodGet, "/peer/services", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body peerServicesResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Identity != "dashboard-b" {
		t.Errorf("Identity = %q, want %q", body.Identity, "dashboard-b")
	}
	if len(body.Services) != 1 || body.Services[0].Machine != "" {
		t.Errorf("Services = %+v, want one service with Machine unset (peer never tags its own local services)", body.Services)
	}
}

func TestPeerServicesHandlerWrongSecret(t *testing.T) {
	h := peerServicesHandler("s3cret", "dashboard-b", nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/services", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerServicesHandlerEmptySecretDisabled(t *testing.T) {
	h := peerServicesHandler("", "dashboard-b", nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/services", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerServicesHandlerWrongMethod(t *testing.T) {
	h := peerServicesHandler("s3cret", "dashboard-b", nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/peer/services", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestFetchPeerServicesTagsMachineAndMerges(t *testing.T) {
	dcB := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-b-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "svc-b", labelHost: "b.example", labelPort: "80"},
		}})
	}))
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dcB)
	srvB := httptest.NewServer(peerServicesHandler("s3cret", "dashboard-b", dcB, onb, ic))
	defer srvB.Close()

	reg := newPeerRegistry([]string{srvB.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	svcs := fetchPeerServices(context.Background(), reg, "s3cret")
	if len(svcs) != 1 {
		t.Fatalf("svcs = %+v, want 1", svcs)
	}
	if svcs[0].Machine != "dashboard-b" {
		t.Errorf("Machine = %q, want %q", svcs[0].Machine, "dashboard-b")
	}
}

func TestFetchPeerServicesSkipsUnreachablePeer(t *testing.T) {
	srv := httptest.NewServer(peerServicesHandler("s3cret", "dashboard-b", nil, nil, nil))
	url := srv.URL
	srv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	svcs := fetchPeerServices(context.Background(), reg, "s3cret")
	if svcs != nil {
		t.Fatalf("svcs = %+v, want nil for an unreachable peer", svcs)
	}
}

func TestURLForIdentityFound(t *testing.T) {
	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "42")

	url, ok := reg.URLForIdentity("dashboard-b")
	if !ok || url != "http://peer-b:8098" {
		t.Fatalf("URLForIdentity(dashboard-b) = (%q, %v), want (http://peer-b:8098, true)", url, ok)
	}
}

func TestURLForIdentityNotFound(t *testing.T) {
	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "42")

	if url, ok := reg.URLForIdentity("dashboard-c"); ok {
		t.Fatalf("URLForIdentity(dashboard-c) = (%q, %v), want ok=false", url, ok)
	}
}

// TestRatchetOwnVersionSkipsDevAndNilRedis: no miniredis-backed *redis.Client
// test double exists in this codebase (authredis_test.go uses a fake
// txRunner interface, not a real *redis.Client), so this only confirms the
// nil-safe/no-op guard clauses don't panic or block.
func TestRatchetOwnVersionSkipsDevAndNilRedis(t *testing.T) {
	reg := newPeerRegistry(nil, "", "id", "dev", 0, nil)
	reg.ratchetOwnVersion(context.Background())
}

// imagesDockerStub answers /containers/json (listAll and listServices both
// hit this, with different query strings — the stub ignores the query, same
// as every other dockerStub in this package) and /images/json (listImages),
// giving buildImagesInfo enough on-disk data to produce at least one
// unprotected, tagged entry (a non-empty DeleteToken to assert on).
func imagesDockerStub(t *testing.T) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
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

func TestPeerImagesHandlerValidSecret(t *testing.T) {
	dc := imagesDockerStub(t)
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
	h := peerImagesHandler("s3cret", "dashboard-b", dc, rs, ih, onb)
	req := httptest.NewRequest(http.MethodGet, "/peer/images", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body peerImagesResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Identity != "dashboard-b" {
		t.Errorf("Identity = %q, want %q", body.Identity, "dashboard-b")
	}
	if body.Images == nil || body.Images.Machine != "" {
		t.Errorf("Images = %+v, want non-nil with Machine unset (peer never tags its own local data)", body.Images)
	}
}

func TestPeerImagesHandlerWrongSecret(t *testing.T) {
	h := peerImagesHandler("s3cret", "dashboard-b", nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/images", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerImagesHandlerEmptySecretDisabled(t *testing.T) {
	h := peerImagesHandler("", "dashboard-b", nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/images", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerImagesHandlerWrongMethod(t *testing.T) {
	h := peerImagesHandler("s3cret", "dashboard-b", nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/peer/images", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestPeerLogsContainersHandlerValidSecret(t *testing.T) {
	dc := logsContainersStub(t, nil)
	h := peerLogsContainersHandler("s3cret", "dashboard-b", dc)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body peerLogsContainersResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Identity != "dashboard-b" {
		t.Errorf("Identity = %q, want %q", body.Identity, "dashboard-b")
	}
	if len(body.Containers) != 1 || body.Containers[0].Name != "app-1" {
		t.Errorf("Containers = %+v, want the stub's one container", body.Containers)
	}
}

func TestPeerLogsContainersHandlerWrongSecret(t *testing.T) {
	h := peerLogsContainersHandler("s3cret", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerLogsContainersHandlerEmptySecretDisabled(t *testing.T) {
	h := peerLogsContainersHandler("", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerLogsContainersHandlerWrongMethod(t *testing.T) {
	h := peerLogsContainersHandler("s3cret", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodPost, "/peer/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestPeerLogsHandlerValidSecret(t *testing.T) {
	frames := framedLogBody("hello", "world")
	dc := logsContainersStub(t, frames)
	h := peerLogsHandler("s3cret", "dashboard-b", dc)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/app-1?tail=50", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body peerLogsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Identity != "dashboard-b" || body.Container != "app-1" || body.Tail != 50 {
		t.Errorf("body = %+v, want identity=dashboard-b container=app-1 tail=50", body)
	}
	if len(body.Lines) == 0 {
		t.Fatal("Lines = [], want the framed stub's non-empty lines")
	}
}

func TestPeerLogsHandlerWrongSecret(t *testing.T) {
	h := peerLogsHandler("s3cret", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/app-1", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerLogsHandlerEmptySecretDisabled(t *testing.T) {
	h := peerLogsHandler("", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/app-1", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerLogsHandlerWrongMethod(t *testing.T) {
	h := peerLogsHandler("s3cret", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodPost, "/peer/logs/app-1", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestPeerLogsHandlerInvalidContainerName proves a hostile name hits the
// peer handler's own validContainerName re-check directly — this handler is
// reachable by anyone possessing the shared peer secret and must be safe to
// call directly, not merely rely on the forwarding host's own check.
func TestPeerLogsHandlerInvalidContainerName(t *testing.T) {
	h := peerLogsHandler("s3cret", "dashboard-b", nil)
	req := httptest.NewRequest(http.MethodGet, "/peer/logs/evil/name", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestPeerGETTimeoutRespectsCustomDuration proves the timeout argument is
// actually honored: a handler slower than a short timeout errors, while the
// same handler with a longer timeout succeeds.
func TestPeerGETTimeoutRespectsCustomDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 2 * time.Second}

	var fast map[string]string
	start := time.Now()
	err := peerGETTimeout(context.Background(), client, srv.URL, "s3cret", "/anything", 100*time.Millisecond, &fast)
	if err == nil {
		t.Fatal("want an error for a handler slower than the timeout, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %s, want it bounded near the 100ms timeout, not the handler's 300ms sleep", elapsed)
	}

	var slow map[string]string
	if err := peerGETTimeout(context.Background(), client, srv.URL, "s3cret", "/anything", time.Second, &slow); err != nil {
		t.Fatalf("want success with a 1s timeout against a 300ms handler, got: %v", err)
	}
	if slow["ok"] != "yes" {
		t.Errorf("slow = %+v, want ok=yes", slow)
	}
}

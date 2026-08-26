package main

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dockerLogFrame builds one Docker-framed log line: an 8-byte header
// (stream-type byte, 3 zero bytes, 4-byte big-endian payload length)
// followed by the payload — the real wire format containerLogs demuxes (see
// parseDockerLogStream in logs.go). A plain-text stub would silently
// exercise the raw-fallback path instead of the real framed path.
func dockerLogFrame(stream byte, text string) []byte {
	payload := []byte(text + "\n")
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

func framedLogBody(lines ...string) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, dockerLogFrame(1, l)...)
	}
	return out
}

// logsContainersStub answers /containers/json with one running container
// ("app-1", service "app") and /containers/{id}/logs with frames (nil is
// fine when a test only exercises the containers list).
func logsContainersStub(t *testing.T, frames []byte) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/logs"):
			w.Write(frames)
		default:
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "c1", Names: []string{"/app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelService: "app"},
			}})
		}
	}))
}

func newLogsTestMux(t *testing.T, dc *dockerClient, reg *PeerRegistry) http.Handler {
	t.Helper()
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	return newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, reg, nil, nil, nil)
}

func TestLogsContainersEndpointHostParamForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit atomic.Bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerDC := logsContainersStub(t, nil)
	peerSrv := httptest.NewServer(peerLogsContainersHandler("s3cret", "dashboard-b", peerDC))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	req := httptest.NewRequest("GET", "/api/logs/containers?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var list []containerSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].Name != "app-1" {
		t.Fatalf("list = %+v, want the peer's one container", list)
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — GET /api/logs/containers?host=<peer> must not touch the local daemon")
	}
}

func TestLogsEndpointHostParamForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit atomic.Bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	frames := framedLogBody("hello from peer", "second line")
	peerDC := logsContainersStub(t, frames)
	peerSrv := httptest.NewServer(peerLogsHandler("s3cret", "dashboard-b", peerDC))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	req := httptest.NewRequest("GET", "/api/logs/app-1?host=dashboard-b&tail=50", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Container string    `json:"container"`
		Tail      int       `json:"tail"`
		Lines     []logLine `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Container != "app-1" || body.Tail != 50 {
		t.Errorf("body = %+v, want container=app-1 tail=50", body)
	}
	if len(body.Lines) != 2 || body.Lines[0].Text != "hello from peer" {
		t.Fatalf("Lines = %+v, want the peer's framed log lines", body.Lines)
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — GET /api/logs/{name}?host=<peer> must not touch the local daemon")
	}
}

func TestLogsEndpointHostParamRejectsInvalidContainerName(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHit.Store(true)
	}))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	// "evil/name" (a slash) and "..xyz" (leading dot, not an alnum start) are
	// both rejected by validContainerName itself. A literal ".." path
	// segment (e.g. "../etc/passwd") is deliberately NOT exercised here — Go's
	// http.ServeMux cleans and 307-redirects those before any handler ever
	// sees them, so it would never reach this validation in the first place.
	for _, name := range []string{"evil/name", "..xyz"} {
		req := httptest.NewRequest("GET", "/api/logs/"+name+"?host=dashboard-b", nil)
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("name %q: status = %d, body %s, want %d", name, rec.Code, rec.Body.String(), http.StatusNotFound)
		}
	}
	if peerHit.Load() {
		t.Error("peer was contacted for an invalid container name — must be rejected before forwarding")
	}
}

func TestLogsContainersEndpointHostParamUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	req := httptest.NewRequest("GET", "/api/logs/containers?host=nonexistent-host", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestLogsEndpointHostParamUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	req := httptest.NewRequest("GET", "/api/logs/app-1?host=nonexistent-host", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestLogsContainersEndpointHostParamNoRegistry(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLogsTestMux(t, dc, nil)

	req := httptest.NewRequest("GET", "/api/logs/containers?host=anything", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestLogsEndpointHostParamNoRegistry(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLogsTestMux(t, dc, nil)

	req := httptest.NewRequest("GET", "/api/logs/app-1?host=anything", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestLogsEndpointHostParamPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(url, true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	req := httptest.NewRequest("GET", "/api/logs/app-1?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("body = %+v, want a non-empty error field", body)
	}
}

// TestLogsEndpointHostParamSlowPeerRespectsExtendedTimeout proves the
// peerGETTimeout fix (5s) is what's actually wired in — a regression back to
// plain peerGET (2s) would make this test fail, since the peer sleeps longer
// than that old bound but shorter than the new one.
func TestLogsEndpointHostParamSlowPeerRespectsExtendedTimeout(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerLogsResp{
			Identity: "dashboard-b", Container: "app-1", Tail: 200,
			Lines: []logLine{{Stream: "stdout", Text: "slow but alive"}},
		})
	}))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	mux := newLogsTestMux(t, dc, reg)

	req := httptest.NewRequest("GET", "/api/logs/app-1?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s — a 3s peer response must succeed under the extended 5s peerGETTimeout bound", rec.Code, rec.Body.String())
	}

	var body struct {
		Lines []logLine `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Lines) != 1 || body.Lines[0].Text != "slow but alive" {
		t.Fatalf("Lines = %+v, want the slow peer's line", body.Lines)
	}
}

func TestLogsContainersEndpointNoHostParamLocal(t *testing.T) {
	dc := logsContainersStub(t, nil)
	mux := newLogsTestMux(t, dc, nil)

	req := httptest.NewRequest("GET", "/api/logs/containers", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var list []containerSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].Name != "app-1" {
		t.Fatalf("list = %+v, want the local stub's one container", list)
	}
}

func TestLogsEndpointNoHostParamLocal(t *testing.T) {
	frames := framedLogBody("local line one", "local line two")
	dc := logsContainersStub(t, frames)
	mux := newLogsTestMux(t, dc, nil)

	req := httptest.NewRequest("GET", "/api/logs/app-1?tail=10", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Container string    `json:"container"`
		Tail      int       `json:"tail"`
		Lines     []logLine `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Container != "app-1" || body.Tail != 10 {
		t.Errorf("body = %+v, want container=app-1 tail=10", body)
	}
	if len(body.Lines) != 2 || body.Lines[0].Text != "local line one" {
		t.Fatalf("Lines = %+v", body.Lines)
	}
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// TestRatchetOwnVersionSkipsDevAndNilRedis: no miniredis-backed *redis.Client
// test double exists in this codebase (authredis_test.go uses a fake
// txRunner interface, not a real *redis.Client), so this only confirms the
// nil-safe/no-op guard clauses don't panic or block.
func TestRatchetOwnVersionSkipsDevAndNilRedis(t *testing.T) {
	reg := newPeerRegistry(nil, "", "id", "dev", 0, nil)
	reg.ratchetOwnVersion(context.Background())
}

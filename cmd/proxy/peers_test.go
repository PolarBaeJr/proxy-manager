package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeerHandshakeHandlerValidSecret(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "proxy-a")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"peer":"proxy-a"`) || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %q, want peer=proxy-a ok=true", body)
	}
}

func TestPeerHandshakeHandlerWrongSecret(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "proxy-a")
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
	h := peerHandshakeHandler("s3cret", "proxy-a")
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
	h := peerHandshakeHandler("s3cret", "proxy-a")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestPeerHandshakeHandlerEmptySecretDisabled mirrors cmd/edge/peers.go's
// gossipHandler: an unconfigured secret hides the endpoint entirely (404)
// rather than accepting/rejecting bearer tokens.
func TestPeerHandshakeHandlerEmptySecretDisabled(t *testing.T) {
	h := peerHandshakeHandler("", "proxy-a")
	req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerHandshakeHandlerWrongMethod(t *testing.T) {
	h := peerHandshakeHandler("s3cret", "proxy-a")
	req := httptest.NewRequest(http.MethodGet, "/peer/handshake", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestPeerRegistryRecordsSuccess(t *testing.T) {
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "proxy-b"))
	defer srv.Close()

	reg := newPeerRegistry([]string{srv.URL}, "s3cret", "proxy-a", 0)
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
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "proxy-b"))
	defer srv.Close()

	reg := newPeerRegistry([]string{srv.URL}, "wrong-secret", "proxy-a", 0)
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
	srv := httptest.NewServer(peerHandshakeHandler("s3cret", "proxy-b"))
	url := srv.URL
	srv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "proxy-a", 0)
	reg.send(context.Background(), url)

	st := reg.Status()[url]
	if st.OK {
		t.Fatal("expected non-OK status for an unreachable peer")
	}
	if st.LastAttempt.IsZero() {
		t.Fatal("expected LastAttempt to be recorded even when unreachable")
	}
}

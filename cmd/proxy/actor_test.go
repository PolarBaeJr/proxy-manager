package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

var testActorSecret = []byte("proxy-actor-secret-for-tests-0123")

// The security boundary. A client can always SEND this header; what matters is
// that the proxy removes it before anything downstream can see it. Stripping —
// not the signature — is what stops a backend receiving an attacker-controlled
// value, and it has to happen on ungated routes too, where authorize() never
// runs to overwrite it.
func TestInboundActorHeaderIsAlwaysStripped(t *testing.T) {
	var seen []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(ActorHeader))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	r := &Router{}
	// An UNGATED route: authorize() never runs, so only the unconditional
	// strip in ServeHTTP can protect the backend.
	r.Set([]*RouteGroup{mkGroup(t, "app.example", "", false, backend.URL)})

	req := httptest.NewRequest("GET", "http://app.example/", nil)
	req.Header.Set(ActorHeader, sso.SignActor(
		sso.ActorClaims{Username: "root", Exp: 1 << 40}, testActorSecret))
	r.ServeHTTP(&accessWriter{ResponseWriter: httptest.NewRecorder()}, req)

	if len(seen) != 1 {
		t.Fatalf("backend hit %d times, want 1", len(seen))
	}
	if seen[0] != "" {
		t.Fatalf("backend saw a client-supplied actor header: %q", seen[0])
	}
}

// Even a validly-signed assertion from a client must be stripped: the proxy
// decides who the actor is, a client never gets to assert it.
func TestValidlySignedClientAssertionStillStripped(t *testing.T) {
	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(ActorHeader)
	}))
	t.Cleanup(backend.Close)

	r := &Router{}
	r.auth = &authGate{actorSecret: testActorSecret}
	r.Set([]*RouteGroup{mkGroup(t, "app.example", "", false, backend.URL)})

	req := httptest.NewRequest("GET", "http://app.example/", nil)
	req.Header.Set(ActorHeader, sso.SignActor(
		sso.ActorClaims{Username: "attacker", Exp: 1 << 40}, testActorSecret))
	r.ServeHTTP(&accessWriter{ResponseWriter: httptest.NewRecorder()}, req)

	if seen != "" {
		t.Fatalf("a client-signed assertion reached the backend: %q", seen)
	}
}

// stampActor stays silent when it has nothing honest to say — the LAN bypass
// authorizes without identifying anyone, and a placeholder there would be a
// fabricated audit record.
func TestStampActorNoOpCases(t *testing.T) {
	req := httptest.NewRequest("GET", "http://app.example/", nil)

	(&authGate{actorSecret: testActorSecret}).stampActor(req, "")
	if got := req.Header.Get(ActorHeader); got != "" {
		t.Errorf("stamped with no username: %q", got)
	}

	(&authGate{}).stampActor(req, "alice")
	if got := req.Header.Get(ActorHeader); got != "" {
		t.Errorf("stamped with no secret configured: %q", got)
	}

	var nilGate *authGate
	nilGate.stampActor(req, "alice") // must not panic
}

// A stamped assertion must verify against the same secret and name the user.
func TestStampActorRoundTrip(t *testing.T) {
	a := &authGate{actorSecret: testActorSecret}
	req := httptest.NewRequest("GET", "http://app.example/", nil)
	req.RemoteAddr = "100.64.0.9:5555"

	a.stampActor(req, "alice")
	raw := req.Header.Get(ActorHeader)
	if raw == "" {
		t.Fatal("nothing stamped")
	}
	claims, ok := sso.VerifyActor(raw, testActorSecret)
	if !ok {
		t.Fatal("stamped assertion failed to verify")
	}
	if claims.Username != "alice" {
		t.Errorf("username = %q, want alice", claims.Username)
	}
	if claims.IP != "100.64.0.9" {
		t.Errorf("ip = %q, want the real client address", claims.IP)
	}
	if claims.Exp == 0 {
		t.Error("no expiry set — assertions must not be replayable forever")
	}
	// A different secret must not verify it.
	if _, ok := sso.VerifyActor(raw, []byte("some-other-secret-entirely-xxxxxx")); ok {
		t.Error("assertion verified under the wrong secret")
	}
}

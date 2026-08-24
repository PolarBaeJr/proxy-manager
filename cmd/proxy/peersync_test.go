package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPeerSyncTickExcludesLearnedBackends proves the anti-recycling filter:
// a mixed group advertises only its local backend count, and an all-learned
// group is skipped entirely (never re-advertised back out).
func TestPeerSyncTickExcludesLearnedBackends(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		bodyCh <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	local := &Backend{URL: "http://10.0.0.1:8080"}
	learned := &Backend{URL: "http://10.0.0.2:8092", Learned: true, PeerID: "c"}
	mixedGroup := &RouteGroup{
		Host: "mixed.example.org", Backends: []*Backend{local, learned},
		RateLimit: true, RateRPM: 10,
	}

	learnedOnly := &Backend{URL: "http://10.0.0.3:8092", Learned: true, PeerID: "c"}
	learnedOnlyGroup := &RouteGroup{Host: "learned-only.example.org", Backends: []*Backend{learnedOnly}}

	r := &Router{}
	r.Set([]*RouteGroup{mixedGroup, learnedOnlyGroup})

	ps := newPeerSync(r, []string{srv.URL}, "s3cret", "a", "http://a:8092", 0)
	ps.tick(context.Background())

	var body []byte
	select {
	case body = <-bodyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tick to POST to the peer")
	}

	if gotAuth != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want Bearer s3cret", gotAuth)
	}
	var payload peerRoutePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Peer != "a" || payload.Advertise != "http://a:8092" {
		t.Fatalf("payload = %+v, want peer=a advertise=http://a:8092", payload)
	}
	if len(payload.Routes) != 1 {
		t.Fatalf("Routes = %+v, want exactly 1 (learned-only group must be skipped entirely)", payload.Routes)
	}
	if payload.Routes[0].Host != "mixed.example.org" || payload.Routes[0].Backends != 1 {
		t.Fatalf("Routes[0] = %+v, want mixed.example.org with Backends=1 (learned backend excluded from the count)", payload.Routes[0])
	}
	if !payload.Routes[0].RateLimit || payload.Routes[0].RateRPM != 10 {
		t.Fatalf("Routes[0] = %+v, want RateLimit=true RateRPM=10 carried across so the receiving peer can enforce it on a learned-only group", payload.Routes[0])
	}
}

// TestPeerSyncTickAllLearnedSendsNothing proves an empty resulting route
// list (every group is learned-only) sends nothing to peers at all.
func TestPeerSyncTickAllLearnedSendsNothing(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	learnedOnly := &Backend{URL: "http://10.0.0.3:8092", Learned: true, PeerID: "c"}
	r := &Router{}
	r.Set([]*RouteGroup{{Host: "learned-only.example.org", Backends: []*Backend{learnedOnly}}})

	ps := newPeerSync(r, []string{srv.URL}, "s3cret", "a", "http://a:8092", 0)
	ps.tick(context.Background())

	select {
	case <-called:
		t.Fatal("tick should send nothing when every group is learned-only")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestPeerSyncTickNoRoutesSendsNothing proves a router with no groups at
// all produces an empty payload and sends nothing.
func TestPeerSyncTickNoRoutesSendsNothing(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := &Router{}
	ps := newPeerSync(r, []string{srv.URL}, "s3cret", "a", "http://a:8092", 0)
	ps.tick(context.Background())

	select {
	case <-called:
		t.Fatal("tick should send nothing when there are no routes")
	case <-time.After(200 * time.Millisecond):
	}
}

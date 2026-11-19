package main

import (
	"net/http/httptest"
	"testing"
)

// The whole point of the /api/v1 alias: every /api/* route registered on the
// real mux must answer identically whether reached at its original path or
// the same path under /api/v1/ — see newDashboardMux's "versioned" wrapper.
// This is what lets a caller move to /api/v1/* without every existing caller
// (ui.go, mcptools.go, peers.go) having to move in lockstep.
func TestV1AliasMirrorsUnversionedRoutes(t *testing.T) {
	mux := newDashboardMux(nil, nil, nil, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	for _, path := range []string{"/api/health"} {
		orig := httptest.NewRecorder()
		mux.ServeHTTP(orig, httptest.NewRequest("GET", path, nil))

		aliasedPath := "/api/v1" + path[len("/api"):]
		aliased := httptest.NewRecorder()
		mux.ServeHTTP(aliased, httptest.NewRequest("GET", aliasedPath, nil))

		if aliased.Code != orig.Code {
			t.Errorf("%s status = %d, want %d (from %s)", aliasedPath, aliased.Code, orig.Code, path)
		}
		if aliased.Body.String() != orig.Body.String() {
			t.Errorf("%s body = %q, want %q (from %s)", aliasedPath, aliased.Body.String(), orig.Body.String(), path)
		}
	}
}

// list_peers is the first tool deliberately built against /api/v1/peers
// rather than /api/peers — this proves that path actually resolves through
// the real mux end to end, not just in the endpoint-routing table test.
func TestListPeersToolReachesV1Route(t *testing.T) {
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	t.Cleanup(func() { internalToken = prev })
	if err := mintInternalToken(); err != nil {
		t.Fatal(err)
	}

	// /api/v1/peers is auth-gated (peersStatusHandler), unlike the public
	// route above — needs a real, confirmed store or requireAuth's IsSetup
	// check panics on a nil AuthStore.
	mux := newDashboardMux(nil, nil, auth, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	srv := NewServer("t", "v")
	registerMCPTools(srv, &apiCaller{mux: mux}, false, false)

	res, _ := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	if res["error"] != nil {
		t.Fatalf("protocol error: %v", res["error"])
	}
	r := res["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if r["isError"] == true {
		t.Fatalf("list_peers failed against the real mux: %s", text)
	}
}

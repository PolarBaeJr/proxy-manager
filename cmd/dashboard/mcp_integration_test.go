package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// End-to-end through the REAL mux: an MCP tool call must authenticate with the
// process-local credential and reach an actual API handler. This is what
// replaces the old design's operator-managed API token, so it needs proving
// rather than assuming.
func TestMCPToolReachesRealHandler(t *testing.T) {
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	t.Cleanup(func() { internalToken = prev })
	if err := mintInternalToken(); err != nil {
		t.Fatal(err)
	}

	// A real mux. The maintenance route tolerates nil stores and reports
	// configured:false, which is enough to prove the request was authenticated
	// and handled rather than rejected.
	mux := newDashboardMux(nil, nil, auth, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil)

	srv := NewServer("t", "v")
	registerMCPTools(srv, &apiCaller{mux: mux}, false)

	res, _ := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"maintenance_status","arguments":{}}}`)
	if res["error"] != nil {
		t.Fatalf("protocol error: %v", res["error"])
	}
	r := res["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if r["isError"] == true {
		t.Fatalf("tool failed against the real mux: %s", text)
	}
	var payload struct {
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("handler returned non-JSON %q: %v", text, err)
	}
}

// Without the credential the same call must be refused — proving the handlers
// really are enforcing auth and the tool is not bypassing it.
func TestMCPToolRefusedWithoutInternalCredential(t *testing.T) {
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	t.Cleanup(func() { internalToken = prev })
	internalToken = ""

	mux := newDashboardMux(nil, nil, auth, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil)
	srv := NewServer("t", "v")
	registerMCPTools(srv, &apiCaller{mux: mux}, false)

	res, _ := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"maintenance_status","arguments":{}}}`)
	r := res["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("call succeeded with no credential: %v", r)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "internal credential unavailable") {
		t.Errorf("unexpected failure reason: %q", text)
	}
}

// The actor assertion must ride along, so the handler's audit entry names the
// person rather than the internal principal.
func TestMCPForwardsActorAssertion(t *testing.T) {
	prev := internalToken
	t.Cleanup(func() { internalToken = prev })
	internalToken = "pmt_internal_test"

	var seen string
	caller := &apiCaller{mux: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(actorHeader)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	})}
	ctx := withActor(context.Background(), "pmgact_assertion")
	if _, err := caller.call(ctx, "GET", "/api/maintenance", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if seen != "pmgact_assertion" {
		t.Fatalf("actor header = %q, want it forwarded", seen)
	}
}

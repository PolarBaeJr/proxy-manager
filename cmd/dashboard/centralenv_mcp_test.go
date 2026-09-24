package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCentralEnvMCPToolGating: get_service_env is read-only; the two tools
// that restart a service on every host need writes AND peer writes.
func TestCentralEnvMCPToolGating(t *testing.T) {
	c, _ := stubDash(t, 200, `{}`)
	for _, tc := range []struct {
		writes, peerWrites, wantMutating bool
	}{{false, false, false}, {true, false, false}, {true, true, true}} {
		s := NewServer("t", "v")
		registerMCPTools(s, c, tc.writes, tc.peerWrites)
		names := toolNames(t, s)
		if !names["get_service_env"] {
			t.Errorf("writes=%v peer=%v: get_service_env missing", tc.writes, tc.peerWrites)
		}
		for _, w := range []string{"set_service_env", "sync_service_env"} {
			if names[w] != tc.wantMutating {
				t.Errorf("writes=%v peer=%v: %s registered=%v", tc.writes, tc.peerWrites, w, names[w])
			}
		}
	}
}

func TestCentralEnvMCPCallsAndRefusesLiteralCredentials(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })
	var calls, bodies []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		bodies = append(bodies, string(b))
		w.Write([]byte(`{"version":2,"changed_keys":["API_TOKEN"]}`))
	})
	s := NewServer("t", "v")
	registerMCPTools(s, &apiCaller{mux: h}, true, true)
	call := func(name, args string) map[string]any {
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		r, _ := res["result"].(map[string]any)
		return r
	}

	for _, args := range []string{
		`{"service":"app","if_version":1,"set":{"API_TOKEN":"literal-secret-value"}}`,
		`{"service":"app","if_version":1,"host_overrides":{"dashboard-b":{"DB_PASSWORD":"hunter2hunter2"}}}`,
	} {
		r := call("set_service_env", args)
		b, _ := json.Marshal(r)
		if r["isError"] != true || !strings.Contains(string(b), "ref:NAME") || strings.Contains(string(b), "literal-secret-value") || strings.Contains(string(b), "hunter2hunter2") {
			t.Fatalf("literal credential = %s", b)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused literal still reached the dashboard: %v", calls)
	}

	r := call("set_service_env", `{"service":"app","if_version":1,"request_id":"r1","set":{"API_TOKEN":"ref:API_TOKEN","PLAIN":"v"},"unset":["OLD"],"host_overrides":{"dashboard-b":{"B":"x"}},"unset_host_overrides":{"dashboard-b":["C"]}}`)
	if r["isError"] == true || len(calls) != 1 || calls[0] != "POST /api/services/app/env" {
		t.Fatalf("set = %v, calls %v", r, calls)
	}
	var sent centralEnvSetRequest
	json.Unmarshal([]byte(bodies[0]), &sent)
	if sent.IfVersion != 1 || sent.RequestID != "r1" || sent.Set["API_TOKEN"] != "ref:API_TOKEN" || sent.Unset[0] != "OLD" ||
		sent.HostOverrides["dashboard-b"]["B"] != "x" || sent.UnsetHostOverrides["dashboard-b"][0] != "C" {
		t.Fatalf("sent = %s", bodies[0])
	}
	if r := call("set_service_env", `{"service":"app","if_version":0,"set":{"A":"1"}}`); r["isError"] != true {
		t.Fatal("if_version 0 accepted")
	}

	call("get_service_env", `{"service":"app"}`)
	call("sync_service_env", `{"service":"app"}`)
	if len(calls) != 3 || calls[1] != "GET /api/services/app/env" || calls[2] != "POST /api/services/app/env/sync" {
		t.Fatalf("calls = %v", calls)
	}
}

// TestCentralEnvMCPGetIsNamesOnly drives get_service_env against the real
// dashboard API over a real central env holding values.
func TestCentralEnvMCPGetIsNamesOnly(t *testing.T) {
	withFastSync(t)
	h, mux := newAPIHost(t, map[string]string{"PLAIN": "value-one-xyz", "TOKEN": "ref:TOKEN"})
	h.ce.secrets = newTestSecrets(t, "app", "TOKEN=secret-two-xyz")
	s := NewServer("t", "v")
	registerMCPTools(s, &apiCaller{mux: mux}, false, false)
	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_service_env","arguments":{"service":"app"}}}`)
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), "PLAIN") || !strings.Contains(string(b), "TOKEN") {
		t.Fatalf("get_service_env = %s", b)
	}
	if strings.Contains(string(b), "value-one-xyz") || strings.Contains(string(b), "secret-two-xyz") {
		t.Fatalf("get_service_env leaked a value: %s", b)
	}
}

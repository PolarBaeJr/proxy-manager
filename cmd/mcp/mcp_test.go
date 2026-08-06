package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rpc(t *testing.T, s *Server, body string) (map[string]any, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	if rec.Body.Len() == 0 {
		return nil, rec.Code
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out, rec.Code
}

// stubDash stands in for the dashboard API and records what was called, so a
// tool that silently hits the wrong endpoint is caught.
func stubDash(t *testing.T, status int, body string) (*dashClient, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want the API token", got)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return newDashClient(srv.URL, "test-token"), &calls
}

func toolNames(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	out := map[string]bool{}
	result, ok := res["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", res)
	}
	for _, raw := range result["tools"].([]any) {
		out[raw.(map[string]any)["name"].(string)] = true
	}
	return out
}

// The whole point of the gate: with writes off, no mutating tool is even
// registered, so it cannot be called by guessing the name.
func TestWriteToolsAbsentUnlessAllowed(t *testing.T) {
	c, _ := stubDash(t, 200, `{}`)

	ro := NewServer("t", "v")
	registerTools(ro, c, false)
	names := toolNames(t, ro)
	for _, w := range []string{"set_maintenance", "scale_service", "lifecycle_service", "set_autoupdate", "stage_canary", "resolve_canary"} {
		if names[w] {
			t.Errorf("mutating tool %q registered in read-only mode", w)
		}
	}
	for _, r := range []string{"list_services", "list_routes", "get_logs", "maintenance_status", "list_dns"} {
		if !names[r] {
			t.Errorf("read tool %q missing", r)
		}
	}

	rw := NewServer("t", "v")
	registerTools(rw, c, true)
	rwNames := toolNames(t, rw)
	for _, w := range []string{"set_maintenance", "scale_service", "lifecycle_service", "set_autoupdate", "stage_canary", "resolve_canary"} {
		if !rwNames[w] {
			t.Errorf("mutating tool %q missing when writes are allowed", w)
		}
	}
	// Every tool marked Mutating must actually be gated.
	for name, tool := range ro.tools {
		if tool.Mutating {
			t.Errorf("tool %q is marked Mutating but registered read-only", name)
		}
	}
}

// Calling a gated tool by name in read-only mode must fail, not fall through.
func TestCallingGatedToolFails(t *testing.T) {
	c, calls := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerTools(s, c, false)

	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_maintenance","arguments":{"host":"x.example","enabled":true}}}`)
	e, ok := res["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", res)
	}
	if int(e["code"].(float64)) != errMethodNotFound {
		t.Errorf("code = %v, want method-not-found", e["code"])
	}
	if len(*calls) != 0 {
		t.Errorf("the dashboard was contacted despite the gate: %v", *calls)
	}
}

func TestInitializeAndPing(t *testing.T) {
	s := NewServer("proxy-manager-dashboard", "1.2.3")
	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	r := res["result"].(map[string]any)
	if r["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", r["protocolVersion"])
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability not advertised")
	}
	if info := r["serverInfo"].(map[string]any); info["name"] != "proxy-manager-dashboard" || info["version"] != "1.2.3" {
		t.Errorf("serverInfo = %v", info)
	}

	if res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); res["error"] != nil {
		t.Errorf("ping errored: %v", res["error"])
	}
}

// A notification carries no id and MUST NOT be answered.
func TestNotificationGetsNoResponse(t *testing.T) {
	s := NewServer("t", "v")
	res, code := rpc(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", code)
	}
	if res != nil {
		t.Errorf("a notification was answered: %v", res)
	}
}

func TestProtocolErrors(t *testing.T) {
	s := NewServer("t", "v")
	cases := []struct {
		name, body string
		want       int
	}{
		{"bad json", `{nope`, errParse},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, errInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"nope/nope"}`, errMethodNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := rpc(t, s, tc.body)
			e, ok := res["error"].(map[string]any)
			if !ok {
				t.Fatalf("expected error, got %v", res)
			}
			if int(e["code"].(float64)) != tc.want {
				t.Errorf("code = %v, want %d", e["code"], tc.want)
			}
		})
	}
}

// GET has no SSE channel to open; refusing it must be explicit.
func TestGetIsRejected(t *testing.T) {
	s := NewServer("t", "v")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "POST" {
		t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}
}

// A failing tool is a RESULT with isError, not a protocol error — the model has
// to see the message to adapt.
func TestToolFailureIsResultNotProtocolError(t *testing.T) {
	c, _ := stubDash(t, 404, `service not found`)
	s := NewServer("t", "v")
	registerTools(s, c, true)

	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_services","arguments":{}}}`)
	if res["error"] != nil {
		t.Fatalf("got a protocol error, want an isError result: %v", res["error"])
	}
	r := res["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("isError not set: %v", r)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "service not found") {
		t.Errorf("dashboard's message was lost: %q", text)
	}
}

// Arguments reach the right endpoint with the right method.
func TestToolsCallCorrectEndpoints(t *testing.T) {
	cases := []struct {
		tool, args, wantCall string
	}{
		{"list_services", `{}`, "GET /api/services"},
		{"maintenance_status", `{}`, "GET /api/maintenance"},
		{"get_logs", `{"container":"my app","tail":50}`, "GET /api/logs/my%20app?tail=50"},
		{"set_maintenance", `{"host":"x.example","enabled":true}`, "POST /api/maintenance/x.example"},
		{"set_maintenance", `{"host":"x.example","enabled":false}`, "DELETE /api/maintenance/x.example"},
		{"scale_service", `{"service":"app","replicas":3}`, "POST /api/services/app/scale"},
		{"lifecycle_service", `{"service":"app","action":"stop"}`, "POST /api/services/app/stop"},
		{"resolve_canary", `{"service":"app","action":"discard"}`, "DELETE /api/services/app/canary"},
		{"resolve_canary", `{"service":"app","action":"promote"}`, "POST /api/services/app/promote"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			c, calls := stubDash(t, 200, `{"ok":true}`)
			s := NewServer("t", "v")
			registerTools(s, c, true)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}`
			res, _ := rpc(t, s, body)
			if res["error"] != nil {
				t.Fatalf("protocol error: %v", res["error"])
			}
			if r := res["result"].(map[string]any); r["isError"] == true {
				t.Fatalf("tool errored: %v", r["content"])
			}
			if len(*calls) != 1 || (*calls)[0] != tc.wantCall {
				t.Fatalf("calls = %v, want [%s]", *calls, tc.wantCall)
			}
		})
	}
}

// Bad arguments must be refused before the dashboard is touched.
func TestArgumentValidation(t *testing.T) {
	cases := []struct{ tool, args string }{
		{"scale_service", `{"service":"app"}`},                      // missing replicas
		{"scale_service", `{"service":"app","replicas":2.5}`},       // fractional
		{"scale_service", `{"service":"app","replicas":-1}`},        // negative
		{"scale_service", `{"service":"","replicas":1}`},            // empty name
		{"lifecycle_service", `{"service":"app","action":"nuke"}`},  // not start/stop
		{"resolve_canary", `{"service":"app","action":"maybe"}`},    // not promote/discard
		{"set_maintenance", `{"host":"x.example","enabled":"yes"}`}, // string not bool
		{"get_logs", `{}`}, // missing container
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			c, calls := stubDash(t, 200, `{}`)
			s := NewServer("t", "v")
			registerTools(s, c, true)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}`
			res, _ := rpc(t, s, body)
			r, ok := res["result"].(map[string]any)
			if !ok || r["isError"] != true {
				t.Fatalf("bad arguments accepted: %v", res)
			}
			if len(*calls) != 0 {
				t.Errorf("dashboard contacted with invalid arguments: %v", *calls)
			}
		})
	}
}

// tools/list must be stable — an unstable order shows up as churn in client
// tool caches.
func TestToolListIsSorted(t *testing.T) {
	c, _ := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerTools(s, c, true)
	var prev string
	for _, tool := range s.toolList() {
		if tool.Name < prev {
			t.Fatalf("tools/list not sorted: %q after %q", tool.Name, prev)
		}
		prev = tool.Name
	}
}

// Every tool needs a schema the model can actually use.
func TestToolSchemasWellFormed(t *testing.T) {
	c, _ := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerTools(s, c, true)
	for _, tool := range s.toolList() {
		if tool.Description == "" {
			t.Errorf("%s: no description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s: schema type = %v, want object", tool.Name, tool.InputSchema["type"])
		}
		props, ok := tool.InputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no properties object", tool.Name)
		}
		req, ok := tool.InputSchema["required"].([]string)
		if !ok {
			t.Fatalf("%s: required is not a string slice", tool.Name)
		}
		for _, r := range req {
			if _, defined := props[r]; !defined {
				t.Errorf("%s: %q is required but not defined in properties", tool.Name, r)
			}
		}
	}
}

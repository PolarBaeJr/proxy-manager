package main

import (
	"encoding/json"
	"io"
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

// stubDash stands in for the dashboard's API mux and records what was called,
// so a tool that silently hits the wrong endpoint is caught. It also asserts
// the in-process credential is presented — without it the real handlers would
// 401 and every tool would fail.
func stubDash(t *testing.T, status int, body string) (*apiCaller, *[]string) {
	t.Helper()
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	var calls []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer "+internalToken {
			t.Errorf("Authorization = %q, want the internal credential", got)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
	return &apiCaller{mux: h}, &calls
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
	registerMCPTools(ro, c, false, false)
	names := toolNames(t, ro)
	for _, w := range []string{"set_maintenance", "scale_service", "lifecycle_service", "set_autoupdate", "stage_canary", "replace_service", "resolve_canary", "onboard_service", "offboard_service", "restart_replica", "create_dns_record", "update_dns_record", "delete_dns_record"} {
		if names[w] {
			t.Errorf("mutating tool %q registered in read-only mode", w)
		}
	}
	for _, r := range []string{"list_services", "list_routes", "get_logs", "maintenance_status", "list_dns", "check_for_update"} {
		if !names[r] {
			t.Errorf("read tool %q missing", r)
		}
	}

	rw := NewServer("t", "v")
	registerMCPTools(rw, c, true, false)
	rwNames := toolNames(t, rw)
	for _, w := range []string{"set_maintenance", "scale_service", "lifecycle_service", "set_autoupdate", "stage_canary", "replace_service", "resolve_canary", "onboard_service", "offboard_service", "restart_replica", "create_dns_record", "update_dns_record", "delete_dns_record"} {
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
	registerMCPTools(s, c, false, false)

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
	registerMCPTools(s, c, true, false)

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

// list_services strips each service's raw Labels map before it reaches the
// model — Labels carries every proxy.* and docker-compose-generated label
// verbatim and is by far the biggest thing in the response once there's more
// than a couple of services, and no MCP tool reads it back. Unrelated fields
// must survive untouched.
func TestListServicesStripsLabels(t *testing.T) {
	body := `[{"name":"app","group":"app","image":"ghcr.io/org/app:v1","host":"app.example","port":8080,"replicas":1,"autoupdate_skip_reason":"stopped retrying after 3 consecutive failures: boom","labels":{"com.docker.compose.project":"stack","proxy.enable":"true","proxy.host":"app.example"}}]`
	c, _ := stubDash(t, 200, body)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_services","arguments":{}}}`)
	r := res["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "labels") || strings.Contains(text, "com.docker.compose") {
		t.Errorf("list_services still leaks labels: %s", text)
	}
	if !strings.Contains(text, `"app.example"`) || !strings.Contains(text, `"replicas"`) {
		t.Errorf("list_services dropped an unrelated field: %s", text)
	}
	// The sticky blocked-reason fallback (autoupdate.go's autoUpdateBlockStore)
	// is exactly the kind of "why is this stuck" signal an MCP client needs —
	// stripServiceLabels must only drop Labels, never this.
	if !strings.Contains(text, "stopped retrying after 3 consecutive failures") {
		t.Errorf("list_services dropped autoupdate_skip_reason: %s", text)
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
		{"stage_canary", `{"service":"app","image":"ghcr.io/org/app:v2"}`, "POST /api/services/app/stage"},
		{"replace_service", `{"service":"app","image":"ghcr.io/org/app:v2"}`, "POST /api/services/app/replace"},
		{"restart_replica", `{"service":"app","member":"goproxy-app-1","action":"start"}`, "POST /api/services/app/replicas/goproxy-app-1/start"},
		{"restart_replica", `{"service":"app","member":"goproxy-app-1","action":"stop"}`, "POST /api/services/app/replicas/goproxy-app-1/stop"},
		{"check_for_update", `{"service":"app"}`, "POST /api/services/app/check"},
		{"create_dns_record", `{"type":"A","name":"x.example","content":"1.2.3.4"}`, "POST /api/cf/records?zone="},
		{"update_dns_record", `{"id":"rec1","content":"1.2.3.4"}`, "PATCH /api/cf/records/rec1?zone="},
		{"delete_dns_record", `{"id":"rec1"}`, "DELETE /api/cf/records/rec1?zone="},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			c, calls := stubDash(t, 200, `{"ok":true}`)
			s := NewServer("t", "v")
			registerMCPTools(s, c, true, false)
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
		{"stage_canary", `{"service":"app","image":"i:v2","env":"nope"}`},              // env not an object
		{"stage_canary", `{"service":"app","image":"i:v2","env":{"K":1}}`},             // non-string env value
		{"stage_canary", `{"service":"app","image":"i:v2","env":{"":"v"}}`},            // empty env key
		{"stage_canary", `{"service":"app","image":"i:v2","env":{"K=X":"v"}}`},         // env key contains "="
		{"stage_canary", `{"service":"app","image":"i:v2","env":{" K ":"v","K":"w"}}`}, // duplicate key after trim
		{"stage_canary", `{"service":"app","image":"i:v2","env_ack":"nope"}`},          // env_ack not an array
		{"stage_canary", `{"service":"app","image":"i:v2","env_ack":[1]}`},             // non-string env_ack item
		{"replace_service", `{"service":"app","image":"i:v2","env":"nope"}`},           // env not an object
		{"replace_service", `{"service":"app","image":"i:v2","env":{"K":1}}`},          // non-string env value
		{"replace_service", `{"service":"app","image":"i:v2","env":{"":"v"}}`},         // empty env key
		{"replace_service", `{"service":"app","image":"i:v2","env":{"K=X":"v"}}`},      // env key contains "="
		{"replace_service", `{"service":"app","image":"i:v2","env_ack":"nope"}`},       // env_ack not an array
		{"replace_service", `{"service":"app","image":"i:v2","env_ack":[1]}`},          // non-string env_ack item
		{"restart_replica", `{"service":"app","member":"m1","action":"nuke"}`},         // not start/stop/restart
		{"create_dns_record", `{"name":"x.example","content":"1.2.3.4"}`},              // missing type
		{"update_dns_record", `{"id":"rec1"}`},                                         // no fields to update
		{"delete_dns_record", `{}`},                                                    // missing id
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			c, calls := stubDash(t, 200, `{}`)
			s := NewServer("t", "v")
			registerMCPTools(s, c, true, false)
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
	registerMCPTools(s, c, true, false)
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
	registerMCPTools(s, c, true, false)
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

// stage_canary and replace_service forward env edits verbatim, and omit the
// keys entirely from the wire body when the caller doesn't provide any.
func TestStageAndReplaceForwardEnvEdits(t *testing.T) {
	cases := []struct{ tool string }{{"stage_canary"}, {"replace_service"}}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			prev := internalToken
			internalToken = "pmt_internal_test"
			t.Cleanup(func() { internalToken = prev })

			var got ReplaceServiceRequest
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &got)
				w.WriteHeader(200)
				w.Write([]byte(`{}`))
			})
			c := &apiCaller{mux: h}
			s := NewServer("t", "v")
			registerMCPTools(s, c, true, false)

			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":{"service":"app","image":"i:v2","env":{"K":"v"},"env_ack":["K"]}}}`
			res, _ := rpc(t, s, body)
			if r := res["result"].(map[string]any); r["isError"] == true {
				t.Fatalf("tool errored: %v", r["content"])
			}
			if got.Env["K"] != "v" {
				t.Errorf("env not forwarded: %+v", got)
			}
			if len(got.EnvAck) != 1 || got.EnvAck[0] != "K" {
				t.Errorf("env_ack not forwarded: %+v", got)
			}

			var raw map[string]any
			h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &raw)
				w.WriteHeader(200)
				w.Write([]byte(`{}`))
			})
			c2 := &apiCaller{mux: h2}
			s2 := NewServer("t", "v")
			registerMCPTools(s2, c2, true, false)
			body2 := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":{"service":"app","image":"i:v2"}}}`
			res2, _ := rpc(t, s2, body2)
			if r := res2["result"].(map[string]any); r["isError"] == true {
				t.Fatalf("tool errored: %v", r["content"])
			}
			if _, ok := raw["env"]; ok {
				t.Errorf("env key present despite not being set: %v", raw)
			}
			if _, ok := raw["env_ack"]; ok {
				t.Errorf("env_ack key present despite not being set: %v", raw)
			}
		})
	}
}

// A 409 conflict from the dashboard (e.g. an env key whose value differs from
// what's running) must surface as an MCP tool error naming the conflicting key.
func TestStageCanaryEnvConflictSurfaces(t *testing.T) {
	c, _ := stubDash(t, 409, `{"error":"env conflict","keys":["PORT"]}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stage_canary","arguments":{"service":"app","image":"i:v2","env":{"PORT":"3000"}}}}`
	res, _ := rpc(t, s, body)
	r, ok := res["result"].(map[string]any)
	if !ok || r["isError"] != true {
		t.Fatalf("expected isError, got %v", res)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "PORT") {
		t.Errorf("conflicting key not surfaced: %q", text)
	}
}

// replace_service surfaces the same conflict shape as stage_canary.
func TestReplaceServiceEnvConflictSurfaces(t *testing.T) {
	c, _ := stubDash(t, 409, `{"error":"env conflict","keys":["PORT"]}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"replace_service","arguments":{"service":"app","image":"i:v2","env":{"PORT":"3000"}}}}`
	res, _ := rpc(t, s, body)
	r, ok := res["result"].(map[string]any)
	if !ok || r["isError"] != true {
		t.Fatalf("expected isError, got %v", res)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "PORT") {
		t.Errorf("conflicting key not surfaced: %q", text)
	}
}

// restart is stop immediately followed by start, in that order, and the tool
// returns start's result — the caller cares about the end state.
func TestRestartReplicaStopsThenStarts(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	var calls []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(200)
		if strings.HasSuffix(r.URL.Path, "/start") {
			w.Write([]byte(`{"status":"started"}`))
		} else {
			w.Write([]byte(`{"status":"stopped"}`))
		}
	})
	c := &apiCaller{mux: h}
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"restart_replica","arguments":{"service":"app","member":"m1","action":"restart"}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	want := []string{"POST /api/services/app/replicas/m1/stop", "POST /api/services/app/replicas/m1/start"}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "started") {
		t.Errorf("expected start's result, got %q", text)
	}
}

// If stop fails, restart must not attempt start.
func TestRestartReplicaStopFailureSkipsStart(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	var calls []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(500)
		w.Write([]byte(`stop failed`))
	})
	c := &apiCaller{mux: h}
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"restart_replica","arguments":{"service":"app","member":"m1","action":"restart"}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("expected error, got %v", res)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want just the stop attempt", calls)
	}
}

// update_dns_record must send only the fields the caller actually provided —
// an explicit false has to reach the wire as false, not get dropped like an
// absent field would.
func TestUpdateDNSRecordOnlySendsProvidedFields(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	var got UpdateDNSRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	})
	c := &apiCaller{mux: h}
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update_dns_record","arguments":{"id":"rec1","proxied":false}}}`
	res, _ := rpc(t, s, body)
	if r := res["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	if got.Proxied == nil || *got.Proxied != false {
		t.Errorf("Proxied = %v, want a pointer to false", got.Proxied)
	}
	if got.Content != nil {
		t.Errorf("Content = %v, want nil (not provided)", *got.Content)
	}
	if got.Name != nil {
		t.Errorf("Name = %v, want nil (not provided)", *got.Name)
	}
}

// An env-var key with surrounding whitespace must be trimmed before it
// reaches the dashboard — otherwise it silently fails to match the real
// (trimmed) key mergeEnv indexes by and gets appended as a bogus new var
// instead of updating the intended one.
func TestArgEnvEditsTrimsKeys(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	var got ReplaceServiceRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	})
	c := &apiCaller{mux: h}
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stage_canary","arguments":{"service":"app","image":"i:v2","env":{" PORT ":"3000"}}}}`
	res, _ := rpc(t, s, body)
	if r := res["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	if v, ok := got.Env["PORT"]; !ok || v != "3000" {
		t.Errorf("env = %+v, want a clean %q key", got.Env, "PORT")
	}
	if _, ok := got.Env[" PORT "]; ok {
		t.Errorf("env = %+v, untrimmed key leaked through", got.Env)
	}
}

// If a failed start follows a successful stop, the error must say the
// replica is now STOPPED — not read like a generic call failure that leaves
// the caller thinking it's still running.
func TestRestartReplicaStartFailureSaysStopped(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			w.WriteHeader(500)
			w.Write([]byte(`start failed`))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"stopped"}`))
	})
	c := &apiCaller{mux: h}
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"restart_replica","arguments":{"service":"app","member":"m1","action":"restart"}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("expected error, got %v", res)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "STOPPED") {
		t.Errorf("error doesn't say the replica is STOPPED: %q", text)
	}
}

// zone:"" (present but empty) must be accepted as "use default", the same
// as an omitted zone — some MCP clients always send every schema key.
func TestDNSToolsAcceptEmptyZone(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	cases := []struct{ tool, args string }{
		{"create_dns_record", `{"zone":"","type":"A","name":"x","content":"1.2.3.4"}`},
		{"update_dns_record", `{"zone":"","id":"rec1","content":"1.2.3.4"}`},
		{"delete_dns_record", `{"zone":"","id":"rec1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			c, calls := stubDash(t, 200, `{}`)
			s := NewServer("t", "v")
			registerMCPTools(s, c, true, false)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}`
			res, _ := rpc(t, s, body)
			r, ok := res["result"].(map[string]any)
			if !ok || r["isError"] == true {
				t.Fatalf("zone:\"\" rejected: %v", res)
			}
			if len(*calls) != 1 {
				t.Fatalf("calls = %v, want exactly one", *calls)
			}
		})
	}
}

// offboard_service now hits the single /offboard endpoint directly — the
// backend itself picks the legacy onboarded-teardown path or the label-
// managed network-disconnect path, so the tool no longer needs its own
// list_services lookup.
func TestOffboardServiceCallsOffboardEndpoint(t *testing.T) {
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	var calls []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"offboarded"}`))
	})
	c := &apiCaller{mux: h}
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"offboard_service","arguments":{"service":"app"}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	want := []string{"POST /api/services/app/offboard"}
	if len(calls) != 1 || calls[0] != want[0] {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// A failure from the backend (e.g. no label-managed containers found for the
// service) must surface as an error result, not be swallowed.
func TestOffboardServiceSurfacesBackendError(t *testing.T) {
	c, calls := stubDash(t, 400, `service "app" not found`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"offboard_service","arguments":{"service":"app"}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("expected an error result, got %v", res)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST /api/services/app/offboard" {
		t.Fatalf("calls = %v", *calls)
	}
}

// onboard_service must forward host/port/path/strip/replicas to the
// discovery onboard endpoint, defaulting strip/replicas when omitted.
func TestOnboardServiceCallsOnboardEndpoint(t *testing.T) {
	c, calls := stubDash(t, 200, `{"status":"onboarded","name":"app"}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"onboard_service","arguments":{"service":"app","host":"app.example.com","port":8080}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	if len(*calls) != 1 || (*calls)[0] != "POST /api/discovery/app/onboard" {
		t.Fatalf("calls = %v, want a single POST to the onboard endpoint", *calls)
	}
}

// onboard_service must require host and port.
func TestOnboardServiceRequiresHostAndPort(t *testing.T) {
	c, calls := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"onboard_service","arguments":{"service":"app"}}}`
	res, _ := rpc(t, s, body)
	r := res["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("expected refusal without host/port, got %v", res)
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %v, want the dashboard never contacted", *calls)
	}
}

// The 10 tools that support peer targeting, and the argument key each one
// reads it under. onboard_service alone uses "peer_host" — its own "host"
// key already means the hostname to ROUTE.
var peerTargetableTools = map[string]string{
	"check_for_update":  "host",
	"scale_service":     "host",
	"lifecycle_service": "host",
	"set_autoupdate":    "host",
	"stage_canary":      "host",
	"replace_service":   "host",
	"resolve_canary":    "host",
	"onboard_service":   "peer_host",
	"offboard_service":  "host",
	"restart_replica":   "host",
}

// A host/peer_host argument must be refused before the dashboard is
// contacted when MCP_ALLOW_PEER_WRITES is off, even with writes on, and the
// error must point at the env var so an operator can fix it.
func TestHostParamRejectedWithoutPeerWrites(t *testing.T) {
	c, calls := stubDash(t, 200, `{"ok":true}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"scale_service","arguments":{"service":"app","replicas":2,"host":"peer-b"}}}`
	res, _ := rpc(t, s, body)
	r, ok := res["result"].(map[string]any)
	if !ok || r["isError"] != true {
		t.Fatalf("expected isError, got %v", res)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "MCP_ALLOW_PEER_WRITES") {
		t.Errorf("error doesn't mention MCP_ALLOW_PEER_WRITES: %q", text)
	}
	if len(*calls) != 0 {
		t.Errorf("the dashboard was contacted despite the rejection: %v", *calls)
	}
}

// With MCP_ALLOW_PEER_WRITES on, the host/peer_host argument must reach the
// dashboard as a ?host= query param on the forwarded request.
func TestHostParamAppendedWhenPeerWritesAllowed(t *testing.T) {
	cases := []struct{ tool, args, wantSuffix string }{
		{"scale_service", `{"service":"app","replicas":2,"host":"peer-b"}`, "?host=peer-b"},
		{"check_for_update", `{"service":"app","host":"peer-b"}`, "?host=peer-b"},
		{"lifecycle_service", `{"service":"app","action":"stop","host":"peer-b"}`, "?host=peer-b"},
		{"set_autoupdate", `{"service":"app","enabled":true,"host":"peer-b"}`, "?host=peer-b"},
		{"stage_canary", `{"service":"app","image":"i:v2","host":"peer-b"}`, "?host=peer-b"},
		{"replace_service", `{"service":"app","image":"i:v2","host":"peer-b"}`, "?host=peer-b"},
		{"resolve_canary", `{"service":"app","action":"promote","host":"peer-b"}`, "?host=peer-b"},
		{"resolve_canary", `{"service":"app","action":"discard","host":"peer-b"}`, "?host=peer-b"},
		{"onboard_service", `{"service":"app","host":"app.example.com","port":8080,"peer_host":"peer-b"}`, "?host=peer-b"},
		{"offboard_service", `{"service":"app","host":"peer-b"}`, "?host=peer-b"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			c, calls := stubDash(t, 200, `{"ok":true}`)
			s := NewServer("t", "v")
			registerMCPTools(s, c, true, true)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.args + `}}`
			res, _ := rpc(t, s, body)
			if r := res["result"].(map[string]any); r["isError"] == true {
				t.Fatalf("tool errored: %v", r["content"])
			}
			if len(*calls) == 0 {
				t.Fatalf("dashboard never contacted")
			}
			for _, call := range *calls {
				if !strings.HasSuffix(call, tc.wantSuffix) {
					t.Errorf("call %q does not end with %q", call, tc.wantSuffix)
				}
			}
		})
	}

	// restart_replica with action=restart calls twice (stop then start),
	// both of which must carry the host param.
	t.Run("restart_replica restart", func(t *testing.T) {
		c, calls := stubDash(t, 200, `{"ok":true}`)
		s := NewServer("t", "v")
		registerMCPTools(s, c, true, true)
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"restart_replica","arguments":{"service":"app","member":"m1","action":"restart","host":"peer-b"}}}`
		res, _ := rpc(t, s, body)
		if r := res["result"].(map[string]any); r["isError"] == true {
			t.Fatalf("tool errored: %v", r["content"])
		}
		want := []string{
			"POST /api/services/app/replicas/m1/stop?host=peer-b",
			"POST /api/services/app/replicas/m1/start?host=peer-b",
		}
		if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
			t.Fatalf("calls = %v, want %v", *calls, want)
		}
	})
}

// Without a host argument, behavior is completely unaffected by the new
// gate — even with MCP_ALLOW_PEER_WRITES off, the call must succeed and the
// request must carry no ?host= at all.
func TestHostParamOmittedIsUnaffected(t *testing.T) {
	c, calls := stubDash(t, 200, `{"ok":true}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"scale_service","arguments":{"service":"app","replicas":2}}}`
	res, _ := rpc(t, s, body)
	if r := res["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	if len(*calls) != 1 || strings.Contains((*calls)[0], "host=") {
		t.Fatalf("calls = %v, want exactly one call with no host param", *calls)
	}
}

// The new gate must not leak a host param into an unrelated tool's request —
// list_dns never reads args["host"], so passing one must be silently ignored
// rather than surfacing in the forwarded URI.
func TestHostParamNotOnUnrelatedTools(t *testing.T) {
	c, calls := stubDash(t, 200, `{"ok":true}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, true)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_dns","arguments":{"host":"peer-b"}}}`
	res, _ := rpc(t, s, body)
	if r := res["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("tool errored: %v", r["content"])
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", *calls)
	}
	if strings.Contains((*calls)[0], "host=") {
		t.Errorf("host leaked into unrelated tool's request: %q", (*calls)[0])
	}
}

// Exactly the 10 peer-targetable tools carry the new host/peer_host schema
// property, and it is never in "required" — everywhere else the schema is
// untouched by this change.
func TestToolSchemasIncludeHostWhereExpected(t *testing.T) {
	c, _ := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, true)

	// Tools that legitimately have a pre-existing, unrelated "host" property
	// (routed hostname), not the new peer-targeting one.
	preexisting := map[string]bool{"set_maintenance": true, "onboard_service": true}

	for _, tool := range s.toolList() {
		props, ok := tool.InputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no properties object", tool.Name)
		}
		req, _ := tool.InputSchema["required"].([]string)
		requiredSet := map[string]bool{}
		for _, r := range req {
			requiredSet[r] = true
		}

		key, wantPeerParam := peerTargetableTools[tool.Name]
		if wantPeerParam {
			if _, present := props[key]; !present {
				t.Errorf("%s: missing %q property", tool.Name, key)
			}
			if requiredSet[key] {
				t.Errorf("%s: %q must not be required", tool.Name, key)
			}
			continue
		}

		if preexisting[tool.Name] {
			// set_maintenance's "host" is its own pre-existing required
			// param; onboard_service's "host" ditto — neither is the new
			// peer-targeting property, so skip the negative check below.
			continue
		}

		if _, present := props["host"]; present {
			t.Errorf("%s: unexpected \"host\" property", tool.Name)
		}
		if _, present := props["peer_host"]; present {
			t.Errorf("%s: unexpected \"peer_host\" property", tool.Name)
		}
	}
}

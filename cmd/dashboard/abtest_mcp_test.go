package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// abToolStub stands in for the dashboard mux: every POST gets 202 with
// accepted, and the n-th GET gets gets[n] (the last one repeated). It
// records each call and its body.
type abToolStub struct {
	mu     sync.Mutex
	calls  []string
	bodies []string
}

func newABToolStub(t *testing.T, accepted string, gets ...string) (*apiCaller, *abToolStub) {
	t.Helper()
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })
	withFastABTool(t)
	st := &abToolStub{}
	n := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		st.mu.Lock()
		defer st.mu.Unlock()
		st.calls = append(st.calls, r.Method+" "+r.URL.RequestURI())
		st.bodies = append(st.bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(accepted))
			return
		}
		body := `{}`
		if len(gets) > 0 {
			body = gets[min(n, len(gets)-1)]
		}
		n++
		w.Write([]byte(body))
	})
	return &apiCaller{mux: h}, st
}

func withFastABTool(t *testing.T) {
	t.Helper()
	old := abToolPollInterval
	abToolPollInterval = time.Millisecond
	t.Cleanup(func() { abToolPollInterval = old })
}

// callTool runs one tools/call and returns the result text and isError.
func callTool(t *testing.T, s *Server, name, args string) (string, bool) {
	t.Helper()
	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	r, ok := res["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result in %v", name, res)
	}
	return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"] == true
}

// get_ab_test is read-only, but reading another host still needs the
// peer opt-in, like every other host-targeted tool.
func TestGetABTestHostNeedsPeerWrites(t *testing.T) {
	c, calls := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, false, false)
	text, isErr := callTool(t, s, "get_ab_test", `{"service":"app","host":"peer-b"}`)
	if !isErr || !strings.Contains(text, "MCP_ALLOW_PEER_WRITES") {
		t.Fatalf("get_ab_test with host = %q (isError %v)", text, isErr)
	}
	if len(*calls) != 0 {
		t.Fatalf("dashboard contacted: %v", *calls)
	}
}

// The A/B tools send exactly the documented bodies: typed fields only, no
// env, abort with no body at all.
func TestABToolsForwardBodies(t *testing.T) {
	c, st := newABToolStub(t, `{"ok":true}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)

	for _, tc := range []struct{ tool, args, wantCall, wantBody string }{
		{"resolve_ab_test", `{"service":"app","action":"promote","force":true,"confirm_aborted":true}`, "POST /api/services/app/ab/promote", `{"force":true,"confirm_aborted":true}`},
		{"resolve_ab_test", `{"service":"app","action":"discard","force":true}`, "POST /api/services/app/ab/discard", `{"force":true}`},
		{"resolve_ab_test", `{"service":"app","action":"reset"}`, "POST /api/services/app/ab/reset", `{"force":false}`},
		{"resolve_ab_test", `{"service":"app","action":"abort"}`, "POST /api/services/app/ab/abort", ``},
		{"set_ab_split", `{"service":"app","split":40}`, "POST /api/services/app/ab/split", `{"split":40}`},
		{"set_ab_groups", `{"service":"app","groups":{}}`, "POST /api/services/app/ab/groups", `{"groups":{}}`},
	} {
		st.calls, st.bodies = nil, nil
		if text, isErr := callTool(t, s, tc.tool, tc.args); isErr {
			t.Fatalf("%s %s: %s", tc.tool, tc.args, text)
		}
		if len(st.calls) != 1 || st.calls[0] != tc.wantCall || st.bodies[0] != tc.wantBody {
			t.Fatalf("%s %s: calls %v bodies %q, want %s %q", tc.tool, tc.args, st.calls, st.bodies, tc.wantCall, tc.wantBody)
		}
	}

	st.calls, st.bodies = nil, nil
	if text, isErr := callTool(t, s, "start_ab_test", `{"service":"app","image":"i:v2","replicas":2,"split":30,"assign":"header","header":"X-Client","exclude":["/api/cron"],"override":false,"thresholds":{"min_samples":100,"err_delta":1.5,"window":"10m"}}`); isErr {
		t.Fatalf("start: %s", text)
	}
	if len(st.bodies) != 1 || strings.Contains(st.bodies[0], `"env"`) {
		t.Fatalf("start body = %q", st.bodies)
	}
	var sent abStartRequest
	if err := json.Unmarshal([]byte(st.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Image != "i:v2" || sent.Replicas != 2 || sent.Split == nil || *sent.Split != 30 || sent.Assign != "header" || sent.Header != "X-Client" ||
		len(sent.Exclude) != 1 || sent.Exclude[0] != "/api/cron" || sent.Override || sent.Thresholds.MinSamples == nil || *sent.Thresholds.MinSamples != 100 ||
		sent.Thresholds.ErrDelta == nil || *sent.Thresholds.ErrDelta != 1.5 || sent.Thresholds.Window != "10m" {
		t.Fatalf("start body = %s", st.bodies[0])
	}
	// The real handler's strict decoder must accept it.
	if _, err := decodeABStart([]byte(st.bodies[0])); err != nil {
		t.Fatalf("decodeABStart(%s) = %v", st.bodies[0], err)
	}
}

// A tool polls GET ab until the accepted op is applied, on the same host.
func TestABToolPollsUntilApplied(t *testing.T) {
	for _, host := range []string{"", "peer-b"} {
		c, st := newABToolStub(t, `{"status":"accepted","op":"relabel"}`,
			`{"state":"active","op":"relabel"}`, `{"state":"active","op":"","config":{"proxy.ab.split":"40"}}`)
		s := NewServer("t", "v")
		registerMCPTools(s, c, true, true)
		args := `{"service":"app","split":40}`
		suffix := ""
		if host != "" {
			args = `{"service":"app","split":40,"host":"` + host + `"}`
			suffix = "?host=" + host
		}
		text, isErr := callTool(t, s, "set_ab_split", args)
		if isErr || !strings.Contains(text, `"proxy.ab.split": "40"`) {
			t.Fatalf("host %q: result %q (isError %v)", host, text, isErr)
		}
		if len(st.calls) != 3 || st.calls[0] != "POST /api/services/app/ab/split"+suffix ||
			st.calls[1] != "GET /api/services/app/ab"+suffix || st.calls[2] != "GET /api/services/app/ab"+suffix {
			t.Fatalf("host %q: calls = %v", host, st.calls)
		}
	}
}

func TestABToolPollOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args, accepted string
		gets                       []string
		wantErr                    bool
		want                       string
	}{
		{"last_error", "resolve_ab_test", `{"service":"app","action":"abort"}`, `{"op":"relabel"}`,
			[]string{`{"state":"active","op":"relabel","last_error":"x"}`}, true, "x"},
		{"start failed", "start_ab_test", `{"service":"app","image":"i:v2"}`, `{"id":"feedf00d","op":"start"}`,
			[]string{`{"state":"active","op":"start"}`, `{"state":"none","history":[{"id":"feedf00d","outcome":"start_failed","detail":"unhealthy"}]}`}, true, "unhealthy"},
		{"start becomes finalize_discard", "start_ab_test", `{"service":"app","image":"i:v2"}`, `{"id":"feedf00d","op":"start"}`,
			[]string{`{"state":"active","op":"finalize_discard","pending":"discard"}`}, false, "finalize_discard"},
		{"force discard finishes", "resolve_ab_test", `{"service":"app","action":"discard","force":true}`, `{"op":"finalize_discard"}`,
			[]string{`{"state":"active","op":"finalize_discard"}`, `{"state":"none","history":[{"outcome":"discarded"}]}`}, false, `"discarded"`},
		{"histograms stripped", "set_ab_split", `{"service":"app","split":5}`, `{"op":"relabel"}`,
			[]string{`{"state":"active","windows":[{"index":1,"variants":{"A":{"requests":3,"hist":[1,2]},"B":{"requests":1,"hist":[3]}}}]}`}, false, `"requests": 3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st := newABToolStub(t, tc.accepted, tc.gets...)
			s := NewServer("t", "v")
			registerMCPTools(s, c, true, false)
			text, isErr := callTool(t, s, tc.tool, tc.args)
			if isErr != tc.wantErr || !strings.Contains(text, tc.want) {
				t.Fatalf("result %q (isError %v), want isError %v containing %q", text, isErr, tc.wantErr, tc.want)
			}
			if strings.Contains(text, `"hist"`) {
				t.Errorf("histograms leaked into the result: %s", text)
			}
			if len(st.calls) != 1+len(tc.gets) {
				t.Errorf("calls = %v, want the POST and %d GETs", st.calls, len(tc.gets))
			}
		})
	}
}

// The five tools against the real dashboard mux and A/B manager.
func TestABToolsAgainstRealMux(t *testing.T) {
	withFastAB(t)
	withFastABTool(t)
	h := newABHarness(t, nil, "")
	h.f.mutateInspect("tpl1", func(in *cenvInspect) { in.env = []string{"A=1", "SECRET=sentinel-do-not-leak"} })
	mux := newLocalTestMux(t, h.dc, nil)
	s := NewServer("t", "v")
	registerMCPTools(s, &apiCaller{mux: mux}, true, false)
	var outputs []string
	call := func(name, args string) (string, bool) {
		t.Helper()
		text, isErr := callTool(t, s, name, args)
		outputs = append(outputs, text)
		return text, isErr
	}

	if text, isErr := call("start_ab_test", `{"service":"app","image":"ghcr.io/org/app:v2","env":{"X":"1"}}`); !isErr || !strings.Contains(text, "image-only") {
		t.Fatalf("start with env = %q", text)
	}
	text, isErr := call("start_ab_test", `{"service":"app","image":"ghcr.io/org/app:v2","replicas":1,"split":20}`)
	if isErr || !strings.Contains(text, `"state": "active"`) || len(h.bReplicas()) != 1 || h.record(t).Op != "" {
		t.Fatalf("start = %q (isError %v), B %d", text, isErr, len(h.bReplicas()))
	}
	abWaitIdle(t, h.m, "app")
	id := h.record(t).ID
	if text, _ := call("get_ab_test", `{"service":"app"}`); !strings.Contains(text, id) {
		t.Fatalf("get_ab_test = %q", text)
	}

	if text, isErr := call("set_ab_split", `{"service":"app","split":40}`); isErr || h.record(t).Config[labelABSplit] != "40" {
		t.Fatalf("split = %q (isError %v)", text, isErr)
	}
	abWaitIdle(t, h.m, "app")
	if text, isErr := call("set_ab_groups", `{"service":"app","groups":{"staff":"B"}}`); isErr || h.bReplicas()[0].Labels[labelABGroups] != "staff:B" {
		t.Fatalf("groups = %q (isError %v)", text, isErr)
	}
	abWaitIdle(t, h.m, "app")

	if text, isErr := call("resolve_ab_test", `{"service":"app","action":"abort"}`); isErr || h.record(t).Phase != abPhaseAborted {
		t.Fatalf("abort = %q (isError %v)", text, isErr)
	}
	abWaitIdle(t, h.m, "app")
	if text, isErr := call("resolve_ab_test", `{"service":"app","action":"promote"}`); !isErr || !strings.Contains(text, "confirm_aborted") {
		t.Fatalf("promote of an aborted B without confirm = %q (isError %v)", text, isErr)
	}

	text, isErr = call("resolve_ab_test", `{"service":"app","action":"discard","force":true}`)
	if isErr || !strings.Contains(text, `"state": "none"`) || !strings.Contains(text, `"discarded"`) || len(h.bReplicas()) != 0 {
		t.Fatalf("force discard = %q (isError %v), B %d", text, isErr, len(h.bReplicas()))
	}

	for _, o := range outputs {
		if strings.Contains(o, "sentinel-do-not-leak") || strings.Contains(o, "SECRET") {
			t.Fatalf("a tool output leaked env: %s", o)
		}
	}
}

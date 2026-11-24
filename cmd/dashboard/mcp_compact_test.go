package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// compactTool calls one tool against a dashboard stub answering body, and
// returns the result text, isError, and the dashboard calls it made.
func compactTool(t *testing.T, body, name, args string) (string, bool, []string) {
	t.Helper()
	c, calls := stubDash(t, 200, body)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)
	text, isErr := callTool(t, s, name, args)
	return text, isErr, *calls
}

// lineFor returns the summary line starting with prefix.
func lineFor(t *testing.T, text, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, text)
	return ""
}

func wantAll(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

func wantNone(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("unexpected %q in:\n%s", sub, s)
		}
	}
}

const compactServicesBody = `[
 {"name":"solo","group":"solo","image":"ghcr.io/org/solo:v3","host":"solo.example","port":80,"replicas":1,"onboarded":true,"member_summaries":null},
 {"name":"app","group":"app","image":"ghcr.io/org/app:v1","host":"app.example","port":8080,"path":"/api","replicas":2,"machine":"pi",
  "update_available":true,"auto_update":true,"autoupdate_skip_reason":"stopped retrying after 3 consecutive failures: boom",
  "labels":{"pmgr.env.origin":"mac","com.docker.compose.project":"stack"},
  "member_summaries":[
   {"name":"goproxy-app-1","id":"aaa111","state":"running"},
   {"name":"goproxy-app-2","id":"bbb222","state":"exited"},
   {"name":"goproxy-app-3","id":"ccc333","state":"running","is_canary":true}]}
]`

func TestListServicesCompactDefault(t *testing.T) {
	text, isErr, _ := compactTool(t, compactServicesBody, "list_services", `{}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	if json.Valid([]byte(text)) {
		t.Fatalf("summary is JSON: %s", text)
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 lines, got %d:\n%s", len(lines), text)
	}
	wantAll(t, lines[0], "2 services", "summary", `service="<name>"`)

	app := lineFor(t, text, "app  ")
	wantAll(t, app, "machine=pi", "app.example/api", "image=ghcr.io/org/app:v1", "replicas=1/2",
		"update-available", "update-blocked", "auto-update", "central-env",
		"goproxy-app-2[exited]", "goproxy-app-3[canary]")
	if !strings.Contains(app, "members=goproxy-app-1,") {
		t.Errorf("running member not bare: %s", app)
	}
	wantNone(t, text, "labels", "com.docker.compose", "stopped retrying")

	solo := lineFor(t, text, "solo  ")
	wantAll(t, solo, "replicas=1", "onboarded")
	wantNone(t, solo, "members=", "machine=", "replicas=1/")
}

func TestListServicesCompactFlagsAndMarkers(t *testing.T) {
	body := `[
 {"name":"ab","image":"i:v1","host":"","replicas":1,"canary_image":"i:v2","canary_replicas":1,"ab_test":{"id":"t1","phase":"running","pending":"promote"},
  "member_summaries":[{"name":"goproxy-ab-1","id":"a","state":"running","health":"healthy"},{"name":"goproxy-ab-2","id":"b","state":"running","is_canary":true,"health":"starting"}]},
 {"name":"can","image":"i:v1","host":"can.example","replicas":1,"canary_image":"i:v2","canary_replicas":2,"all_stopped":true,"image_check_error":"registry said no",
  "unscalable":true,"overlap":true,"dual_tracked":true,"weight":3,
  "member_summaries":[{"name":"goproxy-can-1","id":"a","state":"running","health":"unhealthy"}]}
]`
	text, isErr, _ := compactTool(t, body, "list_services", `{}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	ab := lineFor(t, text, "ab  ")
	wantAll(t, ab, "  unrouted  ", "flags=ab-test:running,ab-pending:promote", "members=goproxy-ab-1,goproxy-ab-2[canary,starting]")
	wantNone(t, ab, "canary:")
	can := lineFor(t, text, "can  ")
	wantAll(t, can, "flags=all-stopped,image-check-error,canary:i:v2x2,unscalable,overlap,dual-tracked,weight:3", "members=goproxy-can-1[unhealthy]")
	wantNone(t, text, "registry said no")
}

func TestListServicesCompactSorted(t *testing.T) {
	body := `[{"name":"zeta","image":"i","host":"z.example","replicas":1,"member_summaries":null},
 {"name":"app","image":"i","host":"a.example","replicas":1,"machine":"pi","member_summaries":null},
 {"name":"app","image":"i","host":"a.example","replicas":1,"machine":"mac","member_summaries":null},
 {"name":"app","image":"i","host":"a.example","replicas":1,"member_summaries":null}]`
	text, _, _ := compactTool(t, body, "list_services", `{}`)
	lines := strings.Split(text, "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got:\n%s", text)
	}
	for i, want := range []string{"app  a.example", "app  machine=mac", "app  machine=pi", "zeta  "} {
		if !strings.HasPrefix(lines[i+1], want) {
			t.Errorf("line %d = %q, want prefix %q", i+1, lines[i+1], want)
		}
	}
}

func TestListServicesDetailByName(t *testing.T) {
	body := `[{"name":"app","image":"i:v1","host":"a.example","replicas":1,"machine":"pi","labels":{"com.docker.compose.project":"stack"},"member_summaries":[{"name":"goproxy-app-1","id":"aaa111","state":"running"}]},
 {"name":"other","image":"i:v9","host":"o.example","replicas":1,"member_summaries":null},
 {"name":"app","image":"i:v1","host":"a.example","replicas":1,"machine":"mac","labels":{"proxy.enable":"true"},"member_summaries":null}]`
	text, isErr, _ := compactTool(t, body, "list_services", `{"service":"app"}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("detail is not a JSON array: %v\n%s", err, text)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d:\n%s", len(got), text)
	}
	wantAll(t, text, `"aaa111"`, `"pi"`, `"mac"`)
	wantNone(t, text, "labels", "com.docker.compose", "o.example")
}

func TestListServicesUnknownName(t *testing.T) {
	body := `[{"name":"zeta","image":"i","host":"z","replicas":1,"member_summaries":null},
 {"name":"app","image":"i","host":"a","replicas":1,"machine":"pi","member_summaries":null},
 {"name":"app","image":"i","host":"a","replicas":1,"member_summaries":null}]`
	text, isErr, calls := compactTool(t, body, "list_services", `{"service":"nope"}`)
	if !isErr {
		t.Fatalf("unknown service accepted: %s", text)
	}
	wantAll(t, text, `no service named "nope"`, "valid names: app, zeta")
	if len(calls) != 1 {
		t.Errorf("calls = %v, want exactly one", calls)
	}

	text, isErr, _ = compactTool(t, `[]`, "list_services", `{"service":"nope"}`)
	if !isErr || !strings.Contains(text, "no services are managed") {
		t.Errorf("empty list: %q (isError %v)", text, isErr)
	}
}

func TestListServicesEmpty(t *testing.T) {
	for _, body := range []string{`[]`, `null`} {
		text, isErr, _ := compactTool(t, body, "list_services", `{}`)
		if isErr || text != "No services." {
			t.Errorf("body %s: %q (isError %v)", body, text, isErr)
		}
	}
}

// A body that isn't the expected array passes through as pretty JSON on
// both paths rather than becoming an error or an empty summary.
func TestListToolsDecodeFallback(t *testing.T) {
	cases := []struct{ tool, args string }{
		{"list_services", `{}`},
		{"list_services", `{"service":"app"}`},
		{"list_routes", `{}`},
		{"list_routes", `{"hostname":"app.example"}`},
		{"list_dns", `{}`},
		{"list_dns", `{"name":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			text, isErr, _ := compactTool(t, `{"unexpected":"shape"}`, tc.tool, tc.args)
			if isErr {
				t.Fatalf("errored: %s", text)
			}
			if want := "{\n  \"unexpected\": \"shape\"\n}"; text != want {
				t.Errorf("got %q, want %q", text, want)
			}
		})
	}
}

const compactRoutesBody = `[
 {"host":"app.example","path":"/api","service":"app","backends":[{"url":"http://10.0.0.2:8080","weight":1,"container":"goproxy-app-1"},{"url":"http://10.0.0.3:8080","weight":2,"container":"goproxy-app-2"}]},
 {"host":"legacy.example","strip":true,"name":"legacy","backends":[{"url":"http://192.168.1.9:3000","weight":1,"container":"static"}]},
 {"host":"dead.example","backends":[]}
]`

func TestListRoutesCompactDefault(t *testing.T) {
	text, isErr, _ := compactTool(t, compactRoutesBody, "list_routes", `{}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	if json.Valid([]byte(text)) {
		t.Fatalf("summary is JSON: %s", text)
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 lines, got:\n%s", text)
	}
	wantAll(t, lines[0], "3 routes", "summary", `hostname="<hostname>"`)

	app := lineFor(t, text, "app.example/api")
	wantAll(t, app, "service=app", "source=containers", "backends=2: goproxy-app-1, goproxy-app-2(w=2)")
	wantNone(t, app, "10.0.0.2", "goproxy-app-1(w", "flags=", "name=")

	legacy := lineFor(t, text, "legacy.example")
	wantAll(t, legacy, "name=legacy", "source=static", "backends=1: http://192.168.1.9:3000", "flags=strip")
	wantNone(t, legacy, "service=", "(w=")

	dead := lineFor(t, text, "dead.example")
	wantAll(t, dead, "flags=no-backends")
	wantNone(t, dead, "source=")
}

func TestListRoutesCompactBackendCapAndMixed(t *testing.T) {
	body := `[{"host":"big.example","backends":[
 {"url":"http://u1","weight":1,"container":"c1"},{"url":"http://u2","weight":1,"container":"c2"},
 {"url":"http://u3","weight":1,"container":"c3"},{"url":"http://u4","weight":1,"container":"static"},
 {"url":"http://u5","weight":1,"container":"c5"},{"url":"http://u6","weight":1,"container":"c6"}]}]`
	text, _, _ := compactTool(t, body, "list_routes", `{}`)
	wantAll(t, text, "source=mixed", "backends=6: c1, c2, c3, http://u4, +2 more")
	wantNone(t, text, "c5", "c6")
}

func TestListRoutesDetailByHostname(t *testing.T) {
	text, isErr, calls := compactTool(t, compactRoutesBody, "list_routes", `{"hostname":"APP.Example"}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("detail is not a JSON array: %v\n%s", err, text)
	}
	if len(got) != 1 || got[0]["host"] != "app.example" {
		t.Fatalf("got %v", got)
	}
	wantAll(t, text, "http://10.0.0.2:8080", "goproxy-app-2")
	wantNone(t, text, "legacy.example")
	if len(calls) != 1 || calls[0] != "GET /api/routes" {
		t.Errorf("calls = %v", calls)
	}
}

func TestListRoutesUnknownHostname(t *testing.T) {
	text, isErr, _ := compactTool(t, compactRoutesBody, "list_routes", `{"hostname":"nope.example"}`)
	if !isErr {
		t.Fatalf("unknown hostname accepted: %s", text)
	}
	wantAll(t, text, `no route for hostname "nope.example"`, "routed hosts: app.example, dead.example, legacy.example")
}

func TestListRoutesEmpty(t *testing.T) {
	for _, body := range []string{`[]`, `null`} {
		text, isErr, _ := compactTool(t, body, "list_routes", `{}`)
		if isErr || text != "No routes." {
			t.Errorf("body %s: %q (isError %v)", body, text, isErr)
		}
	}
}

func compactDNSBody() string {
	long := strings.Repeat("k", 300)
	return `[
 {"id":"rec3","type":"TXT","name":"example.com","content":"` + long + `","proxied":false,"ttl":300},
 {"id":"rec1","type":"A","name":"app.example.com","content":"1.2.3.4","proxied":true,"ttl":1},
 {"id":"rec2","type":"CNAME","name":"www.app.example.com","content":"app.example.com","proxied":false,"ttl":1}
]`
}

func TestListDNSCompactDefault(t *testing.T) {
	text, isErr, _ := compactTool(t, compactDNSBody(), "list_dns", `{}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	if json.Valid([]byte(text)) {
		t.Fatalf("summary is JSON: %s", text)
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 lines, got:\n%s", text)
	}
	wantAll(t, lines[0], "3 DNS records", "summary", `name="<record name>"`)
	for _, l := range lines[1:] {
		if !strings.Contains(l, "  id=rec") {
			t.Errorf("line without id: %s", l)
		}
	}
	// Sorted by name, then type.
	if want := "A  app.example.com  ->  1.2.3.4  proxied  id=rec1"; lines[1] != want {
		t.Errorf("line 1 = %q, want %q", lines[1], want)
	}
	if want := "TXT  example.com  ->  " + strings.Repeat("k", 80) + "…  dns-only  ttl=300  id=rec3"; lines[2] != want {
		t.Errorf("line 2 = %q, want %q", lines[2], want)
	}
	if want := "CNAME  www.app.example.com  ->  app.example.com  dns-only  id=rec2"; lines[3] != want {
		t.Errorf("line 3 = %q, want %q", lines[3], want)
	}
}

func TestListDNSDetailByName(t *testing.T) {
	// Exact (case-insensitive) beats substring: www.app.example.com also
	// contains the name but is left out.
	text, isErr, _ := compactTool(t, compactDNSBody(), "list_dns", `{"name":"APP.example.com"}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("detail is not a JSON array: %v\n%s", err, text)
	}
	if len(got) != 1 || got[0]["id"] != "rec1" {
		t.Fatalf("exact match: got %v", got)
	}

	// No exact match: every record containing the name.
	text, isErr, _ = compactTool(t, compactDNSBody(), "list_dns", `{"name":"app.example"}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || len(got) != 2 {
		t.Fatalf("substring match: err %v, got %v", err, got)
	}

	// Detail carries the content untruncated.
	text, _, _ = compactTool(t, compactDNSBody(), "list_dns", `{"name":"example.com"}`)
	if err := json.Unmarshal([]byte(text), &got); err != nil || len(got) != 1 {
		t.Fatalf("exact apex match: err %v, got %v", err, got)
	}
	if got[0]["content"] != strings.Repeat("k", 300) {
		t.Errorf("content truncated in detail: %v", got[0]["content"])
	}
}

func TestListDNSNoMatch(t *testing.T) {
	text, isErr, _ := compactTool(t, compactDNSBody(), "list_dns", `{"name":"nope"}`)
	if !isErr {
		t.Fatalf("no-match accepted: %s", text)
	}
	wantAll(t, text, `no DNS record matching "nope"`, "among 3 records", "list_dns without name")
	wantNone(t, text, "app.example.com", "rec1")
}

func TestListDNSEmpty(t *testing.T) {
	for _, body := range []string{`[]`, `null`} {
		text, isErr, _ := compactTool(t, body, "list_dns", `{}`)
		if isErr || text != "No DNS records." {
			t.Errorf("body %s: %q (isError %v)", body, text, isErr)
		}
	}
}

func TestListDNSNameKeepsZone(t *testing.T) {
	_, isErr, calls := compactTool(t, compactDNSBody(), "list_dns", `{"zone":"polardev.org","name":"app.example.com"}`)
	if isErr {
		t.Fatal("errored")
	}
	if len(calls) != 1 || calls[0] != "GET /api/cf/records?zone=polardev.org" {
		t.Errorf("calls = %v", calls)
	}
}

func TestListToolSchemasDocumentSelector(t *testing.T) {
	c, _ := stubDash(t, 200, `{}`)
	s := NewServer("t", "v")
	registerMCPTools(s, c, true, false)
	want := map[string]string{"list_services": "service", "list_routes": "hostname", "list_dns": "name"}
	seen := 0
	for _, tool := range s.toolList() {
		sel, ok := want[tool.Name]
		if !ok {
			continue
		}
		seen++
		if !strings.Contains(strings.ToLower(tool.Description), "summary") || !strings.Contains(tool.Description, sel) {
			t.Errorf("%s: description doesn't explain the summary/%s split: %s", tool.Name, sel, tool.Description)
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, present := props[sel]; !present {
			t.Errorf("%s: missing %q property", tool.Name, sel)
		}
		req, _ := tool.InputSchema["required"].([]string)
		for _, r := range req {
			if r == sel {
				t.Errorf("%s: %q must not be required", tool.Name, sel)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of %d tools", seen, len(want))
	}
}

func TestGetLogsGroupsStreamsAndCollapsesRepeats(t *testing.T) {
	body := `[{"stream":"stdout","text":"boot"},
 {"stream":"stderr","text":"probe failed"},{"stream":"stderr","text":"probe failed"},{"stream":"stderr","text":"probe failed"},
 {"stream":"stderr","text":"other"},{"stream":"stdout","text":"probe failed"}]`
	text, isErr, _ := compactTool(t, body, "get_logs", `{"container":"c"}`)
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	want := "6 log lines — grouped by stream, times UTC, consecutive repeats collapsed as (×N).\n" +
		"[stdout]\nboot\n[stderr]\nprobe failed  (×3)\nother\n[stdout]\nprobe failed"
	if text != want {
		t.Errorf("got:\n%s\nwant:\n%s", text, want)
	}
	wantNone(t, text, `"stream"`, `"text"`)
}

func TestGetLogsWrappedEmptyAndFallback(t *testing.T) {
	text, _, _ := compactTool(t, `{"container":"c","lines":[{"stream":"stderr","text":"x"}]}`, "get_logs", `{"container":"c"}`)
	wantAll(t, text, "1 log lines", "[stderr]\nx")
	text, _, _ = compactTool(t, `[]`, "get_logs", `{"container":"c"}`)
	wantAll(t, text, "0 log lines.")
	text, _, _ = compactTool(t, `{"error":"nope"}`, "get_logs", `{"container":"c"}`)
	wantAll(t, text, `"error"`, "nope")
}

func TestGetLogsTimestamps(t *testing.T) {
	body := `[{"stream":"stderr","text":"probe failed","ts":"2026-11-24T01:00:00Z"},
 {"stream":"stderr","text":"probe failed","ts":"2026-11-24T01:00:10Z"},
 {"stream":"stderr","text":"done","ts":"2026-11-24T01:00:11Z"}]`
	text, _, _ := compactTool(t, body, "get_logs", `{"container":"c"}`)
	wantAll(t, text,
		"2026-11-24T01:00:00Z..2026-11-24T01:00:10Z probe failed  (×2)",
		"\n2026-11-24T01:00:11Z done")
}

func TestSplitLogTimestamp(t *testing.T) {
	for _, c := range []struct{ in, ts, text string }{
		{"2026-11-24T01:02:03.123456789Z hello world", "2026-11-24T01:02:03Z", "hello world"},
		{"2026-11-24T01:02:03.5-08:00 x", "2026-11-24T09:02:03Z", "x"},
		{"2026-11-24T01:02:03Z", "2026-11-24T01:02:03Z", ""},
		{"no stamp here", "", "no stamp here"},
		{"", "", ""},
	} {
		ts, text := splitLogTimestamp(c.in)
		if ts != c.ts || text != c.text {
			t.Errorf("%q: got (%q,%q) want (%q,%q)", c.in, ts, text, c.ts, c.text)
		}
	}
}

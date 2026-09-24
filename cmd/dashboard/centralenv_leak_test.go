package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// centralEnvPropagationOutputs drives every surface PR-B adds — the peer
// endpoints, the propagation manager (origin revert, peer rollback),
// reconcile (secret rotation), the dashboard API (forwarded set, conflict,
// relayed rejection, origin-down) and the MCP tools — over a two-host mesh
// whose env holds sentinel everywhere a value can live, and returns every
// response body, job/rolling-op state and the audit log for
// TestCentralEnvNeverLeaksValues to check. The caller captures log output.
func centralEnvPropagationOutputs(t *testing.T, sentinel string) []string {
	withFastSync(t)
	readAudit := withAuditFile(t)
	a, b, _ := newMesh(t, true)
	var out []string
	add := func(what, s string) { out = append(out, what+": "+s) }
	addJSON := func(what string, v any) {
		j, _ := json.Marshal(v)
		add(what, string(j))
	}
	jobs := func(what string) {
		for _, n := range []*meshNode{a, b} {
			j, _ := n.m.get("app")
			addJSON(what+" job "+n.identity, j)
			st, _ := n.rom.get("app")
			addJSON(what+" rolling "+n.identity, st)
			addJSON(what+" last_failure "+n.identity, n.m.failure("app"))
		}
	}

	secretsDir := t.TempDir()
	writeSecret := func(v string) {
		if err := os.WriteFile(filepath.Join(secretsDir, "app.env"), []byte("TOK="+v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSecret(sentinel)
	a.ce.secrets = &secretsStore{dir: secretsDir}
	a.ce.store.Create("app", "dashboard-a", map[string]string{"P": sentinel, "TOK": "ref:TOK"}, map[string]map[string]string{"dashboard-b": {"O": sentinel}}, "test", "")
	a.f.seedMember("a1", "goproxy-app-1", "dashboard-a", 1, []string{"P=" + sentinel, "TOK=" + sentinel}, cenvTemplateHealth)
	b.ce.cache.PutWithHealthcheck("app", "dashboard-a", 1, []string{"O=" + sentinel, "P=" + sentinel, "TOK=" + sentinel}, cenvTemplateHealth)
	b.f.seedMember("b1", "goproxy-app-1", "dashboard-a", 1, []string{"O=" + sentinel, "P=" + sentinel, "TOK=" + sentinel}, cenvTemplateHealth)

	api := func(n *meshNode, method, path, body string) {
		rec := apiDo(t, n.mux, method, path, body)
		add(n.identity+" "+method+" "+path, rec.Body.String())
	}
	// Forwarded set, conflict, relayed bad key / bad ref.
	api(b, "POST", "/api/services/app/env", `{"if_version":1,"set":{"P2":"`+sentinel+`"}}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	api(b, "POST", "/api/services/app/env", `{"if_version":1,"set":{"P":"`+sentinel+`"}}`)
	api(b, "POST", "/api/services/app/env", `{"if_version":2,"set":{"BAD=K":"`+sentinel+`"}}`)
	api(b, "POST", "/api/services/app/env", `{"if_version":2,"set":{"R":"ref:NOPE","P3":"`+sentinel+`"}}`)
	jobs("forwarded")

	// Gate failure on the origin -> revert.
	a.f.setUnhealthy(func(c createBody) bool { return envHas(c.Env, "Q="+sentinel) })
	api(a, "POST", "/api/services/app/env", `{"if_version":2,"set":{"Q":"`+sentinel+`"}}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	jobs("origin-revert")

	// Gate failure on the peer -> rollback, origin partial.
	b.f.setUnhealthy(func(c createBody) bool { return envHas(c.Env, "O2="+sentinel) })
	cur := storeVersion(t, a.syncHost)
	api(a, "POST", "/api/services/app/env", `{"if_version":`+jsonNum(cur)+`,"host_overrides":{"dashboard-b":{"O2":"`+sentinel+`"}}}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	jobs("peer-rollback")
	api(a, "GET", "/api/services/app/env", "")
	api(b, "GET", "/api/services/app/env", "")

	// Secret rotation picked up by reconcile.
	a.m.reconcileOnce(context.Background())
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	writeSecret(sentinel + "-rotated")
	a.m.reconcileOnce(context.Background())
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	b.m.reconcileOnce(context.Background())
	b.waitJob(t, "app")
	jobs("rotation")

	// Peer endpoints other than the (by-design secret-bearing) GET env.
	for _, n := range []*meshNode{a, b} {
		for _, p := range []struct{ method, path, body string }{
			{"GET", "/peer/central-env/app/status", ""},
			{"POST", "/peer/central-env/app/notify", `{"origin":"dashboard-a","version":99}`},
			{"POST", "/peer/central-env/app/set", `{"if_version":1,"set":{"P":"` + sentinel + `"}}`},
			{"POST", "/peer/central-env/app/set", `{"if_version":99,"set":{"BAD=K":"` + sentinel + `"}}`},
		} {
			rec := peerDo(t, n.peerMux, p.method, p.path, "s3cret", p.body)
			add(n.identity+" peer "+p.method+" "+p.path, rec.Body.String())
		}
	}
	a.waitJob(t, "app")
	b.waitJob(t, "app")

	// Origin down: stale view, refused set, scale from cache.
	a.stop()
	api(b, "GET", "/api/services/app/env", "")
	api(b, "POST", "/api/services/app/env", `{"if_version":1,"set":{"P":"`+sentinel+`"}}`)
	if err := b.dc.scaleService(context.Background(), "app", 2); err != nil {
		add("scale", err.Error())
	}
	a.start(t)

	// MCP: literal-credential refusal, a real edit, the names-only view.
	s := NewServer("t", "v")
	registerMCPTools(s, &apiCaller{mux: b.mux}, true, true)
	cur = storeVersion(t, a.syncHost)
	for _, args := range []string{
		`{"service":"app","if_version":1,"set":{"API_TOKEN":"` + sentinel + `"}}`,
		`{"service":"app","if_version":1,"set":{"P":"` + sentinel + `"}}`,
		`{"service":"app","if_version":` + jsonNum(cur) + `,"set":{"P4":"` + sentinel + `"}}`,
	} {
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_service_env","arguments":`+args+`}}`)
		addJSON("mcp set", res)
	}
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	for _, tool := range []string{"get_service_env", "sync_service_env"} {
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{"service":"app"}}}`)
		addJSON("mcp "+tool, res)
	}
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	jobs("final")
	for _, n := range []*meshNode{a, b} {
		rec := peerDo(t, n.peerMux, "POST", "/peer/central-env/app/release", "s3cret", "")
		add(n.identity+" peer release", rec.Body.String())
	}

	// A Docker error that echoes the env it was handed: the rolling-op
	// LastError, the job's error and last_failure must all be scrubbed.
	a.f.mu.Lock()
	a.f.createErr = func(c createBody) string {
		if envHas(c.Env, "E="+sentinel) {
			return "invalid container config: " + strings.Join(c.Env, " ")
		}
		return ""
	}
	a.f.mu.Unlock()
	cur = storeVersion(t, a.syncHost)
	api(a, "POST", "/api/services/app/env", `{"if_version":`+jsonNum(cur)+`,"set":{"E":"`+sentinel+`"}}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	jobs("docker-echo")
	api(a, "GET", "/api/services/app/env", "")
	api(a, "GET", "/api/services/app/rolling-replace", "")
	res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_service_env","arguments":{"service":"app"}}}`)
	addJSON("mcp get_service_env after docker echo", res)
	for _, n := range []*meshNode{a, b} {
		rec := peerDo(t, n.peerMux, "GET", "/peer/central-env/app/status", "s3cret", "")
		add(n.identity+" peer status after docker echo", rec.Body.String())
	}

	// The same echo through paths that don't pin the env themselves: an
	// operator's image-only rolling replace and replace (Resolve builds the
	// env inside the create path), a scale, and the auto-updater — whose
	// error lands in its log, audit and sticky block reason.
	// (Started on the manager directly: the API's two-replica capacity
	// guard would refuse this single-replica mesh before any create.)
	if _, err := a.rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rolling replace to end", func() bool {
		st, ok := a.rom.get("app")
		return ok && !rollingOpActive(st.Status)
	})
	if st, _ := a.rom.get("app"); st.Status != rollingOpStatusFailed {
		t.Errorf("image-only rolling replace = %s, want it to fail on the echoing create", st.Status)
	}
	api(a, "GET", "/api/services/app/rolling-replace", "")
	api(a, "POST", "/api/services/app/replace", `{"image":"ghcr.io/org/app:v1"}`)
	api(a, "POST", "/api/services/app/scale", `{"replicas":3}`)
	oldGap := autoUpdateGap
	autoUpdateGap = 0
	a.f.mu.Lock()
	for id, c := range a.f.items {
		if c.Labels[labelService] == "app" {
			c.Labels[labelAutoUpdate] = "true"
			a.f.items[id] = c
		}
	}
	a.f.mu.Unlock()
	ic := newImageChecker(a.dc)
	blocks := newAutoUpdateBlockStore()
	au := newAutoUpdater(a.dc, ic, newTestOnboardedStore(t), "", a.rm.proxyURL, blocks, a.rm, a.rom)
	for i := 0; i < autoUpdateMaxFailures; i++ {
		ic.statuses["ghcr.io/org/app:v1"] = &imageStatus{Image: "ghcr.io/org/app:v1", UpdateAvailable: true}
		au.runOnce(context.Background())
	}
	autoUpdateGap = oldGap
	add("autoupdate block reason", blocks.Get("app"))
	if blocks.Get("app") == "" {
		t.Error("the auto-updater never failed on the echoing create — the leak check would be vacuous")
	}
	a.waitJob(t, "app")
	b.waitJob(t, "app")

	entries := readAudit()
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e["action"].(string)] = true
	}
	for _, want := range []string{"service.env_set", "service.env_sync_start", "service.env_sync_done", "service.env_sync_failed", "service.env_revert", "service.env_secret_rotated", "service.env_release"} {
		if !actions[want] {
			t.Errorf("audit never recorded %s (got %v)", want, actions)
		}
	}
	addJSON("audit", entries)
	return out
}

func jsonNum(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCentralEnvPropagationOutputsAreNotEmpty(t *testing.T) {
	// Guard against the leak check passing vacuously: the driven paths must
	// actually have produced the errors/states they're meant to — including
	// a Docker error that echoed env (so "[redacted]" proves the scrub ran).
	outs := strings.Join(centralEnvPropagationOutputs(t, "SENTINEL-S3CRET-9f2a"), "\n")
	for _, want := range []string{`"current_version"`, envSyncStatusPartial, envSyncHostRolledBack, "ref:NAME", `"stale":true`,
		`"last_failure"`, envSyncStatusFailedReverted, "[redacted]", "invalid container config"} {
		if !strings.Contains(outs, want) {
			t.Errorf("outputs never contained %q", want)
		}
	}
	// A rotated secret is the old value plus a suffix: redacting the old
	// one out of it first would leave "-rotated" behind.
	if strings.Contains(outs, "]-rotated") {
		t.Error("a value's tail survived redaction of a shorter value it starts with")
	}
}

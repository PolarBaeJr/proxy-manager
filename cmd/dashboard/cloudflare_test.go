package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseCloudflareZones(t *testing.T) {
	type want struct {
		domain, zoneID, token string
	}
	cases := []struct {
		name      string
		spec      string
		defToken  string
		want      []want
		wantSkips int
	}{
		{
			name:     "single entry",
			spec:     "polardev.org:zone1",
			defToken: "tok",
			want:     []want{{"polardev.org", "zone1", "tok"}},
		},
		{
			name:     "two entries",
			spec:     "polardev.org:zone1,the-aquarium.com:zone2",
			defToken: "tok",
			want:     []want{{"polardev.org", "zone1", "tok"}, {"the-aquarium.com", "zone2", "tok"}},
		},
		{
			name:     "per-zone token override",
			spec:     "the-aquarium.com:zone2:othertok",
			defToken: "tok",
			want:     []want{{"the-aquarium.com", "zone2", "othertok"}},
		},
		{
			name:     "surrounding whitespace",
			spec:     "  polardev.org : zone1 ,  the-aquarium.com:zone2 ",
			defToken: "tok",
			want:     []want{{"polardev.org", "zone1", "tok"}, {"the-aquarium.com", "zone2", "tok"}},
		},
		{
			name:     "uppercase domain normalizes",
			spec:     "PolarDev.ORG:zone1",
			defToken: "tok",
			want:     []want{{"polardev.org", "zone1", "tok"}},
		},
		{
			name:      "malformed entries skipped",
			spec:      "justdomain,a:,:b,,polardev.org:zone1",
			defToken:  "tok",
			want:      []want{{"polardev.org", "zone1", "tok"}},
			wantSkips: 3,
		},
		{
			name:      "empty spec",
			spec:      "",
			defToken:  "tok",
			wantSkips: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clients, skipped := parseCloudflareZones(tc.spec, tc.defToken)
			if len(clients) != len(tc.want) {
				t.Fatalf("clients = %d, want %d", len(clients), len(tc.want))
			}
			for i, w := range tc.want {
				c := clients[i]
				if c.domain != w.domain || c.zoneID != w.zoneID || c.token != w.token {
					t.Errorf("client[%d] = {%s %s %s}, want {%s %s %s}", i, c.domain, c.zoneID, c.token, w.domain, w.zoneID, w.token)
				}
			}
			if len(skipped) != tc.wantSkips {
				t.Fatalf("skipped = %v, want %d entries", skipped, tc.wantSkips)
			}
			for _, s := range skipped {
				if strings.TrimSpace(s) == "" {
					t.Errorf("skip reason must be non-empty, got %q", s)
				}
			}
		})
	}
}

// The prod .env only sets the three legacy vars — this test is the guarantee
// that multi-zone support didn't change their behaviour.
func TestRegistryLegacyOnly(t *testing.T) {
	env := map[string]string{
		"CLOUDFLARE_API_TOKEN": "tok",
		"CLOUDFLARE_ZONE_ID":   "zone1",
		"CLOUDFLARE_DOMAIN":    "polardev.org",
	}
	r, _ := newCloudflareRegistryFromEnv(func(k string) string { return env[k] })
	if r == nil {
		t.Fatal("registry = nil, want the legacy zone")
	}
	if got := r.Domains(); len(got) != 1 || got[0] != "polardev.org" {
		t.Fatalf("Domains() = %v, want [polardev.org]", got)
	}
	c, ok := r.Lookup("")
	if !ok || c.zoneID != "zone1" || c.token != "tok" {
		t.Fatalf("Lookup(\"\") = %v, %v — want the legacy zone", c, ok)
	}
	if r.DefaultDomain() != "polardev.org" {
		t.Fatalf("DefaultDomain() = %q, want polardev.org", r.DefaultDomain())
	}
	if _, ok := r.Lookup("other.com"); ok {
		t.Fatal("Lookup(other.com) = ok, want unknown zone")
	}
	// Case-insensitive, and no zone IDs or tokens ever reach Status().
	if _, ok := r.Lookup("PolarDev.org"); !ok {
		t.Fatal("Lookup is not case-insensitive")
	}
	st := r.Status()
	if len(st) != 1 || st[0]["domain"] != "polardev.org" || st[0]["ok"] != true {
		t.Fatalf("Status() = %v", st)
	}
	for _, s := range st {
		for _, v := range s {
			if s, isStr := v.(string); isStr && (strings.Contains(s, "zone1") || strings.Contains(s, "tok")) {
				t.Fatalf("Status() leaks zone id or token: %v", st)
			}
		}
	}
}

func TestRegistryNilSafe(t *testing.T) {
	env := map[string]string{}
	r, msgs := newCloudflareRegistryFromEnv(func(k string) string { return env[k] })
	if r != nil {
		t.Fatalf("registry = %v, want nil when nothing is configured", r)
	}
	if len(msgs) != 0 {
		t.Fatalf("msgs = %v, want none", msgs)
	}
	// renderDNS polls /api/cf/enabled every 5s even when unconfigured.
	if got := r.Status(); got != nil {
		t.Fatalf("Status() = %v, want nil", got)
	}
	if got := r.Domains(); got != nil {
		t.Fatalf("Domains() = %v, want nil", got)
	}
	if got := r.DefaultDomain(); got != "" {
		t.Fatalf("DefaultDomain() = %q, want empty", got)
	}
	if c, ok := r.Lookup("polardev.org"); ok || c != nil {
		t.Fatalf("Lookup on nil registry = %v, %v", c, ok)
	}
	r.noteResult("polardev.org", nil)
}

func TestRegistryLegacyPlusZones(t *testing.T) {
	env := map[string]string{
		"CLOUDFLARE_API_TOKEN": "tok",
		"CLOUDFLARE_ZONE_ID":   "zone1",
		"CLOUDFLARE_DOMAIN":    "polardev.org",
		"CLOUDFLARE_ZONES":     "polardev.org:dupzone,the-aquarium.com:zone2:othertok,sfubadminton.com:zone3",
	}
	r, msgs := newCloudflareRegistryFromEnv(func(k string) string { return env[k] })
	if r == nil {
		t.Fatal("registry = nil")
	}
	want := []string{"polardev.org", "the-aquarium.com", "sfubadminton.com"}
	got := r.Domains()
	if len(got) != len(want) {
		t.Fatalf("Domains() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Domains() = %v, want %v (default first)", got, want)
		}
	}
	// The legacy zone stays the default even though CLOUDFLARE_ZONES relists it.
	if r.DefaultDomain() != "polardev.org" {
		t.Fatalf("DefaultDomain() = %q, want polardev.org", r.DefaultDomain())
	}
	def, _ := r.Lookup("polardev.org")
	if def.zoneID != "zone1" {
		t.Fatalf("polardev.org zone id = %q, want zone1 (duplicate must be skipped)", def.zoneID)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "polardev.org") {
		t.Fatalf("msgs = %v, want one duplicate-skip line", msgs)
	}
	// Per-zone token override wins; zones without one inherit the global token.
	aq, _ := r.Lookup("the-aquarium.com")
	if aq.token != "othertok" || aq.zoneID != "zone2" {
		t.Fatalf("the-aquarium.com = {%s %s}, want {othertok zone2}", aq.token, aq.zoneID)
	}
	sfu, _ := r.Lookup("sfubadminton.com")
	if sfu.token != "tok" {
		t.Fatalf("sfubadminton.com token = %q, want the global token", sfu.token)
	}
}

func TestCloudflareListAgainstFake(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath, gotAuth = req.URL.Path, req.Header.Get("Authorization")
		w.Write([]byte(`{"result":[
			{"id":"2","type":"A","name":"b.example.com","content":"1.2.3.4"},
			{"id":"3","type":"CNAME","name":"a.example.com","content":"x.example.com"},
			{"id":"1","type":"A","name":"a.example.com","content":"1.2.3.4"}
		]}`))
	}))
	defer srv.Close()

	c := newCloudflareClient("zonetok", "zoneABC", "example.com")
	c.baseURL = srv.URL
	recs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Sorted by name, then type.
	wantIDs := []string{"1", "3", "2"}
	if len(recs) != len(wantIDs) {
		t.Fatalf("recs = %v, want %d", recs, len(wantIDs))
	}
	for i, id := range wantIDs {
		if recs[i].ID != id {
			t.Fatalf("record order = %v, want ids %v", recs, wantIDs)
		}
	}
	if !strings.Contains(gotPath, "/zones/zoneABC/dns_records") {
		t.Fatalf("path = %q, want the per-zone id", gotPath)
	}
	if gotAuth != "Bearer zonetok" {
		t.Fatalf("authorization = %q, want the per-zone token", gotAuth)
	}
}

func TestCloudflareErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"message":"Authentication error"}]}`, http.StatusForbidden)
	}))
	defer srv.Close()

	c := newCloudflareClient("tok", "zoneABC", "example.com")
	c.baseURL = srv.URL
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("List err = nil, want 403")
	}
	if cfStatus(err) != http.StatusForbidden {
		t.Fatalf("cfStatus = %d, want 403", cfStatus(err))
	}
	if !strings.HasPrefix(err.Error(), "cloudflare GET /zones/zoneABC/dns_records") {
		t.Fatalf("err = %q, want the unchanged message format", err)
	}
	if cfStatus(context.Canceled) != 0 {
		t.Fatalf("cfStatus(non-API error) = %d, want 0", cfStatus(context.Canceled))
	}
}

// The UI's api() helper reacts to 401 ("session expired") and 403 ("2FA
// required"), so a zone the token can't reach must never answer with either —
// the DNS tab polls every 5s and would pop an auth dialog each time.
func TestWriteCFErrNeverReflectsAuthStatus(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		rec := httptest.NewRecorder()
		writeCFErr(rec, "polardev.org", cfAPIError{Status: code, Method: "GET", Path: "/zones/z/dns_records"})
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Fatalf("cloudflare %d written as %d, want a non-auth status", code, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "token lacks permission for zone polardev.org") {
			t.Fatalf("body = %q, want the per-zone message", rec.Body.String())
		}
	}
	// A transient (non-API) failure keeps the old JSON 500 shape.
	rec := httptest.NewRecorder()
	writeCFErr(rec, "polardev.org", context.Canceled)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("transient error status = %d, want 500", rec.Code)
	}
}

func TestCreateNameZoneMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Create must reject a foreign-zone name before any HTTP call")
		w.Write([]byte(`{"result":{}}`))
	}))
	defer srv.Close()

	c := newCloudflareClient("tok", "zoneABC", "example.com")
	c.baseURL = srv.URL
	if _, err := c.Create(context.Background(), CreateDNSRequest{Name: "app.other.com", Content: "x"}); err == nil {
		t.Fatal("Create err = nil, want a zone-mismatch error")
	}
	// Bare and in-zone names are still fine (the server would be hit, so only
	// validateName is exercised here).
	if err := c.validateName("myapp"); err != nil {
		t.Errorf("validateName(bare) = %v, want nil", err)
	}
	if err := c.validateName("app.example.com"); err != nil {
		t.Errorf("validateName(in-zone) = %v, want nil", err)
	}
	if err := c.validateName("example.com"); err != nil {
		t.Errorf("validateName(apex) = %v, want nil", err)
	}
	// A client with no CLOUDFLARE_DOMAIN can't validate — must stay permissive.
	nod := newCloudflareClient("tok", "zoneABC", "")
	if err := nod.validateName("app.other.com"); err != nil {
		t.Errorf("validateName with empty domain = %v, want nil", err)
	}
}

// ---- Zone auto-discovery ----

// zoneStub serves GET /zones and returns a registry pointed at it.
func zoneStub(t *testing.T, env map[string]string, zones string) *cloudflareRegistry {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/zones" {
			http.NotFound(w, req)
			return
		}
		w.Write([]byte(`{"success":true,"result":` + zones + `}`))
	}))
	t.Cleanup(srv.Close)
	r, _ := newCloudflareRegistryFromEnv(func(k string) string { return env[k] })
	if r == nil {
		t.Fatal("registry is nil")
	}
	if r.discover != nil {
		r.discover.baseURL = srv.URL
	}
	return r
}

// The headline behaviour: a token alone is enough config, and every active
// zone in the account shows up without an env edit or a restart.
func TestCloudflareSyncDiscoversZones(t *testing.T) {
	r := zoneStub(t, map[string]string{"CLOUDFLARE_API_TOKEN": "tok"},
		`[{"id":"z1","name":"polardev.org","status":"active"},
		  {"id":"z2","name":"polardemo.com","status":"active"}]`)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := r.Domains()
	if len(got) != 2 {
		t.Fatalf("domains = %v", got)
	}
	// With no CLOUDFLARE_ZONE_ID to pin one, the default must still resolve.
	if r.DefaultDomain() == "" {
		t.Fatal("default domain is empty after discovery")
	}
	if c, ok := r.Lookup("polardemo.com"); !ok || c.zoneID != "z2" {
		t.Fatalf("lookup polardemo.com = %v ok=%v", c, ok)
	}
}

// A zone that isn't serving DNS yet would accept API calls but do nothing
// useful, so it must not appear as an editable zone.
func TestCloudflareSyncSkipsInactiveZones(t *testing.T) {
	r := zoneStub(t, map[string]string{"CLOUDFLARE_API_TOKEN": "tok"},
		`[{"id":"z1","name":"live.org","status":"active"},
		  {"id":"z2","name":"pending.org","status":"pending"},
		  {"id":"z3","name":"moved.org","status":"moved"}]`)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := r.Domains(); len(got) != 1 || got[0] != "live.org" {
		t.Fatalf("domains = %v, want [live.org]", got)
	}
}

// Env zones carry an explicit per-zone token that a listing can't infer, so
// discovery must neither overwrite nor drop them.
func TestCloudflareSyncPreservesEnvZones(t *testing.T) {
	r := zoneStub(t, map[string]string{
		"CLOUDFLARE_API_TOKEN": "tok",
		"CLOUDFLARE_ZONES":     "pinned.org:envzone:othertoken",
	}, `[{"id":"discovered","name":"pinned.org","status":"active"}]`)

	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	c, ok := r.Lookup("pinned.org")
	if !ok {
		t.Fatal("env zone disappeared")
	}
	if c.zoneID != "envzone" || c.token != "othertoken" {
		t.Fatalf("env zone overwritten: zoneID=%q token=%q", c.zoneID, c.token)
	}

	// And it survives a pass where the account no longer lists it at all.
	r.discover.baseURL = emptyZoneServer(t)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if _, ok := r.Lookup("pinned.org"); !ok {
		t.Fatal("env zone removed by reconciliation")
	}
}

func emptyZoneServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"success":true,"result":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A zone removed from the account has to leave the registry, or the DNS tab
// keeps a dead chip that the browser's saved zone preference can pin to.
func TestCloudflareSyncRemovesVanishedZones(t *testing.T) {
	r := zoneStub(t, map[string]string{"CLOUDFLARE_API_TOKEN": "tok"},
		`[{"id":"z1","name":"a.org","status":"active"},{"id":"z2","name":"b.org","status":"active"}]`)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(r.Domains()) != 2 {
		t.Fatalf("setup: domains = %v", r.Domains())
	}

	r.discover.baseURL = emptyZoneServer(t)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if got := r.Domains(); len(got) != 0 {
		t.Fatalf("domains = %v, want empty", got)
	}
	// Default must not keep naming a deleted entry.
	if c := r.Default(); c != nil {
		t.Fatalf("default = %v, want nil", c)
	}
}

// Sync must be idempotent — repeated passes can't accumulate duplicates.
func TestCloudflareSyncIdempotent(t *testing.T) {
	r := zoneStub(t, map[string]string{"CLOUDFLARE_API_TOKEN": "tok"},
		`[{"id":"z1","name":"a.org","status":"active"}]`)
	for i := 0; i < 3; i++ {
		if err := r.Sync(context.Background()); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if got := r.Domains(); len(got) != 1 {
		t.Fatalf("domains = %v after 3 syncs", got)
	}
}

// A token without the account-level Zone:Read scope must degrade to "env zones
// only", not take the integration down.
func TestCloudflareSyncErrorKeepsEnvZones(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"success":false}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	r, _ := newCloudflareRegistryFromEnv(func(k string) string {
		return map[string]string{
			"CLOUDFLARE_API_TOKEN": "tok",
			"CLOUDFLARE_ZONE_ID":   "z0",
			"CLOUDFLARE_DOMAIN":    "polardev.org",
		}[k]
	})
	r.discover.baseURL = srv.URL
	if err := r.Sync(context.Background()); err == nil {
		t.Fatal("want error from forbidden zone listing")
	}
	if got := r.Domains(); len(got) != 1 || got[0] != "polardev.org" {
		t.Fatalf("domains = %v, want [polardev.org]", got)
	}
}

// No token means nothing to discover with, and no zone vars means nothing
// configured — the integration stays off rather than half-on.
func TestCloudflareRegistryNilWithoutToken(t *testing.T) {
	r, _ := newCloudflareRegistryFromEnv(func(string) string { return "" })
	if r != nil {
		t.Fatalf("want nil registry, got %v", r.Domains())
	}
	// Nil must be safe for the background loop and Sync alike.
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("Sync on nil registry: %v", err)
	}
}

// Status feeds the browser. It may gain a source field but must never grow a
// zone id or token.
func TestCloudflareStatusSource(t *testing.T) {
	r := zoneStub(t, map[string]string{
		"CLOUDFLARE_API_TOKEN": "tok",
		"CLOUDFLARE_ZONE_ID":   "envzone",
		"CLOUDFLARE_DOMAIN":    "pinned.org",
	}, `[{"id":"secretzoneid","name":"found.org","status":"active"}]`)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	blob, err := json.Marshal(r.Status())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "secretzoneid") || strings.Contains(string(blob), "envzone") ||
		strings.Contains(string(blob), "tok\"") {
		t.Fatalf("status leaks zone id or token: %s", blob)
	}
	var got []map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, z := range got {
		src[z["domain"].(string)] = z["source"].(string)
	}
	if src["pinned.org"] != "env" || src["found.org"] != "cloudflare" {
		t.Fatalf("sources = %v", src)
	}
}

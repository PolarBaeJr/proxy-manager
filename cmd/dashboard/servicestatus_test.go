package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func accessStub(t *testing.T, count int, backends []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			Backend string `json:"backend"`
		}
		entries := make([]entry, len(backends))
		for i, b := range backends {
			entries[i] = entry{Backend: b}
		}
		json.NewEncoder(w).Encode(map[string]any{"count": count, "entries": entries})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceRatesNoProxyURL(t *testing.T) {
	svcs := []Service{{Name: "app", Backends: []string{"http://1.2.3.4:80"}}}
	out := serviceRates(context.Background(), "", svcs)
	got, ok := out["app"]
	if !ok || got.Requests5m != nil {
		t.Fatalf("Requests5m = %v, want nil when proxyURL is empty", got.Requests5m)
	}
}

func TestServiceRatesUnroutedServiceNil(t *testing.T) {
	srv := accessStub(t, 3, []string{"http://1.2.3.4:80", "http://1.2.3.4:80", "http://1.2.3.4:80"})
	svcs := []Service{{Name: "db"}} // no Backends → unrouted
	out := serviceRates(context.Background(), srv.URL, svcs)
	got := out["db"]
	if got.Requests5m != nil {
		t.Fatalf("Requests5m = %v, want nil for an unrouted service", got.Requests5m)
	}
}

func TestServiceRatesBucketing(t *testing.T) {
	srv := accessStub(t, 5, []string{
		"http://10.0.0.1:80", "http://10.0.0.1:80", "http://10.0.0.2:80",
		"http://10.0.0.3:80", "http://10.0.0.3:80",
	})
	svcs := []Service{
		{Name: "web", Backends: []string{"http://10.0.0.1:80", "http://10.0.0.2:80"}},
		{Name: "other", Backends: []string{"http://10.0.0.3:80"}},
	}
	out := serviceRates(context.Background(), srv.URL, svcs)
	web := out["web"]
	if web.Requests5m == nil || *web.Requests5m != 3 {
		t.Fatalf("web Requests5m = %v, want 3", web.Requests5m)
	}
	other := out["other"]
	if other.Requests5m == nil || *other.Requests5m != 2 {
		t.Fatalf("other Requests5m = %v, want 2", other.Requests5m)
	}
	if web.Truncated || other.Truncated {
		t.Fatalf("Truncated = true, want false (count %d < limit %d)", 5, accessSnapshotLimit)
	}
}

func TestServiceRatesTruncated(t *testing.T) {
	backends := make([]string, accessSnapshotLimit)
	for i := range backends {
		backends[i] = "http://10.0.0.1:80"
	}
	srv := accessStub(t, accessSnapshotLimit, backends)
	svcs := []Service{{Name: "web", Backends: []string{"http://10.0.0.1:80"}}}
	out := serviceRates(context.Background(), srv.URL, svcs)
	web := out["web"]
	if !web.Truncated {
		t.Fatal("Truncated = false, want true when entry count hits the ring limit")
	}
	if web.Requests5m == nil || *web.Requests5m != accessSnapshotLimit {
		t.Fatalf("web Requests5m = %v, want %d", web.Requests5m, accessSnapshotLimit)
	}
}

// TestBuildServiceStatusSumsMemberStats verifies point 5a: CPU/mem for a
// multi-container service (e.g. a 10-container Supabase-style stack sharing
// one proxy.service label) is SUMMED across every member, not just read off
// one arbitrary container — except mem_limit_bytes, which takes the max
// (summing per-container "no limit set" host-RAM reports would wildly
// overstate the denominator).
func TestBuildServiceStatusSumsMemberStats(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{
			{ID: "c1", Names: []string{"/goproxy-db-1"}, State: "running", Labels: map[string]string{labelService: "db"}},
			{ID: "c2", Names: []string{"/goproxy-db-2"}, State: "running", Labels: map[string]string{labelService: "db"}},
		})
	}))

	dockerStatsMu.Lock()
	prevByID, prevAt := dockerStatsByID, dockerStatsSampledAt
	dockerStatsByID = map[string]containerStats{
		"c1": {CPUPercent: 1.5, MemUsedBytes: 100, MemLimitBytes: 500},
		"c2": {CPUPercent: 2.5, MemUsedBytes: 200, MemLimitBytes: 900},
	}
	dockerStatsMu.Unlock()
	t.Cleanup(func() {
		dockerStatsMu.Lock()
		dockerStatsByID, dockerStatsSampledAt = prevByID, prevAt
		dockerStatsMu.Unlock()
	})

	resp, err := buildServiceStatus(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("buildServiceStatus: %v", err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Services) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	db := resp.Groups[0].Services[0]
	if db.CPUPercent != 4.0 {
		t.Errorf("CPUPercent = %v, want 4.0 (1.5+2.5 summed across members)", db.CPUPercent)
	}
	if db.MemUsedBytes != 300 {
		t.Errorf("MemUsedBytes = %v, want 300 (100+200 summed across members)", db.MemUsedBytes)
	}
	if db.MemLimitBytes != 900 {
		t.Errorf("MemLimitBytes = %v, want 900 (max, not summed)", db.MemLimitBytes)
	}
}

func TestServiceStatusEndpoint(t *testing.T) {
	// proxyURLFromEnv() reads PROXY_URL directly; pin it empty so this test
	// doesn't make a real outbound call (with a 3s timeout) if the ambient
	// environment happens to have it set.
	t.Setenv("PROXY_URL", "")
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
		}})
	}))

	// Not set up yet → 503, regardless of auth header.
	fresh, err := loadAuthStore(t.TempDir() + "/auth.json")
	if err != nil {
		t.Fatalf("loadAuthStore: %v", err)
	}
	freshMux := newDashboardMux(dc, nil, fresh, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	freshMux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/service-status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-set-up: status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), nil, "", nil, nil, nil, nil, nil, nil, nil, nil)

	// No credentials → 401.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/service-status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Valid token → 200 with the expected shape.
	tok, _, err := auth.CreateToken("alice", "test")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/service-status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got ServiceStatusResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WindowSeconds != int(serviceStatusWindow.Seconds()) {
		t.Errorf("WindowSeconds = %d, want %d", got.WindowSeconds, int(serviceStatusWindow.Seconds()))
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Services) != 1 || got.Groups[0].Services[0].Name != "app" {
		t.Fatalf("Groups = %+v", got.Groups)
	}
}

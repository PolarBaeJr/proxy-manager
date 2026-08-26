package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func withSelfHostname(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := selfHostname
	selfHostname = fn
	t.Cleanup(func() { selfHostname = prev })
}

func TestIsSelfContainer(t *testing.T) {
	cases := []struct {
		name     string
		hostname func() (string, error)
		ct       dockerContainer
		want     bool
	}{
		{
			name:     "exact match",
			hostname: func() (string, error) { return "abc123def456", nil },
			ct:       dockerContainer{ID: "abc123def456"},
			want:     true,
		},
		{
			name:     "hostname is a valid prefix of a longer container ID",
			hostname: func() (string, error) { return "abc123def456", nil },
			ct:       dockerContainer{ID: "abc123def456789012345678901234567890123456789012345678901234"},
			want:     true,
		},
		{
			name:     "no match",
			hostname: func() (string, error) { return "abc123def456", nil },
			ct:       dockerContainer{ID: "zzz999yyy888"},
			want:     false,
		},
		{
			name:     "empty hostname",
			hostname: func() (string, error) { return "", nil },
			ct:       dockerContainer{ID: "abc123def456"},
			want:     false,
		},
		{
			name:     "hostname error",
			hostname: func() (string, error) { return "", errors.New("boom") },
			ct:       dockerContainer{ID: "abc123def456"},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSelfHostname(t, tc.hostname)
			if got := isSelfContainer(tc.ct); got != tc.want {
				t.Errorf("isSelfContainer() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServiceContainsSelf(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	self := Service{Members: []dockerContainer{
		{ID: "other"},
		{ID: "abc123def456"},
	}}
	if !serviceContainsSelf(self) {
		t.Error("serviceContainsSelf() = false, want true (one member matches self)")
	}

	other := Service{Members: []dockerContainer{
		{ID: "one"},
		{ID: "two"},
	}}
	if serviceContainsSelf(other) {
		t.Error("serviceContainsSelf() = true, want false (no member matches self)")
	}
}

func TestExcludeSelfDropsOnlyRealSelf(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	svcs := []Service{
		{Name: "dashboard-real", Members: []dockerContainer{{ID: "abc123def456"}}},
		{Name: "other", Members: []dockerContainer{{ID: "unrelated"}}},
		// Same-looking name/labels as the real dashboard, but a different
		// container ID — must NOT be dropped. Proves the filter is
		// identity-based, not name-based.
		{Name: "dashboard", Labels: map[string]string{labelService: "dashboard"}, Members: []dockerContainer{{ID: "not-actually-self"}}},
	}

	out := excludeSelf(svcs)
	if len(out) != 2 {
		t.Fatalf("excludeSelf() dropped %d services, want 1 dropped (2 remaining), got %d remaining", 3-len(out), len(out))
	}
	for _, s := range out {
		if s.Name == "dashboard-real" {
			t.Errorf("excludeSelf() kept %q, want it dropped", s.Name)
		}
	}
	if pickService(out, "dashboard") == nil {
		t.Error("excludeSelf() dropped the name-alike \"dashboard\" service, want it kept (identity-based, not name-based)")
	}
	if pickService(out, "other") == nil {
		t.Error("excludeSelf() dropped the unrelated service, want it kept")
	}
}

func TestServiceContainsSelfByName(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"Id":"abc123def456","Names":["/dashboard"]}]`))
	}))

	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })
	self, err := dc.serviceContainsSelfByName(context.Background(), "dashboard")
	if err != nil {
		t.Fatalf("serviceContainsSelfByName: %v", err)
	}
	if !self {
		t.Error("serviceContainsSelfByName() = false, want true")
	}

	withSelfHostname(t, func() (string, error) { return "someone-else", nil })
	self, err = dc.serviceContainsSelfByName(context.Background(), "dashboard")
	if err != nil {
		t.Fatalf("serviceContainsSelfByName: %v", err)
	}
	if self {
		t.Error("serviceContainsSelfByName() = true, want false")
	}
}

// TestBuildManagedServicesExcludesSelf proves buildManagedServices (api.go) —
// the shared assembly behind both /api/services and peers.go's
// peerServicesHandler — still excludes the dashboard's own service, same as
// the inline logic it replaced.
func TestBuildManagedServicesExcludesSelf(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"Id":"abc123def456","Names":["/dashboard"],"State":"running","Labels":{"proxy.service":"dashboard","proxy.host":"dashboard.example","proxy.port":"8093"}},
			{"Id":"other","Names":["/app"],"State":"running","Labels":{"proxy.service":"app","proxy.host":"app.example","proxy.port":"80"}}
		]`))
	}))
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)

	svcs, err := buildManagedServices(context.Background(), dc, onb, ic, nil)
	if err != nil {
		t.Fatalf("buildManagedServices: %v", err)
	}
	if pickService(svcs, "dashboard") != nil {
		t.Error("buildManagedServices included the dashboard's own service, want it excluded")
	}
	if pickService(svcs, "app") == nil {
		t.Error("buildManagedServices dropped the unrelated service, want it kept")
	}
}

// TestServicesStopRefusesSelf proves POST /api/services/{name}/stop returns
// 403 and never calls through to dc.stopContainer when the named service is
// the dashboard's own container — asserted via the docker stub's captured
// requests, not by mocking.
func TestServicesStopRefusesSelf(t *testing.T) {
	var stopCalled bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stop") {
			stopCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"Id":"abc123def456","Names":["/dashboard"],"State":"running","Labels":{"proxy.service":"dashboard","proxy.host":"dashboard.example","proxy.port":"8093"}}]`))
	}))

	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/api/services/dashboard/stop", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if stopCalled {
		t.Error("dc.stopContainer's underlying /stop endpoint was called — guard did not short-circuit")
	}
}

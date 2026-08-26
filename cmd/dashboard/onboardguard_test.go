// Tests for checkOnboardTarget — the guard that stops onboardContainer /
// onboardManagedOnly from relabeling-and-recreating (and, for
// onboardContainer, ultimately stopping+removing) the dashboard's own
// container or one of the fixed infra containers. Reuses
// newOnboardFakeDockerServer (onboard_container_test.go) and
// withSelfHostname (selfidentity_test.go) — both in this package.

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

// TestOnboardContainerRefusesSelf proves a target that IS the dashboard's own
// container by identity (container ID matches selfHostname — no
// proxy.service label, since onboard targets are never labeled yet) is
// refused, and that NO create/remove Docker calls happened: the original
// container is left exactly as it was, and no goproxy-<name>-N replicas
// exist.
func TestOnboardContainerRefusesSelf(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "abc123def456", name: "myapp", image: "ghcr.io/org/myapp:v1",
		env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	})

	err := dc.onboardContainer(context.Background(), "myapp", OnboardRequest{
		Host: "myapp.example.org", Port: 8080,
	})
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !errors.Is(err, errOnboardRefused) {
		t.Fatalf("err = %v, want it to wrap errOnboardRefused", err)
	}

	all, lerr := dc.listAll(context.Background(), "")
	if lerr != nil {
		t.Fatalf("listAll: %v", lerr)
	}
	if len(all) != 1 {
		t.Fatalf("containers = %d, want exactly the untouched original: %+v", len(all), all)
	}
	if all[0].ID != "abc123def456" || all[0].State != "running" {
		t.Fatalf("original container mutated: %+v", all[0])
	}
}

// TestOnboardContainerRefusesInfraName proves a container named "proxy" (one
// of the fixed infra names) is refused EVEN WHEN it is demonstrably NOT self
// by identity — proving the infra-name check is independent of, and checked
// ahead of, the identity check.
func TestOnboardContainerRefusesInfraName(t *testing.T) {
	if !infraContainerNames["proxy"] {
		t.Fatal("test assumes \"proxy\" is in infraContainerNames")
	}
	// Deliberately NOT self: hostname matches nothing about this container.
	withSelfHostname(t, func() (string, error) { return "totally-unrelated-hostname", nil })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "proxy-id", name: "proxy", image: "ghcr.io/org/proxy:v1",
		env: []string{}, state: "running", labels: map[string]string{},
	})

	err := dc.onboardContainer(context.Background(), "proxy", OnboardRequest{
		Host: "proxy.example.org", Port: 8080,
	})
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !errors.Is(err, errOnboardRefused) {
		t.Fatalf("err = %v, want it to wrap errOnboardRefused", err)
	}

	all, lerr := dc.listAll(context.Background(), "")
	if lerr != nil {
		t.Fatalf("listAll: %v", lerr)
	}
	if len(all) != 1 || all[0].ID != "proxy-id" || all[0].State != "running" {
		t.Fatalf("proxy container mutated: %+v", all)
	}
}

// TestOnboardContainerRefusesOnHostnameLookupError is the fail-closed proof:
// a selfHostname() error during the check must cause a refusal, not a silent
// pass-through that lets an ordinary-looking, non-infra container get
// relabeled-and-recreated (and the original stopped+removed) while identity
// can't be verified.
func TestOnboardContainerRefusesOnHostnameLookupError(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "", errors.New("boom: no /etc/hostname") })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1",
		env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	})

	err := dc.onboardContainer(context.Background(), "myapp", OnboardRequest{
		Host: "myapp.example.org", Port: 8080,
	})
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !errors.Is(err, errOnboardRefused) {
		t.Fatalf("err = %v, want it to wrap errOnboardRefused (fail closed on lookup error)", err)
	}

	all, lerr := dc.listAll(context.Background(), "")
	if lerr != nil {
		t.Fatalf("listAll: %v", lerr)
	}
	if len(all) != 1 || all[0].ID != "orig-id" || all[0].State != "running" {
		t.Fatalf("original container mutated despite fail-closed refusal: %+v", all)
	}
}

// TestOnboardContainerLegitimateStillSucceeds is the regression test proving
// the new guard doesn't over-block: an ordinary unmanaged container, not
// self and not an infra name, onboards exactly as before.
func TestOnboardContainerLegitimateStillSucceeds(t *testing.T) {
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1",
		env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	})

	err := dc.onboardContainer(context.Background(), "myapp", OnboardRequest{
		Host: "myapp.example.org", Port: 8080, Replicas: 2,
	})
	if err != nil {
		t.Fatalf("onboardContainer: %v", err)
	}

	all, lerr := dc.listAll(context.Background(), "")
	if lerr != nil {
		t.Fatalf("listAll: %v", lerr)
	}
	var live []dockerContainer
	for _, c := range all {
		if c.Labels[labelService] == "myapp" {
			live = append(live, c)
		}
	}
	if len(live) != 2 {
		t.Fatalf("live label-managed replicas = %d, want 2: %+v", len(live), live)
	}
	for _, c := range all {
		if c.name() == "myapp" && c.ID == "orig-id" {
			t.Fatalf("original container %q still present after onboarding", c.name())
		}
	}
}

// TestOnboardManagedOnlyRefusesSelf covers checkOnboardTarget's
// defense-in-depth call in onboardManagedOnly (the batch-onboard path): a
// target that IS the dashboard's own container is refused, and the store
// gets no new record.
func TestOnboardManagedOnlyRefusesSelf(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "abc123def456", name: "dashboard", image: "ghcr.io/org/dashboard:v1",
		state: "running", labels: map[string]string{},
	})
	store, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}

	err = onboardManagedOnly(context.Background(), "dashboard", dc, store)
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !errors.Is(err, errOnboardRefused) {
		t.Fatalf("err = %v, want it to wrap errOnboardRefused", err)
	}
	if _, ok := store.Get("dashboard"); ok {
		t.Fatal("onboardManagedOnly stored a record despite refusal")
	}
}

// TestOnboardManagedOnlyRefusesOnHostnameLookupError is the fail-closed proof
// for onboardManagedOnly's guard call: a selfHostname() error must refuse,
// not silently adopt the container.
func TestOnboardManagedOnlyRefusesOnHostnameLookupError(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "", errors.New("boom") })

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1",
		state: "running", labels: map[string]string{},
	})
	store, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}

	err = onboardManagedOnly(context.Background(), "myapp", dc, store)
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !errors.Is(err, errOnboardRefused) {
		t.Fatalf("err = %v, want it to wrap errOnboardRefused (fail closed on lookup error)", err)
	}
	if _, ok := store.Get("myapp"); ok {
		t.Fatal("onboardManagedOnly stored a record despite fail-closed refusal")
	}
}

// TestWriteOnboardErr proves the HTTP mapping: errOnboardRefused (and errors
// wrapping it) -> 403, everything else -> 400 (the handler's pre-existing
// behavior for its other error cases).
func TestWriteOnboardErr(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOnboardErr(rec, errOnboardRefused)
	if rec.Code != 403 {
		t.Fatalf("status for errOnboardRefused = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	writeOnboardErr(rec, errors.New("container not found"))
	if rec.Code != 400 {
		t.Fatalf("status for an unrelated error = %d, want 400", rec.Code)
	}
}

// TestOnboardEndpointRefusesSelf is the full round trip: POST
// /api/discovery/dashboard/onboard against the real mux must return 403 and
// never reach a create/start/stop/remove call, when the named container is
// the dashboard's own by identity. Mirrors selfidentity_test.go's
// TestServicesStopRefusesSelf pattern (internalToken bearer grants the
// elevated auth this route requires).
func TestOnboardEndpointRefusesSelf(t *testing.T) {
	var mutated bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			mutated = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"Id":"abc123def456","Names":["/dashboard"],"State":"running","Labels":{}}]`))
	}))

	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	body := `{"host":"dashboard.example.org","port":8093}`
	req := httptest.NewRequest("POST", "/api/discovery/dashboard/onboard", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if mutated {
		t.Error("a POST reached the docker daemon stub — guard did not short-circuit before any mutating call")
	}
}

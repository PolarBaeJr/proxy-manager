package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// weightLabelStub is the shared fixture for the setter tests: one running,
// label-managed "app" replica carrying whatever proxy.weight the caller
// wants, recording the labels of the container the setter recreates.
func weightLabelStub(t *testing.T, startWeight string, out *map[string]string, sawCreate *bool) *dockerClient {
	t.Helper()
	labels := map[string]string{
		labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
	}
	if startWeight != "" {
		labels[labelWeight] = startWeight
	}
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image: "ghcr.io/org/app:v1", Labels: labels,
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			*sawCreate = true
			var body createBody
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if *out == nil {
				*out = body.Labels
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })
	return dc
}

// TestSetWeightLabelSetsLabel mirrors TestSetUnscalableLabelEnablesLabel:
// the recreate patches proxy.weight and carries every other proxy.* label
// through untouched.
func TestSetWeightLabelSetsLabel(t *testing.T) {
	var createdLabels map[string]string
	var sawCreate bool
	dc := weightLabelStub(t, "", &createdLabels, &sawCreate)

	if err := dc.setWeightLabel(context.Background(), "app", 3); err != nil {
		t.Fatalf("setWeightLabel: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container created")
	}
	if createdLabels[labelWeight] != "3" {
		t.Errorf("labelWeight = %q, want 3", createdLabels[labelWeight])
	}
	if createdLabels[labelHost] != "app.example" || createdLabels[labelService] != "app" {
		t.Errorf("proxy.* labels lost: %v", createdLabels)
	}
}

// TestSetWeightLabelResetRemovesLabel: 1 is the proxy's own default, so a
// reset must drop the label rather than write "1" — otherwise a service reset
// to default is no longer indistinguishable from one that never had a weight.
func TestSetWeightLabelResetRemovesLabel(t *testing.T) {
	var createdLabels map[string]string
	var sawCreate bool
	dc := weightLabelStub(t, "5", &createdLabels, &sawCreate)

	if err := dc.setWeightLabel(context.Background(), "app", 1); err != nil {
		t.Fatalf("setWeightLabel: %v", err)
	}
	if v, ok := createdLabels[labelWeight]; ok {
		t.Errorf("labelWeight = %q, want removed entirely", v)
	}
}

// TestServicesWeightRejectsOutOfRange keeps a fat-fingered value from ever
// reaching Docker. Weight is a RATIO, so 0 would black-hole this host's share
// and an absurd value would black-hole the other one.
func TestServicesWeightRejectsOutOfRange(t *testing.T) {
	for _, w := range []int{0, -2, maxServiceWeight + 1} {
		var createdLabels map[string]string
		var sawCreate bool
		dc := weightLabelStub(t, "", &createdLabels, &sawCreate)
		mux := newLocalTestMux(t, dc, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/services/app/weight",
			strings.NewReader(`{"weight":`+strconv.Itoa(w)+`}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("weight %d: status = %d, want 400", w, rec.Code)
		}
		if sawCreate {
			t.Errorf("weight %d: recreated containers on a rejected value", w)
		}
	}
}

// TestServicesWeightForwardsToOwningPeer is the write-mesh requirement, in the
// house style of TestServicesDuplicateForwardsToOwningPeer: setting the weight
// on a FOREIGN row must relay to the owning peer's own
// /peer/services/{name}/weight and never touch the local daemon. This is the
// exact wiring PR #103 shipped without.
func TestServicesWeightForwardsToOwningPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var createdLabels map[string]string
	var sawCreate bool
	ownerDC := weightLabelStub(t, "", &createdLabels, &sawCreate)
	ownerOnb := newTestOnboardedStore(t)
	ownerReg := newPeerRegistry(nil, "s3cret", "dashboard-b", "dev", 0, nil)
	ownerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", ownerDC, ownerOnb,
		newImageChecker(ownerDC), ownerReg, filepath.Join(t.TempDir(), "routes.json"), noopProxyStub(t), true))
	t.Cleanup(ownerSrv.Close)

	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, newTestPeerRegistry(ownerSrv.URL, true))

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/weight?host=dashboard-b",
		strings.NewReader(`{"weight":4}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — a foreign service's weight must never touch the local daemon")
	}
	if createdLabels[labelWeight] != "4" {
		t.Errorf("owner recreated with labelWeight = %q, want 4 — the hop never landed", createdLabels[labelWeight])
	}
}

// TestUIWeightCapMatchesServer: the browser's max= and its error message are
// a literal in the dashboard HTML, so nothing but this test stops them from
// drifting away from the bound the API actually enforces.
func TestUIWeightCapMatchesServer(t *testing.T) {
	want := "const MAX_SVC_WEIGHT = " + strconv.Itoa(maxServiceWeight) + ";"
	if !strings.Contains(dashboardHTML, want) {
		t.Errorf("dashboard HTML does not contain %q — the UI cap has drifted from maxServiceWeight", want)
	}
}

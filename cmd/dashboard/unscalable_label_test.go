package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSetUnscalableLabelEnablesLabel proves setUnscalableLabel(ctx, name,
// true) recreates a service without proxy.unscalable to add the label —
// mirrors TestSetAutoUpdateLabelFlipsOnlyTheLabel for the singleton flag.
func TestSetUnscalableLabelEnablesLabel(t *testing.T) {
	var createdLabels map[string]string
	var sawCreate bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{
					labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
				},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			var body createBody
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if createdLabels == nil {
				createdLabels = body.Labels
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	if err := dc.setUnscalableLabel(context.Background(), "app", true); err != nil {
		t.Fatalf("setUnscalableLabel: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container created")
	}
	if createdLabels[labelUnscalable] != "true" {
		t.Errorf("labelUnscalable = %q, want true", createdLabels[labelUnscalable])
	}
	if createdLabels[labelHost] != "app.example" || createdLabels[labelService] != "app" {
		t.Errorf("proxy.* labels lost: %v", createdLabels)
	}
}

// TestSetUnscalableLabelDisableRemovesLabel proves disabling drops the label
// entirely rather than setting it to "false".
func TestSetUnscalableLabelDisableRemovesLabel(t *testing.T) {
	var createdLabels map[string]string
	var sawCreate bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{
					labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
					labelUnscalable: "true",
				},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			var body createBody
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if createdLabels == nil {
				createdLabels = body.Labels
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	if err := dc.setUnscalableLabel(context.Background(), "app", false); err != nil {
		t.Fatalf("setUnscalableLabel: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container created")
	}
	if v, ok := createdLabels[labelUnscalable]; ok {
		t.Errorf("labelUnscalable = %q, want removed entirely", v)
	}
}

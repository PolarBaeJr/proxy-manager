package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// End-to-end over replaceService rather than mergeEnv alone: the merge is only
// useful if the value actually reaches createContainer. This is the exact
// journey the dashboard's "Env edits" box takes — add one variable to a running
// service and every other one must survive.
//
// Guards the original bug directly: before the merge, passing a single
// KEY=VALUE replaced the whole environment.
func TestReplaceServiceAddingOneEnvKeepsTheRest(t *testing.T) {
	current := []string{
		"PATH=/usr/local/bin:/usr/bin", // baked into the image
		"DATABASE_URL=postgres://db:5432/app",
		"SECRET_KEY=hunter2",
		"PORT=8080",
	}
	created := captureReplace(t, current, ReplaceServiceRequest{
		Image: "ghcr.io/org/app:v2",
		Env:   map[string]string{"FEATURE_FLAG": "on"},
	})

	got := map[string]bool{}
	for _, e := range created {
		got[e] = true
	}
	for _, want := range current {
		if !got[want] {
			t.Errorf("lost %q — the whole point is that it survives", want)
		}
	}
	if !got["FEATURE_FLAG=on"] {
		t.Errorf("new var missing; env = %v", created)
	}
	if len(created) != len(current)+1 {
		t.Errorf("env has %d entries, want %d: %v", len(created), len(current)+1, created)
	}
}

// Editing an existing key without acknowledging it must not reach Docker at
// all — the user has to be asked first.
func TestReplaceServiceConflictCreatesNothing(t *testing.T) {
	created := captureReplace(t, []string{"PORT=8080"}, ReplaceServiceRequest{
		Image: "ghcr.io/org/app:v2",
		Env:   map[string]string{"PORT": "3000"},
	})
	if created != nil {
		t.Fatalf("containers were created despite an unresolved conflict: %v", created)
	}
}

// ...and once acknowledged it overwrites in place rather than appending a
// second PORT= entry.
func TestReplaceServiceAckedConflictOverwrites(t *testing.T) {
	created := captureReplace(t, []string{"PORT=8080", "KEEP=1"}, ReplaceServiceRequest{
		Image:  "ghcr.io/org/app:v2",
		Env:    map[string]string{"PORT": "3000"},
		EnvAck: []string{"PORT"},
	})
	joined := strings.Join(created, ",")
	if !strings.Contains(joined, "PORT=3000") || strings.Contains(joined, "PORT=8080") {
		t.Errorf("PORT not overwritten: %v", created)
	}
	if !strings.Contains(joined, "KEEP=1") {
		t.Errorf("unrelated var lost: %v", created)
	}
	if strings.Count(joined, "PORT=") != 1 {
		t.Errorf("duplicate PORT entries: %v", created)
	}
}

// captureReplace runs replaceService against a stub daemon and returns the Env
// handed to the FIRST container create, or nil if none happened.
func captureReplace(t *testing.T, currentEnv []string, req ReplaceServiceRequest) []string {
	t.Helper()
	var createdEnv []string
	var sawCreate bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": currentEnv}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			var body struct {
				Env []string `json:"Env"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			if createdEnv == nil {
				createdEnv = body.Env
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			// start / stop / remove / pull all just need to succeed.
			w.Write([]byte("{}"))
		}
	}))

	// replaceService settles for 5s between create and teardown; shrink it so
	// the suite stays fast. Restored by t.Cleanup.
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	err := dc.replaceService(context.Background(), "app", req)
	var ce *envConflictError
	if err != nil && !asEnvConflict(err, &ce) {
		t.Fatalf("replaceService: %v", err)
	}
	if !sawCreate {
		return nil
	}
	return createdEnv
}

func asEnvConflict(err error, target **envConflictError) bool {
	ce, ok := err.(*envConflictError)
	if ok {
		*target = ce
	}
	return ok
}

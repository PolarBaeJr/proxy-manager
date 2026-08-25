package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestImageCheckerBareDigestErrors exercises imageChecker.Check directly
// against a bare-digest image reference: /images/{name}/json succeeds with
// no RepoDigests (a bare digest never has one locally), and
// /distribution/{name}/json 404s (the daemon can't resolve a bare digest
// against a registry the way it can a repo:tag). The checker must record an
// error rather than silently reporting no update available.
func TestImageCheckerBareDigestErrors(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/images/"):
			json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{}})
		case strings.Contains(r.URL.Path, "/distribution/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Write([]byte("{}"))
		}
	}))
	ic := newImageChecker(dc)
	ic.Check(context.Background(), image)

	st := ic.Get(image)
	if st == nil {
		t.Fatal("Get returned nil after Check")
	}
	if st.Err == "" {
		t.Fatal("Err = \"\", want a non-empty error for a bare-digest image")
	}
	if st.UpdateAvailable {
		t.Fatal("UpdateAvailable = true, want false when the check errored")
	}
}

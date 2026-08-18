package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeServiceSecrets writes one service's secrets file under a fresh temp
// directory and returns a store rooted at that directory. Call it once per
// service to build a multi-service fixture (each call reuses the SAME dir if
// given the same *testing.T — see writeServiceSecretsIn for that case).
func writeServiceSecrets(t *testing.T, service, body string) *secretsStore {
	t.Helper()
	dir := t.TempDir()
	return writeServiceSecretsIn(t, dir, service, body)
}

func writeServiceSecretsIn(t *testing.T, dir, service, body string) *secretsStore {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, service+".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &secretsStore{dir: dir}
}

func TestSecretsLookup(t *testing.T) {
	sec := writeServiceSecrets(t, "app", "FOO=bar\n# a comment\n\nWEBHOOK_SECRET=s3cr3t\n")

	v, err := sec.lookup("app", "WEBHOOK_SECRET")
	if err != nil || v != "s3cr3t" {
		t.Fatalf("lookup(app, WEBHOOK_SECRET) = %q, %v", v, err)
	}
	if _, err := sec.lookup("app", "MISSING"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

// TestSecretsLookupScopedPerService is the reason this is a directory of
// per-service files rather than one shared file: two services can use the
// exact same key NAME and still get their own independent value, and a
// service with no file of its own gets a clean "not configured for you"
// error rather than accidentally reading another service's secret.
func TestSecretsLookupScopedPerService(t *testing.T) {
	dir := t.TempDir()
	writeServiceSecretsIn(t, dir, "app-a", "API_KEY=a-value\n")
	sec := writeServiceSecretsIn(t, dir, "app-b", "API_KEY=b-value\n")

	va, err := sec.lookup("app-a", "API_KEY")
	if err != nil || va != "a-value" {
		t.Fatalf("lookup(app-a, API_KEY) = %q, %v", va, err)
	}
	vb, err := sec.lookup("app-b", "API_KEY")
	if err != nil || vb != "b-value" {
		t.Fatalf("lookup(app-b, API_KEY) = %q, %v", vb, err)
	}

	// A third service with no secrets file of its own must not fall back to
	// anyone else's — it should fail cleanly, not silently resolve.
	if _, err := sec.lookup("app-c", "API_KEY"); err == nil {
		t.Fatal("app-c has no secrets file — expected an error, not a cross-service fallback")
	}
}

// TestSecretsLookupDuplicateKeyLastWins is the discriminating case for a
// realistic edit pattern — rotating a secret by appending a new line below
// the old one instead of editing in place. lookup must match mergeEnv's
// documented "Docker keeps the LAST occurrence" convention, or a rotated
// secret would silently keep resolving to the stale value.
func TestSecretsLookupDuplicateKeyLastWins(t *testing.T) {
	sec := writeServiceSecrets(t, "app", "API_KEY=old-value\nOTHER=x\nAPI_KEY=new-value\n")

	v, err := sec.lookup("app", "API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if v != "new-value" {
		t.Errorf("lookup(API_KEY) = %q, want the LAST occurrence (new-value)", v)
	}
}

func TestSecretsLookupNilStore(t *testing.T) {
	var sec *secretsStore
	if _, err := sec.lookup("app", "ANY"); err == nil {
		t.Fatal("expected error from a nil store")
	}
}

// TestSecretsLookupRejectsInvalidServiceName guards servicePath, the one
// place a service name becomes a filesystem path — an invalid name (path
// separators, leading dot, etc.) must be refused rather than joined in.
func TestSecretsLookupRejectsInvalidServiceName(t *testing.T) {
	sec := &secretsStore{dir: t.TempDir()}
	for _, bad := range []string{"../etc", "a/b", "", ".hidden"} {
		if _, err := sec.lookup(bad, "ANY"); err == nil {
			t.Errorf("lookup(%q, ...) should have been rejected as an invalid service name", bad)
		}
	}
}

func TestResolveSecretRefsLiteralPassthrough(t *testing.T) {
	edits := map[string]string{"PORT": "8080"}
	resolved, refs, err := resolveSecretRefs("app", edits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["PORT"] != "8080" {
		t.Fatalf("resolved = %v", resolved)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty (no ref: values)", refs)
	}
}

func TestResolveSecretRefsResolvesRef(t *testing.T) {
	sec := writeServiceSecrets(t, "app", "API_KEY=live-value\n")
	edits := map[string]string{"API_KEY": "ref:API_KEY", "PORT": "8080"}

	resolved, refs, err := resolveSecretRefs("app", edits, sec)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["API_KEY"] != "live-value" {
		t.Fatalf("resolved[API_KEY] = %q, want live-value", resolved["API_KEY"])
	}
	if resolved["PORT"] != "8080" {
		t.Fatalf("resolved[PORT] = %q, want unchanged literal", resolved["PORT"])
	}
	if refs["API_KEY"] != "ref:API_KEY" {
		t.Fatalf("refs[API_KEY] = %q, want the original ref string", refs["API_KEY"])
	}
	if _, ok := refs["PORT"]; ok {
		t.Fatal("PORT was a literal, should not appear in refs")
	}
}

func TestResolveSecretRefsMissingSecretFails(t *testing.T) {
	sec := writeServiceSecrets(t, "app", "OTHER=x\n")
	_, _, err := resolveSecretRefs("app", map[string]string{"API_KEY": "ref:MISSING"}, sec)
	if err == nil {
		t.Fatal("expected an error for an unresolvable ref")
	}
	if !strings.Contains(err.Error(), "API_KEY") {
		t.Errorf("error %q should name the env key", err)
	}
	if strings.Contains(err.Error(), "MISSING") {
		// Not a hard requirement, but the key name itself isn't sensitive —
		// this just documents current behavior rather than asserting secrecy.
		t.Log("error includes the secret NAME (not the value) — expected, not a leak")
	}
}

// TestRedactRefConflictsHidesResolvedSecret is the discriminating case: a
// conflict on a ref-sourced key must never surface the resolved plaintext
// value in the API-facing EnvConflict.Incoming field, or a secret passed by
// reference would leak into the 409 response (and any transcript that reads
// it) the exact way ref: was built to avoid.
func TestRedactRefConflictsHidesResolvedSecret(t *testing.T) {
	err := &envConflictError{Conflicts: []EnvConflict{
		{Key: "API_KEY", Current: "old-value", Incoming: "resolved-secret-value"},
		{Key: "PORT", Current: "8080", Incoming: "9090"},
	}}
	refs := map[string]string{"API_KEY": "ref:API_KEY"}

	got := redactRefConflicts(err, refs)
	var ce *envConflictError
	if ce, _ = got.(*envConflictError); ce == nil {
		t.Fatalf("redactRefConflicts returned %T, want *envConflictError", got)
	}
	for _, c := range ce.Conflicts {
		switch c.Key {
		case "API_KEY":
			if c.Incoming != "ref:API_KEY" {
				t.Errorf("API_KEY.Incoming = %q, want the ref string, not the resolved secret", c.Incoming)
			}
		case "PORT":
			if c.Incoming != "9090" {
				t.Errorf("PORT.Incoming = %q, want unchanged (not ref-sourced)", c.Incoming)
			}
		}
	}
}

func TestRedactRefConflictsPassesThroughOtherErrors(t *testing.T) {
	orig := errFixture("some other error")
	got := redactRefConflicts(orig, map[string]string{"X": "ref:X"})
	if got != orig {
		t.Fatalf("non-conflict error should pass through unchanged, got %v", got)
	}
}

type errFixture string

func (e errFixture) Error() string { return string(e) }

// TestReplaceServiceResolvesSecretRef is the full journey: a "ref:NAME" env
// edit through replaceService must reach the created container's env as the
// resolved secret value, not the literal "ref:NAME" string — that's the
// entire point of the feature (an MCP caller passes a name, never a value).
func TestReplaceServiceResolvesSecretRef(t *testing.T) {
	sec := writeServiceSecrets(t, "app", "WEBHOOK_SECRET=sk_live_abc123\n")

	var createdEnv []string
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080"}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			var body struct {
				Env []string `json:"Env"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			createdEnv = body.Env
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))
	dc.secrets = sec

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{
		Image: "ghcr.io/org/app:v2",
		Env:   map[string]string{"WEBHOOK_SECRET": "ref:WEBHOOK_SECRET"},
	})
	if err != nil {
		t.Fatalf("replaceService: %v", err)
	}

	got := map[string]string{}
	for _, e := range createdEnv {
		if k, v, ok := splitEnvEntry(e); ok {
			got[k] = v
		}
	}
	if got["WEBHOOK_SECRET"] != "sk_live_abc123" {
		t.Errorf("WEBHOOK_SECRET = %q, want the resolved secret value", got["WEBHOOK_SECRET"])
	}
}

// TestReplaceServiceUnresolvableRefCreatesNothing proves a bad ref fails the
// whole request before any container is touched — the same all-or-nothing
// guarantee an env conflict already gets.
func TestReplaceServiceUnresolvableRefCreatesNothing(t *testing.T) {
	sec := writeServiceSecrets(t, "app", "OTHER=x\n")

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
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080"}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))
	dc.secrets = sec

	err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{
		Image: "ghcr.io/org/app:v2",
		Env:   map[string]string{"MISSING": "ref:DOES_NOT_EXIST"},
	})
	if err == nil {
		t.Fatal("expected an error for an unresolvable ref")
	}
	if sawCreate {
		t.Fatal("a container was created despite the ref failing to resolve")
	}
}

// TestReplaceServiceCrossServiceRefIsolation is the end-to-end version of
// TestSecretsLookupScopedPerService: two DIFFERENT services replaced through
// the same secretsStore, each using "ref:API_KEY", must each land their own
// value — not whichever one happens to be looked up first, and not each
// other's.
func TestReplaceServiceCrossServiceRefIsolation(t *testing.T) {
	dir := t.TempDir()
	writeServiceSecretsIn(t, dir, "app-a", "API_KEY=a-value\n")
	writeServiceSecretsIn(t, dir, "app-b", "API_KEY=b-value\n")
	sec := &secretsStore{dir: dir}

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	replaceAndCapture := func(service string) string {
		var createdEnv []string
		dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/containers/json"):
				json.NewEncoder(w).Encode([]dockerContainer{{
					ID: "tpl1", Names: []string{"/goproxy-" + service + "-1"}, State: "running",
					Image:  "ghcr.io/org/" + service + ":v1",
					Labels: map[string]string{labelEnable: "true", labelService: service, labelHost: service + ".example"},
				}})
			case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
				json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080"}}})
			case strings.Contains(r.URL.Path, "/containers/create"):
				var body struct {
					Env []string `json:"Env"`
				}
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &body)
				createdEnv = body.Env
				json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
			default:
				w.Write([]byte("{}"))
			}
		}))
		dc.secrets = sec
		if err := dc.replaceService(context.Background(), service, ReplaceServiceRequest{
			Image: "ghcr.io/org/" + service + ":v2",
			Env:   map[string]string{"API_KEY": "ref:API_KEY"},
		}); err != nil {
			t.Fatalf("replaceService(%s): %v", service, err)
		}
		for _, e := range createdEnv {
			if k, v, ok := splitEnvEntry(e); ok && k == "API_KEY" {
				return v
			}
		}
		t.Fatalf("API_KEY missing from %s's created env: %v", service, createdEnv)
		return ""
	}

	if got := replaceAndCapture("app-a"); got != "a-value" {
		t.Errorf("app-a API_KEY = %q, want a-value", got)
	}
	if got := replaceAndCapture("app-b"); got != "b-value" {
		t.Errorf("app-b API_KEY = %q, want b-value", got)
	}
}

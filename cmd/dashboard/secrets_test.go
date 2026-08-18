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

func writeSecretsFile(t *testing.T, body string) *secretsStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &secretsStore{path: path}
}

func TestSecretsLookup(t *testing.T) {
	sec := writeSecretsFile(t, "FOO=bar\n# a comment\n\nWEBHOOK_SECRET=s3cr3t\n")

	v, err := sec.lookup("WEBHOOK_SECRET")
	if err != nil || v != "s3cr3t" {
		t.Fatalf("lookup(WEBHOOK_SECRET) = %q, %v", v, err)
	}
	if _, err := sec.lookup("MISSING"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestSecretsLookupNilStore(t *testing.T) {
	var sec *secretsStore
	if _, err := sec.lookup("ANY"); err == nil {
		t.Fatal("expected error from a nil store")
	}
}

func TestResolveSecretRefsLiteralPassthrough(t *testing.T) {
	edits := map[string]string{"PORT": "8080"}
	resolved, refs, err := resolveSecretRefs(edits, nil)
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
	sec := writeSecretsFile(t, "API_KEY=live-value\n")
	edits := map[string]string{"API_KEY": "ref:API_KEY", "PORT": "8080"}

	resolved, refs, err := resolveSecretRefs(edits, sec)
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
	sec := writeSecretsFile(t, "OTHER=x\n")
	_, _, err := resolveSecretRefs(map[string]string{"API_KEY": "ref:MISSING"}, sec)
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
	sec := writeSecretsFile(t, "WEBHOOK_SECRET=sk_live_abc123\n")

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
	sec := writeSecretsFile(t, "OTHER=x\n")

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

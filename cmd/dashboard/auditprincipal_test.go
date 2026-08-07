package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readAudit returns the decoded entries written during a test.
func withAuditFile(t *testing.T) func() []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	prev := auditF
	if err := openAuditLog(path); err != nil {
		t.Fatalf("openAuditLog: %v", err)
	}
	t.Cleanup(func() { auditF = prev })
	return func() []map[string]any {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read audit: %v", err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("bad audit line %q: %v", line, err)
			}
			out = append(out, m)
		}
		return out
	}
}

// The bug this fixes: a token-authenticated action was audited with an empty
// user, because requireAuth verified the token and then discarded the name.
// Every API-token call — scripts, MCP tools — was unattributed.
func TestAuditUsesTokenPrincipal(t *testing.T) {
	read := withAuditFile(t)
	store, _ := newConfirmedStore(t, "alice", "correct horse")
	raw, _, err := store.CreateToken("alice", "ci")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	var got string
	h := store.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		// Exactly what the real handlers do.
		info, _ := store.sessionFrom(r)
		audit(r, sessionUser(info), "test.action", "target")
		got = principalFrom(r)
	})

	req := httptest.NewRequest("POST", "/api/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h(httptest.NewRecorder(), req)

	if got != "alice" {
		t.Fatalf("principalFrom = %q, want alice", got)
	}
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("wrote %d audit entries, want 1", len(entries))
	}
	if entries[0]["user"] != "alice" {
		t.Fatalf("audit user = %q, want alice (empty means the bug is back)", entries[0]["user"])
	}
}

// The internal credential must attribute to its own principal, so an MCP-driven
// change is not anonymous either.
func TestAuditUsesInternalPrincipal(t *testing.T) {
	read := withAuditFile(t)
	store, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	t.Cleanup(func() { internalToken = prev })
	if err := mintInternalToken(); err != nil {
		t.Fatal(err)
	}

	h := store.requireElevated(func(w http.ResponseWriter, r *http.Request) {
		info, _ := store.sessionFrom(r)
		audit(r, sessionUser(info), "maintenance.on", "test.example")
	})
	req := httptest.NewRequest("POST", "/api/maintenance/test.example", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	h(httptest.NewRecorder(), req)

	entries := read()
	if len(entries) != 1 || entries[0]["user"] != internalUser {
		t.Fatalf("audit user = %v, want %q", entries[0]["user"], internalUser)
	}
}

// An explicitly-passed user still wins — the fallback must not override a
// handler that already knows who acted.
func TestAuditExplicitUserWins(t *testing.T) {
	read := withAuditFile(t)
	req := httptest.NewRequest("POST", "/api/x", nil)
	req = withPrincipal(req, "from-context")
	audit(req, "explicit", "a", "b")

	entries := read()
	if entries[0]["user"] != "explicit" {
		t.Fatalf("audit user = %v, want the explicitly passed name", entries[0]["user"])
	}
}

// Unauthenticated requests must not acquire a principal.
func TestPrincipalAbsentWithoutAuth(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/x", nil)
	if got := principalFrom(req); got != "" {
		t.Fatalf("principalFrom = %q, want empty", got)
	}
	if got := principalFrom(nil); got != "" {
		t.Fatalf("principalFrom(nil) = %q, want empty", got)
	}
}

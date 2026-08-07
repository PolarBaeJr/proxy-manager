package main

import "testing"

// The auto-updater audits with no HTTP request — audit(nil, ...) on both its
// success and failure paths. auditUser and actorIP dereferenced req without a
// nil check, so enabling actor attribution (a non-empty actorSecret) turned
// every auto-update into a nil dereference that killed the dashboard process.
// Containers then stopped updating entirely: the log showed "replacing" and
// nothing after it.
//
// clientIP already had this guard, with a comment naming the auto-updater. The
// two functions in actor.go did not.
func TestAuditUserAcceptsNilRequest(t *testing.T) {
	// Attribution ON is the condition that made this reachable: with an empty
	// secret both functions return before touching the request, which is why
	// this went unnoticed until attribution was switched on.
	prev := actorSecret
	actorSecret = []byte("test-secret")
	defer func() { actorSecret = prev }()

	if got := auditUser(nil, "autoupdate"); got != "autoupdate" {
		t.Fatalf("auditUser(nil) = %q, want the fallback %q", got, "autoupdate")
	}
	if got := actorIP(nil); got != "" {
		t.Fatalf("actorIP(nil) = %q, want empty", got)
	}
}

// Guard the guard: with no secret configured the behaviour must be unchanged,
// so the nil check cannot mask a regression in the normal path.
func TestAuditUserNilRequestWithoutAttribution(t *testing.T) {
	prev := actorSecret
	actorSecret = nil
	defer func() { actorSecret = prev }()

	if got := auditUser(nil, "autoupdate"); got != "autoupdate" {
		t.Fatalf("auditUser(nil) = %q, want %q", got, "autoupdate")
	}
}

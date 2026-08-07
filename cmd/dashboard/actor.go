// Attribution for service-to-service calls.
//
// A backend that calls this API (the MCP server, say) authenticates with a
// single shared API token, so every action it performs would otherwise be
// audited against whoever generated that token rather than the person who
// actually asked for it. With more than one user that makes the audit log
// actively misleading.
//
// The proxy — which authenticated the person — forwards a short-lived signed
// assertion naming them. This verifies it and uses the name for the AUDIT LOG
// AND NOTHING ELSE.
//
// That restriction is the whole security argument. Anyone holding the API
// token could mint an assertion naming any user if they also had the secret;
// the token already grants full API access, so attribution cannot be allowed
// to grant anything further. It must never reach userAllowed, elevation, or
// any authorization decision. Treat it as a label, not a claim of authority.
package main

import (
	"encoding/hex"
	"log"
	"net/http"
	"strings"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// actorHeader must match cmd/proxy's ActorHeader.
const actorHeader = "X-Pmgr-Actor"

// actorSecret is the shared secret for verifying assertions. Empty means the
// feature is off and every caller is audited as the token owner.
var actorSecret []byte

// initActorSecret reads PMGR_ACTOR_SECRET. Returns lines the caller should log.
//
// Warns loudly when unset while still working: a silent fallback would leave
// every MCP-driven change attributed to the token owner with nothing anywhere
// saying why — the same shape of invisible misconfiguration that left
// CLOUDFLARE_ZONES and MAINT_PAGE_DIR quietly doing nothing.
func initActorSecret(getenv func(string) string) []string {
	raw := strings.TrimSpace(getenv("PMGR_ACTOR_SECRET"))
	if raw == "" {
		return []string{"⚠ PMGR_ACTOR_SECRET unset — service-to-service actions are audited as the API token's owner, not the end user"}
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return []string{"⚠ PMGR_ACTOR_SECRET is not valid hex (" + err.Error() + ") — falling back to token-owner attribution"}
	}
	actorSecret = b
	return []string{"attribution enabled: service calls are audited against the end user"}
}

// auditUser is the name to record for a request.
//
// Prefers a verified assertion, falls back to the authenticated principal.
// The fallback must never fail the request: an expired or malformed assertion
// degrades attribution, it does not break a deploy.
func auditUser(req *http.Request, fallback string) string {
	// Internal actors audit without a request — the auto-updater calls
	// audit(nil, ...) on both its success and failure paths. clientIP already
	// guards this case; these two did not, so enabling actor attribution turned
	// every auto-update into a nil dereference that killed the dashboard
	// process. The updater then never got past "replacing", and no container
	// was ever updated again.
	if req == nil {
		return fallback
	}
	if len(actorSecret) == 0 {
		return fallback
	}
	raw := req.Header.Get(actorHeader)
	if raw == "" {
		return fallback
	}
	claims, ok := sso.VerifyActor(raw, actorSecret)
	if !ok || claims.Username == "" {
		// Worth a line: a bad assertion means clock skew, a secret mismatch, or
		// someone probing. None of them should be silent.
		log.Printf("audit: ignoring invalid actor assertion, attributing to %q", fallback)
		return fallback
	}
	// Marked so a reader can tell a delegated action from a direct one, and
	// still see which credential carried it.
	return claims.Username + " (via " + fallback + ")"
}

// actorIP is the real client address from a verified assertion, or "" to use
// the request's own. Without it an audited MCP action shows the MCP
// container's IP, which is misleading in a different way than a wrong name.
func actorIP(req *http.Request) string {
	// Same nil-request case as auditUser above.
	if req == nil {
		return ""
	}
	if len(actorSecret) == 0 {
		return ""
	}
	claims, ok := sso.VerifyActor(req.Header.Get(actorHeader), actorSecret)
	if !ok {
		return ""
	}
	return claims.IP
}

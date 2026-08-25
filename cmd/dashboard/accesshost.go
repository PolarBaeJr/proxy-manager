// Cross-host access-log viewing (Phase 3). Extends /api/access with an
// on-demand ?host=<identity> parameter, resolved via PeerRegistry.URLForIdentity
// and forwarded to a peer dashboard's /peer/access — never reaching the peer's
// proxy container directly, since the peer mesh's /peer/* endpoints are
// dashboard-to-dashboard only (see peers.go).
//
// Kept as a RAW BYTE PASSTHROUGH end to end (local proxy -> peer dashboard ->
// requesting dashboard -> browser), not a decode/re-encode, because
// AccessEntry (cmd/proxy/accesslog.go) has no field that needs redaction —
// unlike Images' DeleteToken (see imageshost_test.go / api.go's /api/images
// ?host= branch), nothing here is an actionable credential. If a future
// field on AccessEntry ever does need stripping, this passthrough must
// become a decode-into-struct-and-filter like Images; it is only valid as
// long as that's not the case.
//
// Invariant: /peer/access only ever answers with THIS host's own local px —
// it never re-reads a host= param or re-forwards to another peer. A mesh of
// dashboards can therefore never loop (same invariant peers.go documents for
// /peer/service-status and /peer/services).
package main

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"
)

// forwardAccessLog GETs proxyURL+"/access" (forwarding rawQuery verbatim —
// query keys the proxy doesn't recognize, like a stray "host=", are simply
// ignored by accessHandler) and copies the response straight through:
// status code, Content-Type, and body bytes unchanged. Shared by /api/access's
// local path (api.go) and peerAccessHandler below (answering on behalf of
// its own local proxy for a remote caller) so the wire shape can never drift
// between the two.
func forwardAccessLog(ctx context.Context, w http.ResponseWriter, proxyURL, rawQuery string) {
	u := proxyURL + "/access"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusInternalServerError)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "proxy unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// peerAccessHandler returns the HTTP handler for GET /peer/access on the
// dedicated peer-handshake port — same bearer-auth shape as the other
// /peer/* handlers in peers.go. Answers with THIS host's own local proxy
// access log only (forwardAccessLog against proxyURL), never re-forwarding a
// host= param. Sets X-Peer-Identity on success.
func peerAccessHandler(secret, identity, proxyURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if proxyURL == "" {
			http.Error(w, "no local proxy configured on this host", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Peer-Identity", identity)
		forwardAccessLog(r.Context(), w, proxyURL, r.URL.RawQuery)
	})
}

// forwardAccessLogToPeer bearer-authenticates against peerBase+"/peer/access"
// and, on a 200, copies the response straight through (same passthrough
// contract as forwardAccessLog for that leg). A non-200 from the peer is
// deliberately NOT relayed verbatim: 401/403 are the dashboard's own auth
// vocabulary (see writeCFErr in api.go — same reasoning applies here), and
// the Access tab polls every 5s while "follow" is on, so relaying a peer's
// stale/rotated DASHBOARD_PEER_SECRET straight through would pop an auth
// dialog on a loop. Every non-200 peer response becomes a 502 instead.
//
// Uses a longer timeout than peerGET's 2s convention deliberately: peerGET's
// callers move small fixed-shape JSON; an access-log fetch can be up to
// ringSize=2000 entries (several hundred KB) over a Tailscale link to a Pi,
// not loopback.
func forwardAccessLogToPeer(ctx context.Context, w http.ResponseWriter, peerBase, secret, rawQuery string) {
	u := strings.TrimRight(peerBase, "/") + "/peer/access"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		http.Error(w, "peer unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		http.Error(w, "peer error: "+resp.Status+": "+string(body), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if id := resp.Header.Get("X-Peer-Identity"); id != "" {
		w.Header().Set("X-Peer-Identity", id)
	}
	_, _ = io.Copy(w, resp.Body)
}

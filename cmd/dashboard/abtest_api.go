package main

// HTTP side of A/B tests: /api/services/{name}/ab* (api.go) and its peer
// counterpart /peer/services/{name}/ab* (peers.go) share serveABAPI; the
// §1.12 guards shared by both handlers live in abGuard; /peer/ab relays
// this host's proxy report to a peer's drain/stats view.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

const abMaxRequestBody = 64 << 10

// isABRoute reports whether sub (the part after /services/{name}/) is one
// of the A/B routes.
func isABRoute(sub string) bool {
	switch sub {
	case "ab", "ab/split", "ab/groups", "ab/abort", "ab/promote", "ab/discard", "ab/reset":
		return true
	}
	return false
}

// abGuard is §1.12 for the generic service actions: refuse (409) what
// would fight an A/B test. Replace/rolling-replace/spread/stage/rollout
// need no test anywhere for svc; plain promote/discard must not touch an
// A/B B — the /ab endpoints own it. Scale stays allowed (it never clones a
// canary: scaleServiceWithHealthcheck templates from liveOnly).
func abGuard(r *http.Request, dc *dockerClient, name string, parts []string) error {
	if len(parts) != 2 {
		return nil
	}
	ctx := r.Context()
	switch {
	case r.Method == http.MethodPost && (parts[1] == "replace" || parts[1] == "rolling-replace" || parts[1] == "spread" || parts[1] == "stage" || parts[1] == "rollout"):
		if dc.abActive(ctx, name) {
			return errABActive{Service: name, Hint: "promote or discard it first"}
		}
	case (r.Method == http.MethodPost && parts[1] == "promote") || (r.Method == http.MethodDelete && parts[1] == "canary"):
		if dc.abCanaryLocal(ctx, name) {
			return errABActive{Service: name, Hint: "its canary is the test's B — use the A/B endpoints"}
		}
	}
	return nil
}

func writeABErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), abErrStatus(err))
}

// abOpBody is every op's optional body; an empty body is all defaults.
type abOpBody struct {
	Split          *int              `json:"split"`
	Groups         map[string]string `json:"groups"`
	Force          bool              `json:"force"`
	ConfirmAborted bool              `json:"confirm_aborted"`
}

func decodeABOp(r *http.Request) (abOpBody, error) {
	var b abOpBody
	data, err := io.ReadAll(io.LimitReader(r.Body, abMaxRequestBody))
	if err != nil {
		return b, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return b, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, abBadRequest("invalid body: %v", err)
	}
	return b, nil
}

// serveABAPI handles one /ab route for name on THIS host. actor is the
// audit user. Reports whether sub was an A/B route.
func serveABAPI(w http.ResponseWriter, r *http.Request, dc *dockerClient, name, sub, actor string) bool {
	if !isABRoute(sub) {
		return false
	}
	m := dc.ab
	if m == nil {
		http.Error(w, "A/B tests are not enabled on this host", http.StatusServiceUnavailable)
		return true
	}
	if !validServiceName(name) {
		http.Error(w, "invalid service name", http.StatusBadRequest)
		return true
	}
	if sub == "ab" && r.Method == http.MethodGet {
		httpx.WriteJSON(w, http.StatusOK, m.Status(r.Context(), name))
		return true
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	var (
		rec    *abRecord
		err    error
		action string
		detail = name
	)
	if sub == "ab" {
		data, rerr := io.ReadAll(io.LimitReader(r.Body, abMaxRequestBody))
		if rerr != nil {
			httpx.WriteErr(w, rerr)
			return true
		}
		req, derr := decodeABStart(data)
		if derr != nil {
			writeABErr(w, derr)
			return true
		}
		action = "service.ab_start"
		rec, err = m.Start(r.Context(), name, req, true)
		if err == nil {
			detail = fmt.Sprintf("%s id=%s image=%s replicas=%d", name, rec.ID, rec.Image, rec.Replicas)
		}
	} else {
		body, derr := decodeABOp(r)
		if derr != nil {
			writeABErr(w, derr)
			return true
		}
		switch sub {
		case "ab/split":
			if body.Split == nil {
				http.Error(w, "split is required", http.StatusBadRequest)
				return true
			}
			action = "service.ab_split"
			rec, err = m.SetSplit(name, *body.Split, true)
			detail = fmt.Sprintf("%s split=%d", name, *body.Split)
		case "ab/groups":
			action = "service.ab_groups"
			rec, err = m.SetGroups(name, body.Groups, true)
			detail = fmt.Sprintf("%s groups=%s", name, abGroupsLabel(body.Groups))
		case "ab/abort":
			action = "service.ab_abort"
			rec, err = m.Abort(name, true)
		case "ab/promote":
			action = "service.ab_promote"
			rec, err = m.Promote(name, body.Force, body.ConfirmAborted, true)
			detail = fmt.Sprintf("%s force=%t confirm_aborted=%t", name, body.Force, body.ConfirmAborted)
		case "ab/discard":
			action = "service.ab_discard"
			rec, err = m.Discard(name, body.Force, true)
			detail = fmt.Sprintf("%s force=%t", name, body.Force)
		case "ab/reset":
			action = "service.ab_reset"
			rec, err = m.Reset(r.Context(), name, body.Force, true)
			detail = fmt.Sprintf("%s force=%t", name, body.Force)
		}
	}
	if err != nil {
		var ae abError
		if !errors.As(err, &ae) {
			// Not a validation/precondition refusal: an internal failure
			// (e.g. the store couldn't be written).
			httpx.WriteErr(w, err)
			return true
		}
		writeABErr(w, err)
		return true
	}
	if rec != nil && !strings.Contains(detail, "id=") {
		detail += " id=" + rec.ID
	}
	audit(r, actor, action, detail)
	httpx.WriteJSON(w, http.StatusAccepted, abAccepted(name, rec))
	return true
}

// servePeerABPromoteReplace serves POST /peer/services/{name}/ab/promote-
// replace (peer mesh only, behind -peer-writes): the test's owner, finalizing
// a promote, asks this host to roll its own (A) replicas onto B's image
// before B goes live there. It is the one rolling replace abGuard would
// otherwise refuse while the test is listed, so it is narrowed to exactly
// that: this host must not hold the test's B itself, and its own proxy must
// list the named test id for name, not discarding. Everything else is the
// plain peer rolling-replace (capacity gate, one job per service; GET
// .../rolling-replace polls it).
func servePeerABPromoteReplace(w http.ResponseWriter, r *http.Request, dc *dockerClient, onb *OnboardedStore, registry *PeerRegistry, secret string, rom *rollingOpManager, name string) {
	var body abPeerPromoteRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, abMaxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || !abIDRe.MatchString(body.ID) || body.Image == "" {
		http.Error(w, "body must be {\"id\": <test id>, \"image\": <B image>}", http.StatusBadRequest)
		return
	}
	if _, ok := onb.Get(name); ok {
		http.Error(w, fmt.Sprintf("%q is an onboarded service — rolling replace only supports label-managed services", name), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if dc.abCanaryLocal(ctx, name) {
		http.Error(w, fmt.Sprintf("this host runs %q's B — its own promote handles it", name), http.StatusConflict)
		return
	}
	if dc.ab == nil {
		http.Error(w, "no A/B manager on this host", http.StatusConflict)
		return
	}
	rep, err := fetchProxyAB(ctx, dc.ab.client, dc.ab.proxyURL, name)
	if err != nil {
		http.Error(w, "can't confirm the test with this host's proxy: "+err.Error(), http.StatusConflict)
		return
	}
	if exp := rep.experiment(name); exp == nil || exp.ID != body.ID || exp.Phase == abPhaseDiscarding {
		http.Error(w, fmt.Sprintf("this host's proxy doesn't list A/B test %s for %q as promotable", body.ID, name), http.StatusConflict)
		return
	}
	if err := ensureRollingReplaceCapacity(ctx, dc, registry, secret, name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err := rom.start(name, ReplaceServiceRequest{Image: body.Image})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	audit(r, "peer-mesh", "service.ab_promote_replace", fmt.Sprintf("%s id=%s => %s", name, body.ID, body.Image))
	httpx.WriteJSON(w, http.StatusAccepted, st)
}

// abAccepted is the 202 body: the persisted intent. The containers follow
// in the background — poll GET ab (op clears when applied; last_error says
// why it didn't).
func abAccepted(name string, rec *abRecord) map[string]any {
	out := map[string]any{"status": "accepted", "service": name, "job": "GET /api/services/" + name + "/ab"}
	if rec != nil {
		out["id"], out["phase"], out["pending"], out["op"] = rec.ID, rec.Phase, rec.Pending, rec.Op
	}
	return out
}

// peerABHandler serves GET /peer/ab?service= on the peer-handshake port:
// this host's proxy /ab report, re-encoded from the known shape, for a
// peer dashboard's mesh-wide drain and stats. Read-only, so it doesn't
// need -peer-writes — only the shared secret.
func peerABHandler(secret, proxyURL string) http.Handler {
	client := &http.Client{Timeout: abProxyFetchTimeout}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		svc := r.URL.Query().Get("service")
		if svc != "" && !validServiceName(svc) {
			http.Error(w, "invalid service name", http.StatusBadRequest)
			return
		}
		rep, err := fetchProxyAB(r.Context(), client, proxyURL, svc)
		if err != nil {
			http.Error(w, "proxy unavailable", http.StatusBadGateway)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rep)
	})
}

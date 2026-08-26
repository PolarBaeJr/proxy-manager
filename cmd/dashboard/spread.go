// Cross-host scaling: place additional replicas of a label-managed service on
// a peer host as members of the SAME logical service, rather than as an
// independent fork.
//
// The distinction from duplicate.go matters and is the whole point of this
// file. "Duplicate to host…" creates a separately-identified container plus a
// SECOND, competing routes.json entry for the same host+path, whose backend
// URL points at a port the peer publishes on 127.0.0.1 only — so that entry
// is a black hole until an operator hand-writes an nginx-stream block (see
// runServiceDuplicate's hint). Spread instead:
//
//   - reuses the origin's proxy.service / proxy.host / proxy.path identity, so
//     the replica is the same route, not a competing one,
//   - publishes NO host port and writes NO routes.json entry, so it adds no
//     new network exposure and cannot create a routing conflict,
//   - relies on cmd/proxy's existing peer-route mesh (peersync.go /
//     peermerge.go) to carry the peer's backends into the origin's routing
//     pool, opted into active load balancing by the proxy.spread label this
//     file sets — see RouteGroup.Spread in cmd/proxy/router.go.
//
// Like runServiceDuplicate, and for the same reason, this does NOT go through
// api.go's generic ?host= forwardServiceMutation: the origin has to read the
// real container's live env server-side rather than trust a caller-supplied
// body.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

// labelSpread mirrors cmd/proxy/docker.go's constant of the same name — see
// the label-const block in duplicate.go for why these are duplicated rather
// than shared.
const labelSpread = "proxy.spread"

// maxSpreadReplicas caps how many replicas one call may place on a peer.
// Not a capacity model — just a blast-radius bound on a typo'd replica count
// reaching a live host.
const maxSpreadReplicas = 10

// SpreadServiceRequest is the browser/MCP-facing body for
// POST /api/services/{name}/spread.
type SpreadServiceRequest struct {
	Target   string `json:"target"`
	Replicas int    `json:"replicas,omitempty"`
	// AllowUnreachableEnv acknowledges the env-reachability refusal below.
	// Required because the common failure mode is silent: a replica whose
	// DB_HOST names a Docker network alias starts cleanly on the peer and
	// then serves errors for its share of live traffic.
	AllowUnreachableEnv bool `json:"allow_unreachable_env,omitempty"`
}

// SpreadServiceResponse is the browser/MCP-facing response.
type SpreadServiceResponse struct {
	Status   string   `json:"status"`
	Target   string   `json:"target"`
	Replicas int      `json:"replicas"`
	Warnings []string `json:"warnings,omitempty"`
}

// peerSpreadRequest is what the origin sends to the target peer's
// POST /peer/spread. Built entirely server-side from the origin's own live
// Docker state — never a passthrough of the caller's body.
type peerSpreadRequest struct {
	Service    string   `json:"service"` // proxy.service identity, also the replica-name stem
	Display    string   `json:"display,omitempty"`
	Group      string   `json:"group,omitempty"`
	Image      string   `json:"image"`
	Env        []string `json:"env,omitempty"`
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port"`
	Path       string   `json:"path,omitempty"`
	Strip      bool     `json:"strip,omitempty"`
	Health     string   `json:"health,omitempty"`
	Auth       bool     `json:"auth,omitempty"`
	AuthUsers  []string `json:"auth_users,omitempty"`
	AuthMode   string   `json:"auth_mode,omitempty"`
	RateLimit  bool     `json:"ratelimit,omitempty"`
	RateRPM    int      `json:"ratelimit_rpm,omitempty"`
	AutoUpdate bool     `json:"autoupdate,omitempty"`
	Weight     int      `json:"weight,omitempty"`
	Replicas   int      `json:"replicas"`
	// Healthcheck carries the origin's Config.Healthcheck (set by whatever
	// created the origin container — a compose file's `healthcheck:` stanza,
	// most often — not baked into the image) so the seed replica's own
	// recreate-based rolling health gate isn't decorative from the start.
	Healthcheck *healthcheckSpec `json:"healthcheck,omitempty"`
}

type peerSpreadResponse struct {
	Status   string `json:"status"`
	Service  string `json:"service"`
	Replicas int    `json:"replicas"`
}

// errSpreadNotFound is returned when the named service has no containers at
// all — the API layer maps it to 404.
var errSpreadNotFound = errors.New("service not found")

// peerSpreadError wraps a peer's non-2xx (or transport-level, statusCode 0)
// response so the API layer can map it exactly as mapPeerMutationErr would.
// Same shape and reasoning as peerDuplicateError.
type peerSpreadError struct {
	statusCode int
	body       []byte
}

func (e *peerSpreadError) Error() string {
	return fmt.Sprintf("peer rejected spread (status %d): %s", e.statusCode, string(e.body))
}

// connectionEnvKeys are the env-var name shapes whose VALUE is expected to be
// an address. Only these are checked for host-local-looking values, because a
// blanket "value has no dot" rule fires on LOG_LEVEL=info and ENV=production
// and would make the acknowledgement flag a reflex checkbox.
var connectionEnvSuffixes = []string{"_HOST", "_ADDR", "_ADDRESS", "_URL", "_URI", "_DSN", "_ENDPOINT", "_SERVER", "_BROKER"}

var connectionEnvPrefixes = []string{"DATABASE", "DB", "POSTGRES", "MYSQL", "MARIADB", "MONGO", "REDIS", "AMQP", "RABBITMQ", "MEMCACHED", "ELASTIC"}

// credentialEnvFragments name env vars whose duplication onto a second live
// host is the risk the discord-bot incident turned up: a second container
// holding the same bot token connects as the same identity and answers every
// event twice. There is no way to tell a safe copy from an unsafe one from
// the outside, so these are warned about, never blocked.
var credentialEnvFragments = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "APIKEY", "API_KEY", "PRIVATE_KEY", "CREDENTIAL", "SESSION"}

// unreachableEnvKeys returns the NAMES (never the values — Env routinely
// holds secrets) of connection-shaped env vars whose value looks like it can
// only resolve inside the origin host's own Docker network: a bare hostname
// with no dot, no port-only form, and not a loopback literal. A replica on
// another host cannot resolve those, so it starts fine and then fails on
// first use.
func unreachableEnvKeys(env []string) []string {
	var out []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" || !looksLikeConnectionKey(k) {
			continue
		}
		if hostLocalTarget(v) {
			out = append(out, k)
		}
	}
	return out
}

// credentialEnvKeys returns the NAMES of env vars that look like they carry a
// shared identity or credential.
func credentialEnvKeys(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		up := strings.ToUpper(k)
		for _, frag := range credentialEnvFragments {
			if strings.Contains(up, frag) {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

func looksLikeConnectionKey(k string) bool {
	up := strings.ToUpper(k)
	for _, s := range connectionEnvSuffixes {
		if strings.HasSuffix(up, s) {
			return true
		}
	}
	for _, p := range connectionEnvPrefixes {
		if up == p || strings.HasPrefix(up, p+"_") {
			return true
		}
	}
	return false
}

// hostLocalTarget reports whether v names something only resolvable on the
// origin host — a bare Docker network alias ("postgres", "redis:6379") or a
// loopback literal. A value with a dot (an FQDN or an IP) or an explicit
// tailnet address is assumed routable and passes.
func hostLocalTarget(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	// Strip a scheme and any credentials so postgres://user:pass@db:5432/x
	// is judged on "db", not on the whole string. Values are never logged or
	// returned — only the key name is.
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	if i := strings.LastIndex(v, "@"); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.IndexAny(v, "/?"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, ":"); i >= 0 && !strings.Contains(v, "]") {
		v = v[:i]
	}
	v = strings.Trim(v, "[]")
	if v == "" {
		return false
	}
	if v == "localhost" || v == "127.0.0.1" || v == "::1" || v == "host.docker.internal" {
		return true
	}
	return !strings.Contains(v, ".") && !strings.Contains(v, ":")
}

// anySpreadLabeled reports whether any of these containers was placed by a
// spread, which is what distinguishes replicas this feature owns from a
// same-named service that arrived on the host some other way.
func anySpreadLabeled(in []dockerContainer) bool {
	for _, ct := range in {
		if ct.Labels[labelSpread] == "true" {
			return true
		}
	}
	return false
}

// runServiceSpread places `replicas` members of a label-managed service on a
// peer host under the same proxy.service identity. Refuses anything it cannot
// verify is safe to run a second live copy of; on success the peer's own
// proxy picks the new containers up by label and advertises them back into
// this host's routing pool, so nothing here touches routes.json.
func runServiceSpread(ctx context.Context, dc *dockerClient, registry *PeerRegistry, onb *OnboardedStore, name string, req SpreadServiceRequest, actorAssertion string) (SpreadServiceResponse, error) {
	if infraContainerNames[name] {
		return SpreadServiceResponse{}, fmt.Errorf("%q is a fixed infrastructure container", name)
	}
	replicas := req.Replicas
	if replicas == 0 {
		replicas = 1
	}
	if replicas < 1 || replicas > maxSpreadReplicas {
		return SpreadServiceResponse{}, fmt.Errorf("replicas must be between 1 and %d", maxSpreadReplicas)
	}
	svc, ok, err := findService(ctx, dc, name)
	if err != nil {
		return SpreadServiceResponse{}, err
	}
	if !ok {
		return SpreadServiceResponse{}, errSpreadNotFound
	}
	// Same reasoning as runServiceDuplicate: findService never sets
	// svc.Onboarded, so the store is consulted directly too.
	if svc.Onboarded {
		return SpreadServiceResponse{}, fmt.Errorf("%q is an onboarded service — cross-host scaling only supports label-managed services", name)
	}
	if onb != nil {
		if _, ok := onb.Get(name); ok {
			return SpreadServiceResponse{}, fmt.Errorf("%q is an onboarded service — cross-host scaling only supports label-managed services", name)
		}
	}
	if svc.Unscalable {
		return SpreadServiceResponse{}, fmt.Errorf("%q is a singleton service — cross-host scaling is not supported for singleton services", name)
	}
	if svc.Host == "" {
		// Without a proxy.host there is no route for the mesh to merge the
		// peer's backends into, so the replica would run and receive nothing.
		return SpreadServiceResponse{}, fmt.Errorf("%q has no %s label — a cross-host replica would never receive traffic", name, labelHost)
	}
	if !validPort(svc.Port) {
		return SpreadServiceResponse{}, fmt.Errorf("%q has no valid %s label", name, labelPort)
	}
	if registry == nil {
		return SpreadServiceResponse{}, fmt.Errorf("peer mesh not configured")
	}
	peerURL, ok := registry.URLForIdentity(req.Target)
	if !ok {
		return SpreadServiceResponse{}, fmt.Errorf("unknown target host %q", req.Target)
	}
	secret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if secret == "" {
		return SpreadServiceResponse{}, fmt.Errorf("DASHBOARD_PEER_SECRET not configured")
	}

	all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return SpreadServiceResponse{}, err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return SpreadServiceResponse{}, fmt.Errorf("service %q has no live replicas", name)
	}
	tpl := preferRunning(existing)[0]

	env, err := dc.inspectEnv(ctx, tpl.ID)
	if err != nil {
		return SpreadServiceResponse{}, fmt.Errorf("inspect template env: %w", err)
	}
	clone, err := dc.inspectCloneSpec(ctx, tpl.ID)
	if err != nil {
		return SpreadServiceResponse{}, fmt.Errorf("inspect template clone spec: %w", err)
	}

	// Bind mounts are refused for the same reason duplicate.go refuses them
	// (no such path on the target). Named volumes are refused too, which is
	// STRICTER than duplicate.go on purpose: a fork with empty volumes is at
	// least self-consistent, but a replica of one logical service whose
	// members hold divergent local state is a data-corruption bug that only
	// shows up under load balancing. Copying volume data across hosts is out
	// of scope.
	for _, m := range clone.Mounts {
		switch m.Type {
		case "bind":
			return SpreadServiceResponse{}, fmt.Errorf("refusing to scale %q across hosts: bind mount %s would not exist on the target host — resolve manually first", name, m.Source)
		case "volume":
			return SpreadServiceResponse{}, fmt.Errorf("refusing to scale %q across hosts: named volume %q cannot be shared between hosts, and a replica with its own empty copy would diverge from the original", name, m.Source)
		}
	}

	var warnings []string
	if bad := unreachableEnvKeys(env); len(bad) > 0 {
		if !req.AllowUnreachableEnv {
			return SpreadServiceResponse{}, fmt.Errorf(
				"refusing to scale %q across hosts: %s name a host-local address (a Docker network alias or loopback) that a replica on %s cannot reach — point them at a routable address first, or pass allow_unreachable_env to override",
				name, strings.Join(bad, ", "), req.Target)
		}
		warnings = append(warnings, fmt.Sprintf("%s look host-local and were accepted only because allow_unreachable_env was set — the replica on %s may fail at runtime", strings.Join(bad, ", "), req.Target))
	}
	if creds := credentialEnvKeys(env); len(creds) > 0 {
		warnings = append(warnings, fmt.Sprintf("%s are copied verbatim to %s — a second live copy connects as the SAME identity, which for a bot/worker means duplicated side effects, not extra capacity", strings.Join(creds, ", "), req.Target))
	}

	var authUsers []string
	if raw := tpl.Labels[labelAuthUsers]; raw != "" {
		for _, u := range strings.Split(raw, ",") {
			authUsers = append(authUsers, strings.TrimSpace(u))
		}
	}
	rateRPM, _ := strconv.Atoi(tpl.Labels[labelRateRPM])
	autoUpdate := tpl.Labels[labelAutoUpdate] == "true"
	weight := parseWeightLabel(tpl.Labels[labelWeight])

	// proxy.name is a free-text display label everywhere else in this
	// codebase (set once at creation, shown escaped in the UI, never itself
	// identifier-shaped — e.g. "Badminton Player (prod)") but the peer's
	// receiving handler validates Display with validServiceName, the strict
	// identifier allowlist meant for proxy.service-style values. Sending an
	// ordinary human-readable name through would make the peer hard-refuse
	// the ENTIRE spread over a cosmetic label. Drop it instead of the origin
	// hard-failing here too — the new replica just falls back to showing its
	// raw service name, same as any service that never set proxy.name.
	display := tpl.Labels[labelName]
	if display != "" && !validServiceName(display) {
		warnings = append(warnings, fmt.Sprintf("proxy.name %q can't be carried over as-is (letters, digits, . _ - only) — the replica on %s will show its raw service name instead", display, req.Target))
		display = ""
	}

	// proxy.group drives which Status-view/statusbot folder the replica lands
	// in. Without it, the peer's default-to-service-name rule (docker.go)
	// puts the new replica in its OWN group instead of the origin's, so the
	// same logical service shows up as two unrelated-looking entries across
	// hosts. Drop rather than hard-fail for the same reason as Display: a
	// group label that predates the identifier-only rule shouldn't block the
	// whole spread over a cosmetic mismatch.
	group := tpl.Labels[labelGroup]
	if group != "" && !validServiceName(group) {
		warnings = append(warnings, fmt.Sprintf("proxy.group %q can't be carried over as-is (letters, digits, . _ - only) — the replica on %s will default to its own group", group, req.Target))
		group = ""
	}

	// proxy.weight has no upper bound at the label level (parseWeightLabel
	// accepts any positive int, e.g. a hand-authored compose file), but the
	// peer's receiving handler validates it against validWeight/
	// maxServiceWeight like every other weight-setting path in this
	// codebase. Sending a value outside that range would make the peer
	// hard-refuse the ENTIRE spread over a number that only affects routing
	// share — drop to the default instead, same reasoning as Display/Group.
	if weight > maxServiceWeight {
		warnings = append(warnings, fmt.Sprintf("proxy.weight %d exceeds the maximum of %d — the replica on %s will use the default weight instead", weight, maxServiceWeight, req.Target))
		weight = 1
	}

	peerReq := peerSpreadRequest{
		Service:     svc.Name,
		Display:     display,
		Group:       group,
		Image:       tpl.Image,
		Env:         env,
		Host:        svc.Host,
		Port:        svc.Port,
		Path:        svc.Path,
		Strip:       tpl.Labels[labelStrip] == "true",
		Health:      tpl.Labels[labelHealth],
		Auth:        tpl.Labels[labelAuth] == "true",
		AuthUsers:   authUsers,
		AuthMode:    tpl.Labels[labelAuthMode],
		RateLimit:   tpl.Labels[labelRateLimit] == "true",
		RateRPM:     rateRPM,
		AutoUpdate:  autoUpdate,
		Weight:      weight,
		Replicas:    replicas,
		Healthcheck: clone.Healthcheck,
	}
	reqBody, err := json.Marshal(peerReq)
	if err != nil {
		return SpreadServiceResponse{}, err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	var peerResp peerSpreadResponse
	statusCode, respBody, mutErr := peerMutate(ctx, client, peerURL, secret, http.MethodPost, "/peer/spread",
		60*time.Second, bytes.NewReader(reqBody), &peerResp, actorAssertion)
	if mutErr != nil {
		return SpreadServiceResponse{}, &peerSpreadError{statusCode: 0, body: []byte(mutErr.Error())}
	}
	if statusCode < 200 || statusCode >= 300 {
		return SpreadServiceResponse{}, &peerSpreadError{statusCode: statusCode, body: respBody}
	}

	return SpreadServiceResponse{Status: "spread", Target: req.Target, Replicas: peerResp.Replicas, Warnings: warnings}, nil
}

// peerSpreadHandler returns the HTTP handler for POST /peer/spread on the
// dedicated peer-handshake port — same bearer-auth/writesEnabled gate as
// every other write-mesh handler. Labels are built SERVER-SIDE from typed
// fields only, so a caller cannot smuggle an arbitrary label in.
//
// Converges to exactly req.Replicas members: the first one is created from
// the shipped spec, and scaleService then owns the count from there, so a
// repeat call with a higher or lower number adjusts rather than stacking.
func peerSpreadHandler(secret, identity string, dc *dockerClient, writesEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || !writesEnabled {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req peerSpreadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.WriteErr(w, err)
			return
		}
		if !validServiceName(req.Service) {
			http.Error(w, "invalid service (allowed: a-z A-Z 0-9 . _ -, max 63 chars)", http.StatusBadRequest)
			return
		}
		if infraContainerNames[req.Service] {
			http.Error(w, fmt.Sprintf("%q is a fixed infrastructure container", req.Service), http.StatusForbidden)
			return
		}
		if req.Image == "" {
			http.Error(w, "image is required", http.StatusBadRequest)
			return
		}
		if !validHostname(req.Host) {
			http.Error(w, "invalid hostname (allowed: a-z A-Z 0-9 . -, max 253 chars)", http.StatusBadRequest)
			return
		}
		if !validPort(req.Port) {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		if !validRoutePath(req.Path) {
			http.Error(w, "invalid path (must be empty or start with / — allowed: a-z A-Z 0-9 / _ . -, max 512 chars)", http.StatusBadRequest)
			return
		}
		if req.Display != "" && !validServiceName(req.Display) {
			http.Error(w, "invalid display name", http.StatusBadRequest)
			return
		}
		if req.Group != "" && !validServiceName(req.Group) {
			http.Error(w, "invalid group", http.StatusBadRequest)
			return
		}
		for _, u := range req.AuthUsers {
			if !validUsername(u) {
				http.Error(w, "invalid auth_users entry", http.StatusBadRequest)
				return
			}
		}
		if req.AuthMode != "" && req.AuthMode != "basic" && req.AuthMode != "oauth" {
			http.Error(w, "invalid auth_mode", http.StatusBadRequest)
			return
		}
		if req.Replicas < 1 || req.Replicas > maxSpreadReplicas {
			http.Error(w, fmt.Sprintf("replicas must be between 1 and %d", maxSpreadReplicas), http.StatusBadRequest)
			return
		}
		// 0 means the origin never had a weight label (parseWeightLabel's own
		// default) rather than an explicit request — only a non-zero value is
		// validated, matching how the /api/services/{name}/weight handler
		// treats the same range.
		if req.Weight != 0 && !validWeight(req.Weight) {
			http.Error(w, fmt.Sprintf("weight must be between 1 and %d", maxServiceWeight), http.StatusBadRequest)
			return
		}

		all, err := dc.listAll(r.Context(), fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, req.Service))
		if err != nil {
			httpx.WriteErr(w, err)
			return
		}
		// A member carrying proxy.unscalable here means the operator marked
		// this service a singleton on THIS host even though the origin didn't
		// — refuse rather than let the two hosts disagree about it.
		for _, ct := range all {
			if ct.Labels[labelUnscalable] == "true" {
				http.Error(w, fmt.Sprintf("%q is marked %s on this host", req.Service, labelUnscalable), http.StatusConflict)
				return
			}
		}

		// Containers this host already runs for the service that spread did NOT
		// place — most plausibly a leftover from "Duplicate to host…", which
		// names its container <service> and sets no proxy.spread. Proceeding
		// would hand the count to scaleService, which clones the labels of the
		// container it finds, so every new replica would come up WITHOUT the
		// spread label and the pool would never activate: the service ends up
		// running on both hosts, looking healthy everywhere, receiving no
		// cross-host traffic at all. Refuse instead of succeeding invisibly.
		liveHere := liveOnly(all)
		if len(liveHere) > 0 && !anySpreadLabeled(liveHere) {
			http.Error(w, fmt.Sprintf(
				"this host already runs containers for %q that spread did not place (no %s label) — remove or relabel them first, or use the existing duplicate flow",
				req.Service, labelSpread), http.StatusConflict)
			return
		}

		live := len(liveHere)
		if live == 0 {
			cname := fmt.Sprintf("goproxy-%s-%d", req.Service, nextReplicaIndex(all, req.Service))
			named, err := dc.listAll(r.Context(), fmt.Sprintf(`{"name":["%s"]}`, cname))
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			for _, ct := range named {
				if ct.name() == cname {
					http.Error(w, fmt.Sprintf("container %q already exists", cname), http.StatusConflict)
					return
				}
			}

			labels := map[string]string{
				labelEnable:  "true",
				labelHost:    req.Host,
				labelPort:    strconv.Itoa(req.Port),
				labelService: req.Service,
				// The origin's own containers are never relabeled, so this is
				// the only place the route's spread opt-in enters the mesh —
				// the peer proxy advertises it back and the origin adopts it
				// (cmd/proxy/peermerge.go's overlay).
				labelSpread: "true",
			}
			if req.Display != "" {
				labels[labelName] = req.Display
			}
			if req.Group != "" {
				labels[labelGroup] = req.Group
			}
			if req.Path != "" {
				labels[labelPath] = req.Path
			}
			if req.Strip {
				labels[labelStrip] = "true"
			}
			if req.Health != "" {
				labels[labelHealth] = req.Health
			}
			if req.Auth {
				labels[labelAuth] = "true"
			}
			if len(req.AuthUsers) > 0 {
				labels[labelAuthUsers] = strings.Join(req.AuthUsers, ",")
			}
			if req.AuthMode != "" {
				labels[labelAuthMode] = req.AuthMode
			}
			if req.RateLimit {
				labels[labelRateLimit] = "true"
			}
			if req.RateRPM > 0 {
				labels[labelRateRPM] = strconv.Itoa(req.RateRPM)
			}
			if req.AutoUpdate {
				labels[labelAutoUpdate] = "true"
			}
			// Weight 1 (or unset) is the proxy's own default — dropping the
			// label rather than writing it out keeps a reset-to-default
			// indistinguishable from a service that never had a weight set,
			// same convention as setWeightLabel.
			if req.Weight > 1 {
				labels[labelWeight] = strconv.Itoa(req.Weight)
			}

			// No PortBindings and no Mounts, unlike peerDuplicateHandler:
			// the replica is reached over the edge network by the peer's own
			// proxy, so it needs no host port — which also keeps
			// inspectHostConfigUnknowns from refusing to replace/autoupdate
			// it later, the way a duplicated container is refused today.
			dc.pullImage(r.Context(), req.Image)
			id, err := dc.createContainer(r.Context(), cname, createBody{
				Image:        req.Image,
				Labels:       labels,
				Env:          req.Env,
				Healthcheck:  req.Healthcheck,
				ExposedPorts: map[string]struct{}{fmt.Sprintf("%d/tcp", req.Port): {}},
			})
			if err != nil {
				httpx.WriteErr(w, err)
				return
			}
			if err := dc.startContainer(r.Context(), id); err != nil {
				httpx.WriteErr(w, err)
				return
			}
			live = 1
		}

		// scaleService owns the count from here, so a repeat call adjusts
		// rather than stacking. Skipped when the seed container above already
		// satisfies the request — scaleService would otherwise re-list purely
		// to conclude it has nothing to do.
		if live != req.Replicas {
			if err := dc.scaleService(r.Context(), req.Service, req.Replicas); err != nil {
				httpx.WriteErr(w, err)
				return
			}
		}

		audit(r, "peer-mesh", "service.spread_receive", req.Service)
		httpx.WriteJSON(w, http.StatusOK, peerSpreadResponse{Status: "scaled", Service: req.Service, Replicas: req.Replicas})
	})
}

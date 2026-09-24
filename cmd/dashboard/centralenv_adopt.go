// Central per-service env: ADOPT.
//
// Adopt turns an existing label-managed service (often compose-created) into
// a centrally managed one without an outage: the host it runs on becomes
// the origin, its live env (minus what the image itself sets) seeds a v1
// record, and the normal propagation job then recreates every replica on
// every host from it — origin first, health-gated, surge-of-one — stamping
// pmgr.env.* and dropping the compose labels as it goes.
//
// Everything is preflighted first and a dry run is the default. The report
// compares env across hosts by KEY NAME; whether two hosts hold the same
// value is decided by comparing HMAC-SHA256(nonce, value), with a fresh
// random nonce per preflight, so no value (nor an unsalted hash of one)
// ever crosses the mesh or reaches a report. The hashes and the nonce live
// only in memory for the one request.
//
// Known limitations:
//   - A value that changes between the dry run and the execute is not
//     caught: the fingerprint covers names and key sets, and the HMAC check
//     only guards the window inside the execute. Closing it would mean
//     persisting hashes.
//   - If a peer's roll fails during an adopt, that peer's cache is left
//     unusable for recreating its original (compose) replicas until the
//     origin retries or the service is released.
//   - A later version that fails while Adopting is still set (some host not
//     yet converged) takes the normal revert path, not the un-adopt one.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// adoptClaimOwner holds svc's serviceClaims slot while an adopt
	// re-checks and seeds the record; it is handed straight to the
	// propagation job (envSyncClaimOwner) so nothing can slip in between.
	adoptClaimOwner = "adopt"

	adoptAckNoHealth     = "ack_no_health"
	adoptAckCompose      = "ack_compose"
	adoptAckRestart      = "ack_restart_policy"
	adoptAckImageUpdate  = "ack_image_update"
	adoptImportBase      = "base"
	adoptImportOverride  = "override"
	defaultShmSize       = 64 << 20
	composeProjectLabel  = "com.docker.compose.project"
	composeConfigLabel   = "com.docker.compose.project.config_files"
	composeWorkdirLabel  = "com.docker.compose.project.working_dir"
	composeServiceLabel  = "com.docker.compose.service"
	composeDependsLabel  = "com.docker.compose.depends_on"
	adoptNonceBytes      = 32
	adoptLiveEnvMaxKeys  = 256
	adoptPeerCallTimeout = 10 * time.Second
)

// centralEnvAdoptImport pulls keys from one peer's live env into the record:
// "base" (every host gets that peer's value — only for keys the origin
// lacks) or "override" (only that host keeps its own value).
type centralEnvAdoptImport struct {
	Keys []string `json:"keys"`
	As   string   `json:"as"`
}

// centralEnvAdoptRequest is POST /api/services/{svc}/env/adopt. DryRun
// defaults to true; executing needs dry_run:false, the fingerprint of a dry
// run of the same state, every required ack, and every peer-only key either
// imported or accepted as dropped.
type centralEnvAdoptRequest struct {
	DryRun           *bool                            `json:"dry_run"`
	RequestID        string                           `json:"request_id,omitempty"`
	Fingerprint      string                           `json:"fingerprint,omitempty"`
	AckNoHealth      bool                             `json:"ack_no_health,omitempty"`
	AckCompose       bool                             `json:"ack_compose,omitempty"`
	AckRestartPolicy bool                             `json:"ack_restart_policy,omitempty"`
	AckImageUpdate   bool                             `json:"ack_image_update,omitempty"`
	Import           map[string]centralEnvAdoptImport `json:"import,omitempty"`
	AcceptDropped    []string                         `json:"accept_dropped,omitempty"`
}

func (r centralEnvAdoptRequest) dryRun() bool { return r.DryRun == nil || *r.DryRun }

func (r centralEnvAdoptRequest) acked(ack string) bool {
	switch ack {
	case adoptAckNoHealth:
		return r.AckNoHealth
	case adoptAckCompose:
		return r.AckCompose
	case adoptAckRestart:
		return r.AckRestartPolicy
	case adoptAckImageUpdate:
		return r.AckImageUpdate
	}
	return false
}

// adoptComposeInfo is where a compose-created service came from — paths and
// names only.
type adoptComposeInfo struct {
	Project     string   `json:"project"`
	ConfigFiles string   `json:"config_files,omitempty"`
	WorkingDir  string   `json:"working_dir,omitempty"`
	Services    []string `json:"services,omitempty"`
	DependsOn   string   `json:"depends_on,omitempty"`
}

// adoptHostFacts is one host's view of svc for a preflight. Keys (name →
// salted hash) is filled only on the peer wire (live-keys) and never copied
// into a report.
type adoptHostFacts struct {
	Identity  string   `json:"identity"`
	Members   []string `json:"members"`
	Canary    bool     `json:"canary,omitempty"`
	Busy      string   `json:"busy,omitempty"`
	Managed   bool     `json:"managed,omitempty"`
	Blockers  []string `json:"blockers,omitempty"`
	HasHealth bool     `json:"has_healthcheck"`
	// NoHealthMembers lack a Docker healthcheck of their own.
	NoHealthMembers []string          `json:"no_health_members,omitempty"`
	HealthLabel     bool              `json:"health_label,omitempty"`
	Unscalable      bool              `json:"unscalable,omitempty"`
	HasHost         bool              `json:"has_host"`
	Restart         map[string]string `json:"restart,omitempty"`
	Compose         *adoptComposeInfo `json:"compose,omitempty"`
	NewerImage      string            `json:"newer_image,omitempty"`
	Keys            map[string]string `json:"keys,omitempty"`
	RefLike         []string          `json:"ref_like,omitempty"`
	HostLocal       []string          `json:"host_local,omitempty"`
}

// adoptHostDiff is one peer's env compared with the origin's, by name.
type adoptHostDiff struct {
	PeerOnly    []string `json:"peer_only"`
	OriginOnly  []string `json:"origin_only"`
	Differs     []string `json:"differs"`
	Unreachable []string `json:"unreachable"`
}

// centralEnvAdoptReport is the (dry-run) answer. Names, counts and paths
// only — never a value or a hash.
type centralEnvAdoptReport struct {
	Service      string              `json:"service"`
	Origin       string              `json:"origin"`
	Adoptable    bool                `json:"adoptable"`
	Blockers     []string            `json:"blockers"`
	Warnings     []string            `json:"warnings"`
	Info         []string            `json:"info"`
	RequiredAcks []string            `json:"required_acks"`
	MissingAcks  []string            `json:"missing_acks"`
	Unresolved   map[string][]string `json:"unresolved"`
	Env          struct {
		OriginKeys []string                  `json:"origin_keys"`
		Hosts      map[string]*adoptHostDiff `json:"hosts"`
	} `json:"env"`
	Members     map[string][]string          `json:"members"`
	Compose     map[string]*adoptComposeInfo `json:"compose"`
	RetireSteps []string                     `json:"retire_steps"`
	// Fingerprint covers the observed state (members, key sets, compose,
	// required acks, blockers) — not the request's acks/imports — so an
	// execute can prove nothing moved since the dry run it was built from.
	Fingerprint string `json:"fingerprint"`
}

// adoptPlan is what an execute needs beyond the report. In memory only.
type adoptPlan struct {
	nonce     []byte
	originEnv map[string]string
	peers     map[string]adoptHostFacts
	peerURLs  map[string]string
	compose   *adoptComposeInfo
}

func newAdoptNonce() []byte {
	b := make([]byte, adoptNonceBytes)
	_, _ = rand.Read(b)
	return b
}

func adoptKeyHash(nonce []byte, value string) string {
	mac := hmac.New(sha256.New, nonce)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// adoptStrictFacts is what inspectAdoptStrict reads from one member.
type adoptStrictFacts struct {
	imageID     string
	env         []string
	healthcheck *healthcheckSpec
	restart     string
	refused     []string
}

// inspectAdoptStrict lists the fields of a container that createBody does
// not carry and that inspectHostConfigUnknowns does not refuse either — a
// recreate would silently drop them. Autoupdate and replace already drop
// them today (pre-existing, out of scope here); adopt refuses instead,
// because it recreates every replica of a service on every host at once.
// Fields the image itself sets are compared against the image, so an
// inherited value isn't mistaken for an override.
func (c *dockerClient) inspectAdoptStrict(ctx context.Context, id, logDriver string) (adoptStrictFacts, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return adoptStrictFacts{}, err
	}
	defer body.Close()
	var resp struct {
		Image  string `json:"Image"`
		Config struct {
			Env         []string         `json:"Env"`
			User        string           `json:"User"`
			WorkingDir  string           `json:"WorkingDir"`
			StopSignal  string           `json:"StopSignal"`
			StopTimeout *int             `json:"StopTimeout"`
			Healthcheck *healthcheckSpec `json:"Healthcheck"`
		} `json:"Config"`
		HostConfig struct {
			LogConfig struct {
				Type   string            `json:"Type"`
				Config map[string]string `json:"Config"`
			} `json:"LogConfig"`
			Tmpfs          map[string]string `json:"Tmpfs"`
			Ulimits        []json.RawMessage `json:"Ulimits"`
			Init           *bool             `json:"Init"`
			SecurityOpt    []string          `json:"SecurityOpt"`
			ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
			Sysctls        map[string]string `json:"Sysctls"`
			GroupAdd       []string          `json:"GroupAdd"`
			ShmSize        int64             `json:"ShmSize"`
			RestartPolicy  struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return adoptStrictFacts{}, err
	}
	var img struct {
		Config struct {
			User       string `json:"User"`
			WorkingDir string `json:"WorkingDir"`
			StopSignal string `json:"StopSignal"`
		} `json:"Config"`
	}
	if resp.Image != "" {
		ib, err := c.get(ctx, "/images/"+url.PathEscape(resp.Image)+"/json")
		if err != nil {
			return adoptStrictFacts{}, fmt.Errorf("inspect image: %w", err)
		}
		err = json.NewDecoder(ib).Decode(&img)
		ib.Close()
		if err != nil {
			return adoptStrictFacts{}, fmt.Errorf("inspect image: %w", err)
		}
	}
	var refused []string
	add := func(cond bool, f string) {
		if cond {
			refused = append(refused, f)
		}
	}
	cfg, hc := resp.Config, resp.HostConfig
	add(cfg.User != "" && cfg.User != img.Config.User, "Config.User")
	add(cfg.WorkingDir != "" && cfg.WorkingDir != img.Config.WorkingDir, "Config.WorkingDir")
	add(cfg.StopSignal != "" && cfg.StopSignal != img.Config.StopSignal, "Config.StopSignal")
	add(cfg.StopTimeout != nil, "Config.StopTimeout")
	add((hc.LogConfig.Type != "" && hc.LogConfig.Type != logDriver) || len(hc.LogConfig.Config) > 0, "HostConfig.LogConfig")
	add(len(hc.Tmpfs) > 0, "HostConfig.Tmpfs")
	add(len(hc.Ulimits) > 0, "HostConfig.Ulimits")
	add(hc.Init != nil && *hc.Init, "HostConfig.Init")
	add(len(hc.SecurityOpt) > 0, "HostConfig.SecurityOpt")
	add(hc.ReadonlyRootfs, "HostConfig.ReadonlyRootfs")
	add(len(hc.Sysctls) > 0, "HostConfig.Sysctls")
	add(len(hc.GroupAdd) > 0, "HostConfig.GroupAdd")
	add(hc.ShmSize != 0 && hc.ShmSize != defaultShmSize, "HostConfig.ShmSize")
	return adoptStrictFacts{imageID: resp.Image, env: cfg.Env, healthcheck: cfg.Healthcheck, restart: hc.RestartPolicy.Name, refused: refused}, nil
}

// daemonLogDriver is the daemon's default logging driver ("json-file" when
// it can't be read) — a container on it didn't choose its driver.
func (c *dockerClient) daemonLogDriver(ctx context.Context) string {
	body, err := c.get(ctx, "/info")
	if err != nil {
		return "json-file"
	}
	defer body.Close()
	var info struct {
		LoggingDriver string `json:"LoggingDriver"`
	}
	if json.NewDecoder(body).Decode(&info) != nil || info.LoggingDriver == "" {
		return "json-file"
	}
	return info.LoggingDriver
}

// adoptLocalFacts gathers this host's facts for svc. env is the template's
// own env minus what its image sets — the values stay with the caller;
// facts carries only names (and, when nonce is non-nil, salted hashes).
// ignoreClaim is a serviceClaims owner that doesn't count as busy (the
// adopt's own claim, during execute).
func adoptLocalFacts(ctx context.Context, m *envSyncManager, svc string, nonce []byte, ignoreClaim string) (adoptHostFacts, map[string]string, error) {
	dc := m.dc
	f := adoptHostFacts{Identity: m.ce.identity, Members: []string{}}
	all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err != nil {
		return f, nil, err
	}
	live := liveOnly(all)
	f.Canary = len(canaryOnly(all)) > 0
	if h := dc.claims.holder(svc); h != "" && h != ignoreClaim {
		f.Busy = "claimed by " + h
	} else if m.rom != nil {
		if st, ok := m.rom.get(svc); ok && rollingOpActive(st.Status) {
			f.Busy = "a rolling replace is running"
		}
	}
	if f.Busy == "" && m.rm != nil {
		if st, ok := m.rm.get(svc); ok && rolloutActive(st.Status) {
			f.Busy = "a canary rollout is active"
		}
	}
	if f.Busy == "" && m.busy(svc) {
		f.Busy = "a central env job is running"
	}
	f.Managed = m.ce.store.Has(svc)
	if _, ok := m.ce.cache.Get(svc); ok {
		f.Managed = true
	}
	if len(live) == 0 {
		return f, nil, nil
	}
	tpl := preferRunning(live)[0]
	f.Unscalable = tpl.Labels[labelUnscalable] == "true"
	f.HasHost = tpl.Labels[labelHost] != ""
	f.HealthLabel = tpl.Labels[labelHealth] != ""
	logDriver := dc.daemonLogDriver(ctx)
	var tplFacts adoptStrictFacts
	for _, ct := range live {
		name := ct.name()
		f.Members = append(f.Members, name)
		if ct.Labels[labelEnvOrigin] != "" {
			f.Managed = true
		}
		unknowns, err := dc.inspectHostConfigUnknowns(ctx, ct.ID)
		if err != nil {
			return f, nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		st, err := dc.inspectAdoptStrict(ctx, ct.ID, logDriver)
		if err != nil {
			return f, nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		for _, u := range append(unknowns, st.refused...) {
			f.Blockers = append(f.Blockers, name+": "+u)
		}
		if healthcheckMissing(st.healthcheck) {
			f.NoHealthMembers = append(f.NoHealthMembers, name)
		}
		if st.restart != "unless-stopped" {
			if f.Restart == nil {
				f.Restart = map[string]string{}
			}
			f.Restart[name] = st.restart
		}
		if p := ct.Labels[composeProjectLabel]; p != "" {
			if f.Compose == nil {
				f.Compose = &adoptComposeInfo{Project: p, ConfigFiles: ct.Labels[composeConfigLabel], WorkingDir: ct.Labels[composeWorkdirLabel], DependsOn: ct.Labels[composeDependsLabel]}
			} else if f.Compose.Project != p {
				f.Blockers = append(f.Blockers, fmt.Sprintf("%s: belongs to compose project %q, others to %q", name, p, f.Compose.Project))
			}
			if s := ct.Labels[composeServiceLabel]; s != "" && !containsString(f.Compose.Services, s) {
				f.Compose.Services = append(f.Compose.Services, s)
			}
		}
		if ct.ID == tpl.ID {
			tplFacts = st
		}
	}
	sort.Strings(f.Members)
	if f.Compose != nil {
		sort.Strings(f.Compose.Services)
	}
	f.HasHealth = !healthcheckMissing(tplFacts.healthcheck)
	var imageEnv []string
	if tplFacts.imageID != "" {
		if imageEnv, err = dc.inspectImageEnv(ctx, tplFacts.imageID); err != nil {
			return f, nil, fmt.Errorf("inspect image of %s: %w", tpl.name(), err)
		}
	}
	env := subtractImageEnv(tplFacts.env, imageEnv)
	for k, v := range env {
		if strings.HasPrefix(v, secretRefPrefix) {
			f.RefLike = append(f.RefLike, k)
		}
	}
	sort.Strings(f.RefLike)
	f.HostLocal = unreachableEnvKeys(envMapToSlice(env))
	if nonce != nil {
		f.Keys = make(map[string]string, len(env))
		for k, v := range env {
			f.Keys[k] = adoptKeyHash(nonce, v)
		}
	}
	image := tpl.Image
	if ref, err := dc.inspectConfigImage(ctx, tpl.ID); err == nil && ref != "" && !looksLikeBareDigest(ref) {
		image = ref
	}
	if tpl.ImageID != "" {
		if id, err := dc.localImageID(ctx, image); err == nil && id != "" && id != tpl.ImageID {
			f.NewerImage = image
		}
	}
	return f, env, nil
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// peerLiveKeys asks one peer for its adopt facts (names + salted hashes).
func (m *envSyncManager) peerLiveKeys(ctx context.Context, peerURL, svc string, nonce []byte) (adoptHostFacts, int, error) {
	var f adoptHostFacts
	path := "/peer/central-env/" + url.PathEscape(svc) + "/live-keys?nonce=" + hex.EncodeToString(nonce)
	code, body, err := peerMutate(ctx, m.client, peerURL, m.secret, http.MethodGet, path, adoptPeerCallTimeout, nil, nil, "")
	if err != nil {
		return f, 0, err
	}
	if code != http.StatusOK {
		return f, code, fmt.Errorf("status %d", code)
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return f, code, errors.New("bad live-keys response")
	}
	return f, code, nil
}

// peerLiveEnv fetches the VALUES of exactly keys from one peer's live
// template. Never logged; the caller verifies them against the preflight's
// salted hashes before using any.
func (m *envSyncManager) peerLiveEnv(ctx context.Context, peerURL, svc string, keys []string) (map[string]string, error) {
	body, _ := json.Marshal(map[string]any{"keys": keys})
	var out struct {
		Values map[string]string `json:"values"`
	}
	code, _, err := peerMutate(ctx, m.client, peerURL, m.secret, http.MethodPost, "/peer/central-env/"+url.PathEscape(svc)+"/live-env", adoptPeerCallTimeout, strings.NewReader(string(body)), &out, "")
	if err != nil {
		// peerMutate's error names the URL, never the body.
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("live-env: status %d", code)
	}
	return out.Values, nil
}

// peerRunsServiceErr is peerRunsService that tells "doesn't run it" from
// "couldn't ask".
func (m *envSyncManager) peerRunsServiceErr(ctx context.Context, peerURL, svc string) (bool, error) {
	var body peerServicesResp
	if err := peerGET(ctx, m.client, peerURL, m.secret, "/peer/services", &body); err != nil {
		return false, err
	}
	for _, s := range body.Services {
		if s.Name == svc {
			return true, nil
		}
	}
	return false, nil
}

// adoptPreflight builds the report for adopting svc onto this host, and the
// in-memory plan an execute needs.
func (m *envSyncManager) adoptPreflight(ctx context.Context, svc string, req centralEnvAdoptRequest, ignoreClaim string) (centralEnvAdoptReport, *adoptPlan, error) {
	me := m.ce.identity
	rep := centralEnvAdoptReport{Service: svc, Origin: me, Blockers: []string{}, Warnings: []string{}, Info: []string{},
		RequiredAcks: []string{}, MissingAcks: []string{}, Unresolved: map[string][]string{},
		Members: map[string][]string{}, Compose: map[string]*adoptComposeInfo{}, RetireSteps: []string{}}
	rep.Env.OriginKeys = []string{}
	rep.Env.Hosts = map[string]*adoptHostDiff{}
	plan := &adoptPlan{nonce: newAdoptNonce(), peers: map[string]adoptHostFacts{}, peerURLs: map[string]string{}}
	block := func(format string, a ...any) { rep.Blockers = append(rep.Blockers, fmt.Sprintf(format, a...)) }
	requireAck := func(ack string) {
		if !containsString(rep.RequiredAcks, ack) {
			rep.RequiredAcks = append(rep.RequiredAcks, ack)
		}
	}

	if infraContainerNames[svc] {
		block("%q is a fixed infrastructure container", svc)
	}
	if self, err := m.dc.serviceContainsSelfByName(ctx, svc); err != nil {
		return rep, nil, err
	} else if self {
		block("%q is this dashboard's own service", svc)
	}
	if m.onb != nil {
		if _, ok := m.onb.Get(svc); ok {
			block("%q has an onboarded record (dual-tracked) — offboard it first; adopt covers label-managed services only", svc)
		}
	}
	local, originEnv, err := adoptLocalFacts(ctx, m, svc, plan.nonce, ignoreClaim)
	if err != nil {
		return rep, nil, err
	}
	plan.originEnv = originEnv
	hosts := map[string]adoptHostFacts{me: local}
	if len(local.Members) == 0 {
		block("no live replica of %q on %s — run adopt on the host that should own its env", svc, me)
	} else {
		if local.Unscalable {
			block("%q is proxy.unscalable — a surge-of-one roll would briefly run two copies", svc)
		}
		if !local.HasHost {
			block("%q has no proxy.host label", svc)
		}
	}
	if m.ce.store.Has(svc) || local.Managed {
		block("%q is already centrally managed", svc)
	}

	// Peers. A configured peer that can't be asked is a blocker: the next
	// reconcile would otherwise half-adopt it with no preflight at all.
	if m.registry != nil {
		status := m.registry.Status()
		// Peers(), not Status(): a peer never reached since startup has no
		// status entry at all.
		for _, u := range m.registry.Peers() {
			if status[u].Identity == "" {
				block("peer %s has not completed a handshake — cannot tell whether it runs %q", u, svc)
			}
		}
	}
	if m.secret != "" {
		for _, p := range m.peers() {
			switch {
			case !p.capable:
				runs, err := m.peerRunsServiceErr(ctx, p.url, svc)
				if err != nil {
					block("%s is unreachable — cannot tell whether it runs %q", p.identity, svc)
				} else if runs {
					block("%s runs %q but does not advertise %s", p.identity, svc, centralEnvFeature)
				}
				continue
			case !p.adoptCapable:
				ps, code, err := m.peerStatus(ctx, p.url, svc)
				if err != nil && code != http.StatusNotFound {
					block("%s is unreachable — cannot tell whether it runs %q", p.identity, svc)
				} else if err == nil && ps.runsOrCaches() {
					block("%s runs %q but cannot take part in an adopt (upgrade its dashboard to one advertising %s)", p.identity, svc, centralEnvAdoptFeature)
				}
				continue
			}
			f, code, err := m.peerLiveKeys(ctx, p.url, svc, plan.nonce)
			if err != nil {
				if code == http.StatusNotFound {
					block("%s refused the adopt check for %q (is -peer-writes on there?)", p.identity, svc)
				} else {
					block("%s is unreachable — cannot tell whether it runs %q", p.identity, svc)
				}
				continue
			}
			f.Identity = p.identity
			if f.Managed {
				block("%s already has a central env (or stamped replicas) for %q", p.identity, svc)
			}
			if len(f.Members) == 0 {
				continue
			}
			hosts[p.identity] = f
			plan.peers[p.identity] = f
			plan.peerURLs[p.identity] = p.url
		}
	}

	hostIDs := make([]string, 0, len(hosts))
	for h := range hosts {
		hostIDs = append(hostIDs, h)
	}
	sort.Strings(hostIDs)
	originHasHealth := local.HasHealth || local.HealthLabel
	for _, h := range hostIDs {
		f := hosts[h]
		if len(f.Members) > 0 {
			rep.Members[h] = f.Members
		}
		for _, b := range f.Blockers {
			block("%s: cannot recreate %s", h, b)
		}
		if f.Canary {
			block("%s: %q has a staged canary — promote or discard it first", h, svc)
		}
		if f.Busy != "" {
			block("%s: %q is busy (%s)", h, svc, f.Busy)
		}
		if len(f.RefLike) > 0 {
			block("%s: live value(s) of %s start with %q and would be read as secret references", h, strings.Join(f.RefLike, ", "), secretRefPrefix)
		}
		if h != me && len(f.NoHealthMembers) > 0 && originHasHealth && local.HasHealth {
			rep.Info = append(rep.Info, fmt.Sprintf("%s: no healthcheck on %s — the adopt roll gives them %s's", h, strings.Join(f.NoHealthMembers, ", "), me))
		}
		if len(f.Restart) > 0 {
			names := sortedKeys(f.Restart)
			var parts []string
			for _, n := range names {
				parts = append(parts, fmt.Sprintf("%s=%s", n, orNone(f.Restart[n])))
			}
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: restart policy %s is normalized to unless-stopped", h, strings.Join(parts, ", ")))
			requireAck(adoptAckRestart)
		}
		if f.NewerImage != "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: local tag %s points to a newer pulled image than the running replicas — the adopt roll moves them onto it", h, f.NewerImage))
			requireAck(adoptAckImageUpdate)
		}
		if f.Compose != nil {
			rep.Compose[h] = f.Compose
			if h == me {
				plan.compose = f.Compose
			}
			rep.Warnings = append(rep.Warnings, composeOwnedWarning(h, svc, f.Compose))
			rep.RetireSteps = append(rep.RetireSteps, composeRetireSteps(h, f.Compose)...)
			requireAck(adoptAckCompose)
		}
	}
	if len(local.Members) > 0 && !originHasHealth {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s has no Docker healthcheck and no proxy.health on %s — the adopt roll (and every later env roll) can only gate on the container staying up", svc, me))
		requireAck(adoptAckNoHealth)
	}

	m.adoptEnvDiff(&rep, plan, req, block)

	sort.Strings(rep.RequiredAcks)
	for _, a := range rep.RequiredAcks {
		if !req.acked(a) {
			rep.MissingAcks = append(rep.MissingAcks, a)
		}
	}
	rep.Fingerprint = adoptFingerprint(rep)
	rep.Adoptable = len(rep.Blockers) == 0 && len(rep.MissingAcks) == 0 && len(rep.Unresolved) == 0
	return rep, plan, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// adoptEnvDiff fills the per-host key diff and validates the request's
// imports / accept_dropped against it. Values are read only from the
// origin's own env (for the unreachable check); peers are compared purely
// by salted hash.
func (m *envSyncManager) adoptEnvDiff(rep *centralEnvAdoptReport, plan *adoptPlan, req centralEnvAdoptRequest, block func(string, ...any)) {
	me := m.ce.identity
	origin := plan.originEnv
	originHash := make(map[string]string, len(origin))
	for k, v := range origin {
		originHash[k] = adoptKeyHash(plan.nonce, v)
	}
	rep.Env.OriginKeys = sortedKeys(origin)

	accepted := map[string]bool{}
	for _, k := range req.AcceptDropped {
		accepted[k] = true
	}
	// Base imports: key → (source host, hash). Override imports: host → keys.
	baseFrom := map[string]string{}
	overrideKeys := map[string]map[string]bool{}
	importHosts := make([]string, 0, len(req.Import))
	for h := range req.Import {
		importHosts = append(importHosts, h)
	}
	sort.Strings(importHosts)
	for _, h := range importHosts {
		imp := req.Import[h]
		f, ok := plan.peers[h]
		if !ok {
			block("import from %s: it is not a peer running %q", h, rep.Service)
			continue
		}
		if imp.As != adoptImportBase && imp.As != adoptImportOverride {
			block("import from %s: as must be %q or %q", h, adoptImportBase, adoptImportOverride)
			continue
		}
		for _, k := range imp.Keys {
			hk, has := f.Keys[k]
			switch {
			case !has:
				block("import from %s: it has no %s", h, k)
			case imp.As == adoptImportBase && originHash[k] != "":
				block("import %s from %s as base: %s already has it — import it as override to keep %s's value there only", k, h, me, h)
			case imp.As == adoptImportBase:
				if src, dup := baseFrom[k]; dup && plan.peers[src].Keys[k] != hk {
					block("import %s as base from both %s and %s, whose values differ — pick one, or import as override", k, src, h)
				} else if !dup {
					baseFrom[k] = h
				}
			default:
				if overrideKeys[h] == nil {
					overrideKeys[h] = map[string]bool{}
				}
				overrideKeys[h][k] = true
			}
		}
	}
	for k, src := range baseFrom {
		if containsString(plan.peers[src].HostLocal, k) {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s imported from %s looks host-local (unreachable from %s)", k, src, me))
		}
	}

	peerIDs := make([]string, 0, len(plan.peers))
	for h := range plan.peers {
		peerIDs = append(peerIDs, h)
	}
	sort.Strings(peerIDs)
	for _, h := range peerIDs {
		f := plan.peers[h]
		d := &adoptHostDiff{PeerOnly: []string{}, OriginOnly: []string{}, Differs: []string{}, Unreachable: []string{}}
		for _, k := range sortedKeys(f.Keys) {
			oh, inOrigin := originHash[k]
			switch {
			case !inOrigin:
				d.PeerOnly = append(d.PeerOnly, k)
			case oh != f.Keys[k]:
				d.Differs = append(d.Differs, k)
			}
		}
		for _, k := range rep.Env.OriginKeys {
			if _, ok := f.Keys[k]; !ok {
				d.OriginOnly = append(d.OriginOnly, k)
			}
		}
		// What this host would actually run: the origin's env, minus what it
		// keeps as its own override, plus base imports from other hosts.
		effective := map[string]string{}
		for k, v := range origin {
			if !overrideKeys[h][k] {
				effective[k] = v
			}
		}
		d.Unreachable = unreachableEnvKeys(envMapToSlice(effective))
		for k, src := range baseFrom {
			if src != h && !overrideKeys[h][k] && containsString(plan.peers[src].HostLocal, k) && !containsString(d.Unreachable, k) {
				d.Unreachable = append(d.Unreachable, k)
			}
		}
		if d.Unreachable == nil {
			d.Unreachable = []string{}
		}
		sort.Strings(d.Unreachable)
		rep.Env.Hosts[h] = d

		for _, k := range d.PeerOnly {
			if overrideKeys[h][k] {
				continue
			}
			if src, ok := baseFrom[k]; ok {
				if src != h && plan.peers[src].Keys[k] != f.Keys[k] {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s differs on %s: the value imported from %s wins unless %s imports it as override", k, h, src, h))
				}
				continue
			}
			if accepted[k] {
				rep.Info = append(rep.Info, fmt.Sprintf("%s will be dropped from %s (accepted)", k, h))
				continue
			}
			rep.Unresolved[h] = append(rep.Unresolved[h], k)
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s will be dropped from %s — import it (as base or override) or list it in accept_dropped", k, h))
		}
		for _, k := range d.Differs {
			if !overrideKeys[h][k] {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s differs on %s: %s's value wins unless imported as override", k, h, me))
			}
		}
		var added []string
		added = append(added, d.OriginOnly...)
		for k, src := range baseFrom {
			if src != h {
				if _, has := f.Keys[k]; !has {
					added = append(added, k)
				}
			}
		}
		sort.Strings(added)
		if len(added) > 0 {
			rep.Info = append(rep.Info, fmt.Sprintf("%s will be added to %s", strings.Join(added, ", "), h))
		}
		if creds := credentialEnvKeys(envKeysOnly(added)); len(creds) > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s will be added to %s — a second live copy of a credential can cause duplicate side effects (e.g. a bot answering every event twice)", strings.Join(creds, ", "), h))
		}
		if len(d.Unreachable) > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s on %s look host-local to %s and may be unreachable there — consider a per-host override", strings.Join(d.Unreachable, ", "), h, me))
		}
	}
	for _, k := range req.AcceptDropped {
		found := false
		for _, d := range rep.Env.Hosts {
			if containsString(d.PeerOnly, k) {
				found = true
			}
		}
		if !found {
			rep.Info = append(rep.Info, fmt.Sprintf("accept_dropped: %s is not peer-only anywhere (ignored)", k))
		}
	}
}

func envKeysOnly(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "="
	}
	return out
}

func composeOwnedWarning(host, svc string, c *adoptComposeInfo) string {
	w := fmt.Sprintf("compose_owned on %s: %q is run by compose project %q (config %s, working dir %s). "+
		"Adopt strips the compose labels from every replica it recreates. Afterwards NEVER run `docker compose up`, `down` or `pull` "+
		"for %s in that project: up would create unmanaged duplicates in the same proxy.service pool, and down could remove the "+
		"project network the adopted containers still use. Retire only the service block(s) for %s from the compose file — do NOT "+
		"delete the project directory if other services (e.g. a bot) or the .env still live there.",
		host, svc, c.Project, orNone(c.ConfigFiles), orNone(c.WorkingDir), strings.Join(c.Services, ", "), strings.Join(c.Services, ", "))
	if c.DependsOn != "" {
		w += " Its compose depends_on ordering (" + c.DependsOn + ") is lost: the dashboard does not order starts."
	}
	return w
}

func composeRetireSteps(host string, c *adoptComposeInfo) []string {
	svcs := strings.Join(c.Services, ", ")
	return []string{
		fmt.Sprintf("%s: once the adopt converged, remove the service block(s) for %s from %s (leave every other service and the .env in place)", host, svcs, orNone(c.ConfigFiles)),
		fmt.Sprintf("%s: keep the project network in the compose file while any other service or adopted replica still uses it; never `docker compose down` project %q", host, c.Project),
	}
}

// adoptFingerprint hashes the observed part of a report.
func adoptFingerprint(rep centralEnvAdoptReport) string {
	hosts := map[string][3][]string{}
	for h, d := range rep.Env.Hosts {
		hosts[h] = [3][]string{d.PeerOnly, d.OriginOnly, d.Differs}
	}
	// Request-derived blockers (bad imports) don't belong in it; they're
	// all prefixed "import ".
	var observed []string
	for _, b := range rep.Blockers {
		if !strings.HasPrefix(b, "import ") {
			observed = append(observed, b)
		}
	}
	data, _ := json.Marshal(struct {
		Members  map[string][]string
		Keys     []string
		Hosts    map[string][3][]string
		Compose  map[string]*adoptComposeInfo
		Acks     []string
		Blockers []string
	}{rep.Members, rep.Env.OriginKeys, hosts, rep.Compose, rep.RequiredAcks, observed})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// adoptBuildRecord turns a passed preflight + request into the v1 content:
// the origin's env as base, imports fetched from their peers now — and
// verified against the preflight's salted hashes, so a value that changed
// since is refused rather than captured.
func (m *envSyncManager) adoptBuildRecord(ctx context.Context, svc string, plan *adoptPlan, req centralEnvAdoptRequest) (map[string]string, map[string]map[string]string, error) {
	base := copyStringMap(plan.originEnv)
	if base == nil {
		base = map[string]string{}
	}
	var overrides map[string]map[string]string
	hosts := make([]string, 0, len(req.Import))
	for h := range req.Import {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		imp := req.Import[h]
		if len(imp.Keys) == 0 {
			continue
		}
		vals, err := m.peerLiveEnv(ctx, plan.peerURLs[h], svc, imp.Keys)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch imported keys from %s: %w", h, err)
		}
		for _, k := range imp.Keys {
			v, ok := vals[k]
			if !ok || adoptKeyHash(plan.nonce, v) != plan.peers[h].Keys[k] {
				return nil, nil, fmt.Errorf("%s's value of %s changed since the check — run the dry run again", h, k)
			}
			if imp.As == adoptImportBase {
				if _, taken := base[k]; !taken {
					base[k] = v
				}
				continue
			}
			if overrides == nil {
				overrides = map[string]map[string]string{}
			}
			if overrides[h] == nil {
				overrides[h] = map[string]string{}
			}
			overrides[h][k] = v
		}
	}
	return base, overrides, nil
}

// adoptedFromLabel records where an adopted service came from.
func adoptedFromLabel(c *adoptComposeInfo) string {
	if c == nil {
		return "labels"
	}
	s := "compose:" + c.Project
	if c.ConfigFiles != "" {
		s += " (" + c.ConfigFiles + ")"
	}
	return s
}

// errAdoptRefused is an execute that didn't pass its re-check; the API
// returns the fresh report with it.
type errAdoptRefused struct {
	reason string
	report centralEnvAdoptReport
}

func (e errAdoptRefused) Error() string { return e.reason }

// centralEnvAdoptResult is an accepted execute. Names only.
type centralEnvAdoptResult struct {
	Status       string              `json:"status"`
	Service      string              `json:"service"`
	Origin       string              `json:"origin"`
	Version      uint64              `json:"version"`
	RequestID    string              `json:"request_id"`
	Replayed     bool                `json:"replayed,omitempty"`
	BaseKeys     []string            `json:"base_keys,omitempty"`
	OverrideKeys map[string][]string `json:"override_keys,omitempty"`
	Job          string              `json:"job"`
}

// adoptExecute re-runs the preflight under svc's claim, refuses if anything
// moved since the dry run (fingerprint) or anything is unacknowledged, then
// seeds the record and hands the claim straight to the propagation job.
func (m *envSyncManager) adoptExecute(ctx context.Context, r *http.Request, svc string, req centralEnvAdoptRequest, actor string) (centralEnvAdoptResult, error) {
	res := centralEnvAdoptResult{Status: "adopting", Service: svc, Origin: m.ce.identity, RequestID: req.RequestID,
		Job: "GET /api/services/" + svc + "/env"}
	if req.RequestID != "" && m.ce.store.HasRequestID(svc, req.RequestID) {
		rec, _, _ := m.ce.store.Get(svc)
		res.Version, res.Replayed = rec.Version, true
		res.BaseKeys, _, res.OverrideKeys = centralEnvNames(rec)
		return res, nil
	}
	if req.Fingerprint == "" {
		return res, errAdoptRefused{reason: "fingerprint is required — run the dry run first and pass its fingerprint"}
	}
	if !m.dc.claims.tryClaim(svc, adoptClaimOwner) {
		return res, errAdoptRefused{reason: fmt.Sprintf("%q is busy (claimed by %s) — try again when it finishes", svc, m.dc.claims.holder(svc))}
	}
	handed := false
	defer func() {
		if !handed {
			m.dc.claims.release(svc, adoptClaimOwner)
		}
	}()
	rep, plan, err := m.adoptPreflight(ctx, svc, req, adoptClaimOwner)
	if err != nil {
		return res, err
	}
	if rep.Fingerprint != req.Fingerprint {
		return res, errAdoptRefused{reason: "the service changed since the dry run — review the new report and retry with its fingerprint", report: rep}
	}
	if !rep.Adoptable {
		return res, errAdoptRefused{reason: "not adoptable — see blockers, missing_acks and unresolved", report: rep}
	}
	base, overrides, err := m.adoptBuildRecord(ctx, svc, plan, req)
	if err != nil {
		return res, errAdoptRefused{reason: err.Error(), report: rep}
	}
	v, err := m.ce.store.CreateAdopted(svc, m.ce.identity, base, overrides, actor, adoptedFromLabel(plan.compose), req.RequestID)
	if err != nil {
		return res, err
	}
	res.Version = v
	res.BaseKeys = sortedKeys(base)
	for h, kv := range overrides {
		if res.OverrideKeys == nil {
			res.OverrideKeys = map[string][]string{}
		}
		res.OverrideKeys[h] = sortedKeys(kv)
	}
	var ov []string
	for h, ks := range res.OverrideKeys {
		ov = append(ov, h+":"+strings.Join(ks, "+"))
	}
	sort.Strings(ov)
	audit(r, actor, "service.env_adopt", fmt.Sprintf("%s v%d base=%d keys=%s overrides=%s from=%s", svc, v, len(base), strings.Join(res.BaseKeys, ","), strings.Join(ov, ";"), adoptedFromLabel(plan.compose)))
	m.dc.claims.transfer(svc, adoptClaimOwner, envSyncClaimOwner)
	handed = true
	m.request(svc)
	return res, nil
}

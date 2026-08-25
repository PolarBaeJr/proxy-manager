// Onboarded services — containers that didn't start with proxy.* labels but
// the user clicked "Onboard" on. We track them in /data/onboarded.json so
// the dashboard remembers (a) which containers it adopted, (b) the template
// image + env needed to clone replicas, and (c) the route entry it wrote to
// routes.json so cleanup can revert it.
//
// This is NOT the same as label-managed services in docker.go. Label-managed
// services have `proxy.service=<name>` labels and are discovered every render
// from `docker ps`. Onboarded services are discovered from this JSON file.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultOnboardedFile = "/data/onboarded.json"
	defaultRoutesFile    = "/etc/proxy/routes.json"
	defaultProxyURL      = "http://proxy:8094"
	onboardedNetwork     = "edge"
)

// OnboardedService is one service the dashboard adopted from an unlabelled
// container. The Template fields are captured at onboard time so replicas can
// be cloned later even if the original is destroyed.
type OnboardedService struct {
	Name string `json:"name"` // logical service name (matches container)
	// Host is the proxy.host equivalent. An EMPTY Host means the service is
	// managed-only: adopted for lifecycle/image tracking, with no route, not
	// on the edge network, and not scalable/replaceable. Adding a route later
	// (via onboardContainer) upgrades it to a full onboarded service in place.
	Host           string            `json:"host"`
	Port           int               `json:"port"` // internal container port
	Path           string            `json:"path,omitempty"`
	Strip          bool              `json:"strip,omitempty"`
	Image          string            `json:"image"`                    // currently-live image (clones use this)
	Env            []string          `json:"env,omitempty"`            // captured at onboard time
	Labels         map[string]string `json:"labels,omitempty"`         // original labels we want to preserve
	Replicas       int               `json:"replicas"`                 // currently routed backend count (>= 1)
	PreviousImage  string            `json:"previous_image,omitempty"` // set on replace/promote — for rollback
	AutoUpdate     bool              `json:"auto_update,omitempty"`    // opted in to unattended updates
	CanaryImage    string            `json:"canary_image,omitempty"`   // non-empty while a canary is staged
	CanaryReplicas int               `json:"canary_replicas,omitempty"`
	// OriginalRouted flips to false once a replace/promote drops the original
	// from the route. The original container keeps running; we just stop
	// referring to it as a backend. Used so route-rebuild knows whether to
	// include http://<name>:<port> as the first backend.
	OriginalRouted bool  `json:"original_routed"`
	CreatedAt      int64 `json:"created_at"`
}

type OnboardedStore struct {
	path  string
	mu    sync.RWMutex
	items []OnboardedService
}

func loadOnboardedStore(path string) (*OnboardedStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &OnboardedStore{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.items)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *OnboardedStore) save() error {
	b, _ := json.MarshalIndent(s.items, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *OnboardedStore) List() []OnboardedService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]OnboardedService, len(s.items))
	copy(out, s.items)
	return out
}

func (s *OnboardedStore) Get(name string) (OnboardedService, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.items {
		if it.Name == name {
			return it, true
		}
	}
	return OnboardedService{}, false
}

func (s *OnboardedStore) Put(svc OnboardedService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].Name == svc.Name {
			s.items[i] = svc
			return s.save()
		}
	}
	s.items = append(s.items, svc)
	return s.save()
}

func (s *OnboardedStore) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.items {
		if it.Name == name {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return s.save()
		}
	}
	return nil
}

// SetReplicas just updates the count after a successful scale.
func (s *OnboardedStore) SetReplicas(name string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].Name == name {
			s.items[i].Replicas = n
			return s.save()
		}
	}
	return fmt.Errorf("not onboarded: %s", name)
}

// SetAutoUpdate flips the unattended-update opt-in for one service.
func (s *OnboardedStore) SetAutoUpdate(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].Name == name {
			s.items[i].AutoUpdate = enabled
			return s.save()
		}
	}
	return fmt.Errorf("not onboarded: %s", name)
}

// ---- routes.json read/write (the file the proxy reads on refresh) ----

type routesFile struct {
	Routes []routesEntry `json:"routes"`
}
type routesEntry struct {
	Name     string   `json:"name,omitempty"`
	Host     string   `json:"host"`
	Path     string   `json:"path,omitempty"`
	Strip    bool     `json:"strip,omitempty"`
	Backends []string `json:"backends"`
	Health   string   `json:"health,omitempty"`
	// Marker: routes the dashboard wrote, so we can rewrite/delete them
	// without touching user-curated entries.
	Onboarded string `json:"onboarded,omitempty"` // matches OnboardedService.Name
}

func readRoutesFile(path string) (routesFile, error) {
	var f routesFile
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

func writeRoutesFile(path string, f routesFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// upsertOnboardedRoute rewrites the routes.json entry tagged onboarded=<name>.
// If no such entry exists, appends one. Returns the new full file.
func upsertOnboardedRoute(path, name, host, routePath string, strip bool, backends []string) error {
	f, err := readRoutesFile(path)
	if err != nil {
		return err
	}
	entry := routesEntry{
		Name:      "onboarded: " + name,
		Host:      host,
		Path:      routePath,
		Strip:     strip,
		Backends:  backends,
		Onboarded: name,
	}
	for i, r := range f.Routes {
		if r.Onboarded == name {
			f.Routes[i] = entry
			return writeRoutesFile(path, f)
		}
	}
	f.Routes = append(f.Routes, entry)
	return writeRoutesFile(path, f)
}

func removeOnboardedRoute(path, name string) error {
	f, err := readRoutesFile(path)
	if err != nil {
		return err
	}
	out := f.Routes[:0]
	for _, r := range f.Routes {
		if r.Onboarded != name {
			out = append(out, r)
		}
	}
	f.Routes = out
	return writeRoutesFile(path, f)
}

// ---- Docker network connect/disconnect (extends dockerClient) ----

func (c *dockerClient) connectToEdge(ctx context.Context, containerID string) error {
	body := map[string]any{"Container": containerID}
	resp, err := c.do(ctx, "POST", "/networks/"+onboardedNetwork+"/connect", body)
	if err != nil {
		// Already connected is a 403 — non-fatal for us.
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	resp.Close()
	return nil
}

func (c *dockerClient) disconnectFromEdge(ctx context.Context, containerID string) error {
	body := map[string]any{"Container": containerID, "Force": true}
	resp, err := c.do(ctx, "POST", "/networks/"+onboardedNetwork+"/disconnect", body)
	if err != nil {
		if strings.Contains(err.Error(), "is not connected") {
			return nil
		}
		return err
	}
	resp.Close()
	return nil
}

// offboardLabelManaged is the general soft-remove for any label-managed
// service: disconnect every member from the edge network so
// assembleGroups' next refresh has no IP to route to, WITHOUT stopping or
// deleting the container(s) — they keep running, keep their proxy.* labels,
// and keep serving whatever they'd otherwise serve on other networks.
// Reversible: reconnecting to edge (or a fresh `docker compose up -d`, which
// reasserts the network) resumes routing.
func (c *dockerClient) offboardLabelManaged(ctx context.Context, name string) error {
	existing, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found", name)
	}
	for _, ct := range existing {
		if err := c.disconnectFromEdge(ctx, ct.ID); err != nil {
			return fmt.Errorf("disconnect %s: %w", ct.name(), err)
		}
	}
	return nil
}

// ---- Onboarding flow ----

// errOnboardRefused is returned by checkOnboardTarget when the resolved
// target must not be destroyed by onboarding: the dashboard's own container,
// or one of the fixed infra containers (proxy, in particular — destroying it
// takes down routing for every service on the host, not just the
// dashboard's own reachability).
var errOnboardRefused = errors.New("refusing to onboard this container")

// checkOnboardTarget refuses to onboard the dashboard's own container (by
// identity, not label — onboard's targets never carry proxy.* labels yet,
// since they haven't been onboarded, so the label-based
// serviceContainsSelfByName check used elsewhere wouldn't work here) or any
// of the fixed infra containers named in infraContainerNames.
//
// Fails CLOSED on a hostname-lookup error — deliberately does NOT reuse
// isSelfContainer's own comparison as-is, because isSelfContainer fails OPEN
// (returns false, i.e. "not self") on a selfHostname() error, which is
// correct for its existing callers (they only filter a display/action list
// where "not self" just means "still shown/actionable", and other guards
// layered around them, like this one, exist for exactly this kind of case).
// Here the same "can't verify" outcome must mean refuse: onboardContainer
// stops and removes the original container after recreating replicas, with
// no live re-read afterward that could rescue a wrong "not self"
// determination. isSelfContainer itself is left untouched — other callers
// may correctly depend on its current fail-open semantics.
func checkOnboardTarget(ct dockerContainer) error {
	name := ct.name()
	if infraContainerNames[name] {
		return fmt.Errorf("%q is a fixed infrastructure container: %w", name, errOnboardRefused)
	}
	hostname, err := selfHostname()
	if err != nil {
		return fmt.Errorf("could not verify container identity: %v: %w", err, errOnboardRefused)
	}
	h := strings.TrimSpace(hostname)
	if h == "" {
		return fmt.Errorf("could not verify container identity (empty hostname): %w", errOnboardRefused)
	}
	if ct.ID == h || strings.HasPrefix(ct.ID, h) {
		return fmt.Errorf("%q is the dashboard's own container: %w", name, errOnboardRefused)
	}
	return nil
}

type OnboardRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Path     string `json:"path,omitempty"`
	Strip    bool   `json:"strip,omitempty"`
	Replicas int    `json:"replicas,omitempty"`
}

// onboardContainer relabels-and-recreates an existing (typically unlabelled)
// container as N ordinary label-managed replicas: capture image/env/mounts,
// build proxy.* labels from req, create + start the replacements, then stop
// and remove the original. The new containers are indistinguishable from any
// docker-compose-created labeled service — assembleGroups discovers them on
// the next refresh like any other. No OnboardedStore record, no routes.json
// entry: this container is now fully label-managed.
//
// Destructive by design: unlike the old track-in-place onboarding, this
// recreates the container. inspectHostConfigUnknowns runs first specifically
// so a container with config a recreate can't reproduce (extra port
// bindings, capabilities, a second docker network, ...) is refused rather
// than silently downgraded.
func (c *dockerClient) onboardContainer(ctx context.Context, name string, req OnboardRequest) error {
	if req.Host == "" || req.Port <= 0 {
		return fmt.Errorf("host and port are required")
	}
	if !validServiceName(name) {
		return fmt.Errorf("invalid container name (allowed: a-z A-Z 0-9 . _ -, max 63 chars)")
	}
	if !validHostname(req.Host) {
		return fmt.Errorf("invalid hostname (allowed: a-z A-Z 0-9 . -, max 253 chars)")
	}
	if !validPort(req.Port) {
		return fmt.Errorf("invalid port")
	}
	if !validRoutePath(req.Path) {
		return fmt.Errorf("invalid path (must be empty or start with / — allowed: a-z A-Z 0-9 / _ . -, max 512 chars)")
	}
	if req.Replicas < 1 {
		req.Replicas = 1
	}
	// Look up the existing container — must be running.
	containers, err := c.listAll(ctx, fmt.Sprintf(`{"name":["%s"]}`, name))
	if err != nil {
		return err
	}
	var ct *dockerContainer
	for i := range containers {
		if containers[i].name() == name {
			ct = &containers[i]
			break
		}
	}
	if ct == nil {
		return fmt.Errorf("container %q not found", name)
	}
	if err := checkOnboardTarget(*ct); err != nil {
		return err
	}

	// Fail closed: refuse rather than silently drop any HostConfig/Config/
	// network config a recreate has no way to reproduce. Deliberately does
	// NOT check compose-managed origin — onboarding must work regardless of
	// a container's origin.
	unknowns, err := c.inspectHostConfigUnknowns(ctx, ct.ID)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", name, err)
	}
	if len(unknowns) > 0 {
		return fmt.Errorf("refusing to onboard %q: would drop %s — resolve manually first", name, strings.Join(unknowns, ", "))
	}

	// Capture image, env, and mounts so the replacements are equivalent.
	image := ct.Image
	env, err := c.inspectEnv(ctx, ct.ID)
	if err != nil {
		return fmt.Errorf("inspect env: %w", err)
	}
	clone, err := c.inspectCloneSpec(ctx, ct.ID)
	if err != nil {
		return fmt.Errorf("inspect clone spec: %w", err)
	}

	labels := map[string]string{
		labelEnable:  "true",
		labelHost:    req.Host,
		labelPort:    strconv.Itoa(req.Port),
		labelService: name,
		labelName:    name,
	}
	if req.Path != "" {
		labels[labelPath] = req.Path
	}
	if req.Strip {
		labels[labelStrip] = "true"
	}

	var newIDs []string
	for i := 1; i <= req.Replicas; i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", name, i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image: image, Labels: labels, Env: env, HostConfig: hostConfig{Mounts: clone.Mounts},
		})
		if err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("start %s: %w", cname, err)
		}
		newIDs = append(newIDs, id)
	}

	// Give the new containers a few seconds to bind their ports / accept
	// connections before we tear down the original.
	time.Sleep(replaceSettleDelay)

	if project := ct.Labels["com.docker.compose.project"]; project != "" {
		composeFile := ct.Labels["com.docker.compose.project.config_files"]
		log.Printf("onboarded %q away from compose — %s still defines this service; remove or comment that block or a future 'docker compose up -d' will recreate an unlabeled duplicate", name, composeFile)
	}
	_ = c.stopContainer(ctx, ct.ID)
	if err := c.removeContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("onboard %q: new replicas are running but failed to remove the original %s: %w", name, ct.name(), err)
	}
	return nil
}

// onboardManagedOnly adopts an existing container for lifecycle/image tracking
// only: it appears in Services and the Images panel and can be offboarded, but
// gets NO route, NO edge-network connect, and NO env capture. Routing a member
// stays a separate explicit action (onboardContainer) afterward.
func onboardManagedOnly(ctx context.Context, name string, c *dockerClient, store *OnboardedStore) error {
	if !validServiceName(name) {
		return fmt.Errorf("invalid container name (allowed: a-z A-Z 0-9 . _ -, max 63 chars)")
	}
	containers, err := c.listAll(ctx, fmt.Sprintf(`{"name":["%s"]}`, name))
	if err != nil {
		return err
	}
	var ct *dockerContainer
	for i := range containers {
		if containers[i].name() == name {
			ct = &containers[i]
			break
		}
	}
	if ct == nil {
		return fmt.Errorf("container %q not found", name)
	}
	if err := checkOnboardTarget(*ct); err != nil {
		return err
	}
	return store.Put(OnboardedService{
		Name:           name,
		Image:          ct.Image,
		Replicas:       1,
		OriginalRouted: false,
		CreatedAt:      time.Now().Unix(),
	})
}

// scaleOnboarded clones the template image+env into N-1 additional containers
// named <name>-r<index>, rewrites the route's backend list, and updates the
// store. Scale-down removes the highest-numbered clones first. The ORIGINAL
// container is never removed by scale-down.
func (c *dockerClient) scaleOnboarded(ctx context.Context, name string, desired int, store *OnboardedStore, routesPath string) error {
	svc, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("not an onboarded service")
	}
	if svc.Host == "" {
		return fmt.Errorf("%q is managed-only (no route) — manage it via docker compose", name)
	}
	if desired < 1 {
		return fmt.Errorf("onboarded services must keep at least the original (>= 1)")
	}
	// Find all current containers: the original + any goproxy-onb-<name>-N.
	cloneFilter := fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name)
	clones, err := c.listAll(ctx, cloneFilter)
	if err != nil {
		return err
	}
	// Filter out actively-staged canary containers from the scale math.
	// Once promote ran, CanaryImage clears and the c-prefixed container IS
	// the live one — count it as a regular clone.
	if svc.CanaryImage != "" {
		nonCanary := clones[:0:0]
		for _, cl := range clones {
			if !strings.HasPrefix(cl.name(), fmt.Sprintf("goproxy-onb-%s-c", name)) {
				nonCanary = append(nonCanary, cl)
			}
		}
		clones = nonCanary
	}
	originalCount := 0
	if svc.OriginalRouted {
		originalCount = 1
	}
	current := originalCount + len(clones)
	if current == desired {
		_ = store.SetReplicas(name, desired)
		return rebuildOnboardedRoute(ctx, c, name, svc, routesPath)
	}
	if desired > current {
		needed := desired - current
		next := nextCloneIndex(clones, name)
		for i := 0; i < needed; i++ {
			cname := fmt.Sprintf("goproxy-onb-%s-%d", name, next+i)
			id, err := c.createContainer(ctx, cname, createBody{
				Image: svc.Image, Env: svc.Env,
			})
			if err != nil {
				return fmt.Errorf("create %s: %w", cname, err)
			}
			if err := c.startContainer(ctx, id); err != nil {
				return fmt.Errorf("start %s: %w", cname, err)
			}
		}
	} else {
		// Scale down: remove highest-numbered clones until count matches.
		toRemove := current - desired
		// Sort clones by name descending (highest first).
		sortByNameDesc(clones)
		if len(clones) < toRemove {
			return fmt.Errorf("can only scale down to %d (one is the original)", 1)
		}
		for i := 0; i < toRemove; i++ {
			_ = c.stopContainer(ctx, clones[i].ID)
			if err := c.removeContainer(ctx, clones[i].ID); err != nil {
				return fmt.Errorf("remove %s: %w", clones[i].name(), err)
			}
		}
	}
	_ = store.SetReplicas(name, desired)
	// Re-fetch svc to include updated replica count.
	if s2, ok := store.Get(name); ok {
		svc = s2
	}
	return rebuildOnboardedRoute(ctx, c, name, svc, routesPath)
}

// rebuildOnboardedRoute rewrites routes.json for this service from current
// state: original (if still routed) + live clones + canaries (if any).
//
// A container's name starting with goproxy-onb-<name>-c is treated as a
// CANARY only while svc.CanaryImage is set. Once the canary is promoted,
// CanaryImage clears and the same container is treated as a regular live
// backend — it IS the new live, just retained its original name.
func rebuildOnboardedRoute(ctx context.Context, c *dockerClient, name string, svc OnboardedService, routesPath string) error {
	if svc.Host == "" {
		return removeOnboardedRoute(routesPath, name)
	}
	backends := []string{}
	if svc.OriginalRouted {
		backends = append(backends, fmt.Sprintf("http://%s:%d", name, svc.Port))
	}
	clones, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name))
	if err != nil {
		return err
	}
	canaryActive := svc.CanaryImage != ""
	cPrefix := fmt.Sprintf("goproxy-onb-%s-c", name)
	// First pass: non-canary live backends.
	for _, cl := range clones {
		if canaryActive && strings.HasPrefix(cl.name(), cPrefix) {
			continue
		}
		backends = append(backends, fmt.Sprintf("http://%s:%d", cl.name(), svc.Port))
	}
	// Second pass: canary backends (only while a canary is actively staged).
	if canaryActive {
		for _, cl := range clones {
			if strings.HasPrefix(cl.name(), cPrefix) {
				backends = append(backends, fmt.Sprintf("http://%s:%d", cl.name(), svc.Port))
			}
		}
	}
	if len(backends) == 0 {
		return removeOnboardedRoute(routesPath, name)
	}
	return upsertOnboardedRoute(routesPath, name, svc.Host, svc.Path, svc.Strip, backends)
}

// stageOnboarded spins up N=Replicas canary containers with new image+env and
// adds them to the route alongside the original + existing clones. Proxy then
// round-robins across all backends until promote or discard.
func (c *dockerClient) stageOnboarded(ctx context.Context, name string, req ReplaceServiceRequest, store *OnboardedStore, routesPath string) error {
	svc, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("not onboarded: %s", name)
	}
	if svc.Host == "" {
		return fmt.Errorf("%q is managed-only (no route) — manage it via docker compose", name)
	}
	if req.Image == "" {
		return fmt.Errorf("image is required")
	}
	if svc.CanaryImage != "" {
		return fmt.Errorf("%q already has a canary — promote or discard first", name)
	}
	edits, refs, err := resolveSecretRefs(name, req.Env, c.secrets)
	if err != nil {
		return err
	}
	env, err := mergeEnv(c.onboardedBaseEnv(ctx, name, svc), edits, req.EnvAck)
	if err != nil {
		return redactRefConflicts(err, refs)
	}
	c.pullImage(ctx, req.Image)
	for i := 1; i <= svc.Replicas; i++ {
		cname := fmt.Sprintf("goproxy-onb-%s-c%d", name, i)
		id, err := c.createContainer(ctx, cname, createBody{Image: req.Image, Env: env})
		if err != nil {
			return fmt.Errorf("create canary %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start canary %s: %w", cname, err)
		}
	}
	svc.CanaryImage = req.Image
	svc.CanaryReplicas = svc.Replicas
	if err := store.Put(svc); err != nil {
		return err
	}
	return rebuildOnboardedRoute(ctx, c, name, svc, routesPath)
}

// promoteOnboarded keeps only the canary containers serving: drops the
// original + old clones from the route, removes the old clones (the original
// keeps running but is no longer a backend), updates the saved image to the
// canary image. PreviousImage is stamped so rollback shows up.
func (c *dockerClient) promoteOnboarded(ctx context.Context, name string, store *OnboardedStore, routesPath string) error {
	svc, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("not onboarded: %s", name)
	}
	if svc.Host == "" {
		return fmt.Errorf("%q is managed-only (no route) — manage it via docker compose", name)
	}
	if svc.CanaryImage == "" {
		return fmt.Errorf("no canary to promote for %q", name)
	}
	// Capture the ACTUAL live env from a promoted canary container FIRST,
	// before any mutation happens. svc.Env is the template scaleOnboarded
	// clones from later; if an env edit was staged via stageOnboarded it
	// only ever reached the canary containers, never svc.Env itself. Failing
	// here (before tearing down old clones or flipping svc.Image) avoids
	// half-promoting into an inconsistent state when we can't determine the
	// promoted env.
	all, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name))
	if err != nil {
		return err
	}
	cPrefix := fmt.Sprintf("goproxy-onb-%s-c", name)
	var canaryID string
	for _, cl := range all {
		if strings.HasPrefix(cl.name(), cPrefix) {
			canaryID = cl.ID
			break
		}
	}
	if canaryID == "" {
		return fmt.Errorf("no canary container found for %q", name)
	}
	canaryEnv, err := c.inspectEnv(ctx, canaryID)
	if err != nil {
		return fmt.Errorf("inspect canary env: %w", err)
	}
	// Tear down old (non-canary) clones first.
	for _, cl := range all {
		if strings.HasPrefix(cl.name(), cPrefix) {
			continue
		}
		_ = c.stopContainer(ctx, cl.ID)
		_ = c.removeContainer(ctx, cl.ID)
	}
	// Drop original from the route — user's container keeps running but isn't
	// a backend anymore. They can stop it manually if they want.
	svc.OriginalRouted = false
	svc.PreviousImage = svc.Image
	svc.Image = svc.CanaryImage
	svc.Env = canaryEnv
	svc.CanaryImage = ""
	svc.CanaryReplicas = 0
	if err := store.Put(svc); err != nil {
		return err
	}
	return rebuildOnboardedRoute(ctx, c, name, svc, routesPath)
}

// discardOnboarded removes the canary containers; the original + old clones
// keep serving unchanged.
func (c *dockerClient) discardOnboarded(ctx context.Context, name string, store *OnboardedStore, routesPath string) error {
	svc, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("not onboarded: %s", name)
	}
	if svc.CanaryImage == "" {
		return fmt.Errorf("no canary to discard for %q", name)
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-c"]}`, name))
	if err != nil {
		return err
	}
	for _, cl := range all {
		if !strings.HasPrefix(cl.name(), fmt.Sprintf("goproxy-onb-%s-c", name)) {
			continue
		}
		_ = c.stopContainer(ctx, cl.ID)
		_ = c.removeContainer(ctx, cl.ID)
	}
	svc.CanaryImage = ""
	svc.CanaryReplicas = 0
	if err := store.Put(svc); err != nil {
		return err
	}
	return rebuildOnboardedRoute(ctx, c, name, svc, routesPath)
}

// replaceOnboarded is stage + promote in one shot, without the dual-serving
// canary window. New clones come up first; only after they're started is the
// route swapped and the old clones removed. PreviousImage stamped for rollback.
//
// Exempt from replaceService's inspectHostConfigUnknowns guard: this only
// ever recreates goproxy-onb-<name>-* clones, which never have PortBindings
// set and are already gated once at onboard time.
func (c *dockerClient) replaceOnboarded(ctx context.Context, name string, req ReplaceServiceRequest, store *OnboardedStore, routesPath string) error {
	svc, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("not onboarded: %s", name)
	}
	if svc.Host == "" {
		return fmt.Errorf("%q is managed-only (no route) — manage it via docker compose", name)
	}
	if req.Image == "" {
		return fmt.Errorf("image is required")
	}
	if svc.CanaryImage != "" {
		return fmt.Errorf("%q has a canary in flight — promote or discard first", name)
	}
	edits, refs, err := resolveSecretRefs(name, req.Env, c.secrets)
	if err != nil {
		return err
	}
	env, err := mergeEnv(c.onboardedBaseEnv(ctx, name, svc), edits, req.EnvAck)
	if err != nil {
		return redactRefConflicts(err, refs)
	}
	c.pullImage(ctx, req.Image)
	// Spin up replacements named -r2 to avoid colliding with existing -1..N.
	startIdx := 1000
	var newIDs []string
	for i := 0; i < svc.Replicas; i++ {
		cname := fmt.Sprintf("goproxy-onb-%s-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{Image: req.Image, Env: env})
		if err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			for _, oid := range newIDs {
				_ = c.removeContainer(ctx, oid)
			}
			return fmt.Errorf("start %s: %w", cname, err)
		}
		newIDs = append(newIDs, id)
	}
	// Give the new clones a few seconds to bind before swapping.
	time.Sleep(3 * time.Second)
	// Remove any clone that isn't one of the freshly-created ones. The only
	// thing we preserve is an ACTIVE canary (svc.CanaryImage set) — but
	// replaceOnboarded already rejected that case earlier, so post-promote
	// c-prefixed leftovers ARE eligible for cleanup here.
	all, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name))
	if err != nil {
		return err
	}
	for _, cl := range all {
		n := cl.name()
		isNew := false
		for i := 0; i < svc.Replicas; i++ {
			if n == fmt.Sprintf("goproxy-onb-%s-%d", name, startIdx+i) {
				isNew = true
				break
			}
		}
		if isNew {
			continue
		}
		_ = c.stopContainer(ctx, cl.ID)
		_ = c.removeContainer(ctx, cl.ID)
	}
	svc.OriginalRouted = false
	svc.PreviousImage = svc.Image
	svc.Image = req.Image
	if err := store.Put(svc); err != nil {
		return err
	}
	return rebuildOnboardedRoute(ctx, c, name, svc, routesPath)
}

func nextCloneIndex(clones []dockerContainer, name string) int {
	prefix := "goproxy-onb-" + name + "-"
	max := 0
	for _, c := range clones {
		n := c.name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		var v int
		_, _ = fmt.Sscanf(n[len(prefix):], "%d", &v)
		if v > max {
			max = v
		}
	}
	return max + 1
}

func sortByNameDesc(in []dockerContainer) {
	for i := 0; i < len(in); i++ {
		for j := i + 1; j < len(in); j++ {
			if in[j].name() > in[i].name() {
				in[i], in[j] = in[j], in[i]
			}
		}
	}
}

// offboardContainer is the inverse of onboardContainer: remove clones, drop
// the static route, drop the onboarded record, disconnect original from edge.
// The original container itself is left intact (the user started it, the user
// can stop it).
func (c *dockerClient) offboardContainer(ctx context.Context, name string, store *OnboardedStore, routesPath string) error {
	svc, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("not onboarded: %s", name)
	}
	_ = svc // currently unused but useful for future logic
	clones, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name))
	if err == nil {
		for _, cl := range clones {
			_ = c.stopContainer(ctx, cl.ID)
			_ = c.removeContainer(ctx, cl.ID)
		}
	}
	// Disconnect the original from edge (best-effort; user may have already removed it).
	originals, _ := c.listAll(ctx, fmt.Sprintf(`{"name":["%s"]}`, name))
	for _, ct := range originals {
		if ct.name() == name {
			_ = c.disconnectFromEdge(ctx, ct.ID)
		}
	}
	if err := removeOnboardedRoute(routesPath, name); err != nil {
		return err
	}
	return store.Remove(name)
}

// ---- Talk to the proxy's /refresh endpoint after routes.json changes ----

func proxyRefresh(proxyURL string) {
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}
	client := http.Client{Timeout: 2 * time.Second}
	_, _ = client.Post(proxyURL+"/refresh", "application/json", nil)
}

// onboardedBaseEnv is the env an onboarded service is ACTUALLY running.
//
// OnboardedService.Env is only a snapshot taken at onboard time and is never
// refreshed when a container is recreated, so it drifts. That matters now that
// env is merged rather than replaced: a conflict dialog built on the snapshot
// would ask the user to choose against a "current" value nothing is running.
//
// Prefers a live non-canary clone, then the original container (named for the
// service itself), and only falls back to the stored snapshot when neither can
// be read — better a stale base than no base, since the caller would otherwise
// create replicas with an empty environment.
//
// A c-prefixed container is only skipped while svc.CanaryImage is set (an
// actively-staged canary, not yet the live config) — matching the same
// CanaryImage-gated pattern used by scaleOnboarded and rebuildOnboardedRoute.
// Once promote has run, CanaryImage clears and the c-prefixed container IS
// the live one; skipping it unconditionally would fall through to the stale
// original container's env, silently reverting any promoted env edit.
func (c *dockerClient) onboardedBaseEnv(ctx context.Context, name string, svc OnboardedService) []string {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name))
	if err == nil {
		cPrefix := fmt.Sprintf("goproxy-onb-%s-c", name)
		for _, ct := range all {
			if svc.CanaryImage != "" && strings.HasPrefix(ct.name(), cPrefix) {
				continue // actively-staged canary replica — not the live config
			}
			if env, err := c.inspectEnv(ctx, ct.ID); err == nil && len(env) > 0 {
				return env
			}
		}
	}
	if orig, err := c.listAll(ctx, fmt.Sprintf(`{"name":["%s"]}`, name)); err == nil {
		for _, ct := range orig {
			if ct.name() != name {
				continue
			}
			if env, err := c.inspectEnv(ctx, ct.ID); err == nil && len(env) > 0 {
				return env
			}
		}
	}
	return svc.Env
}

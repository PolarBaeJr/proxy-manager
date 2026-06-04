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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	Name      string            `json:"name"`              // logical service name (matches container)
	Host      string            `json:"host"`              // proxy.host equivalent
	Port      int               `json:"port"`              // internal container port
	Image     string            `json:"image"`             // for replica cloning
	Env       []string          `json:"env,omitempty"`     // captured at onboard time
	Labels    map[string]string `json:"labels,omitempty"`  // original labels we want to preserve
	Replicas  int               `json:"replicas"`          // includes the original (>= 1)
	CreatedAt int64             `json:"created_at"`
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
func upsertOnboardedRoute(path, name, host string, backends []string) error {
	f, err := readRoutesFile(path)
	if err != nil {
		return err
	}
	entry := routesEntry{
		Name:      "onboarded: " + name,
		Host:      host,
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

// ---- Onboarding flow ----

type OnboardRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Replicas int    `json:"replicas,omitempty"`
}

func (c *dockerClient) onboardContainer(ctx context.Context, name string, req OnboardRequest, store *OnboardedStore, routesPath string) error {
	if req.Host == "" || req.Port <= 0 {
		return fmt.Errorf("host and port are required")
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
	// Capture image + env so we can clone replicas later.
	image := ct.Image
	env, err := c.inspectEnv(ctx, ct.ID)
	if err != nil {
		return fmt.Errorf("inspect env: %w", err)
	}
	if err := c.connectToEdge(ctx, ct.ID); err != nil {
		return fmt.Errorf("connect to edge: %w", err)
	}
	// Persist onboarded record FIRST so scale/delete can find it even if the
	// route-write step below fails.
	svc := OnboardedService{
		Name:      name,
		Host:      req.Host,
		Port:      req.Port,
		Image:     image,
		Env:       env,
		Replicas:  1,
		CreatedAt: time.Now().Unix(),
	}
	if err := store.Put(svc); err != nil {
		return fmt.Errorf("save onboarded record: %w", err)
	}
	// Write the static route entry using the container's DNS name (resolves
	// inside the edge network as long as the container is connected).
	backend := fmt.Sprintf("http://%s:%d", name, req.Port)
	if err := upsertOnboardedRoute(routesPath, name, req.Host, []string{backend}); err != nil {
		return fmt.Errorf("update routes.json: %w", err)
	}
	// Scale to requested replicas (will clone if >1).
	if req.Replicas > 1 {
		if err := c.scaleOnboarded(ctx, name, req.Replicas, store, routesPath); err != nil {
			return fmt.Errorf("scale: %w", err)
		}
	}
	return nil
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
	if desired < 1 {
		return fmt.Errorf("onboarded services must keep at least the original (>= 1)")
	}
	// Find all current containers: the original + any goproxy-onb-<name>-N.
	cloneFilter := fmt.Sprintf(`{"name":["goproxy-onb-%s-"]}`, name)
	clones, err := c.listAll(ctx, cloneFilter)
	if err != nil {
		return err
	}
	// liveOnly() is irrelevant here — onboarded containers don't use the canary label.
	current := 1 + len(clones)
	if current == desired {
		return rebuildOnboardedRoute(ctx, c, name, svc, clones, routesPath)
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
		// Re-list to include the new clones.
		clones, err = c.listAll(ctx, cloneFilter)
		if err != nil {
			return err
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
		clones = clones[toRemove:]
	}
	if err := rebuildOnboardedRoute(ctx, c, name, svc, clones, routesPath); err != nil {
		return err
	}
	return store.SetReplicas(name, desired)
}

func rebuildOnboardedRoute(_ context.Context, _ *dockerClient, name string, svc OnboardedService, clones []dockerContainer, routesPath string) error {
	backends := []string{fmt.Sprintf("http://%s:%d", name, svc.Port)}
	for _, c := range clones {
		backends = append(backends, fmt.Sprintf("http://%s:%d", c.name(), svc.Port))
	}
	return upsertOnboardedRoute(routesPath, name, svc.Host, backends)
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

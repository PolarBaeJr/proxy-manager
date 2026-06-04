package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	dockerSock = "/var/run/docker.sock"
	dockerAPI  = "v1.43"

	managedNetwork = "edge"

	labelEnable     = "proxy.enable"
	labelHost       = "proxy.host"
	labelPort       = "proxy.port"
	labelPath       = "proxy.path"
	labelStrip      = "proxy.strip"
	labelName       = "proxy.name"
	labelWeight     = "proxy.weight"
	labelService    = "proxy.service"
	labelUnscalable = "proxy.unscalable" // when "true", dashboard greys out +/- buttons
	labelPrevImage  = "proxy.previous_image" // set on Replace; enables one-click Rollback
	labelCanary     = "proxy.canary"         // "true" → staged replicas, served alongside live
)

// dockerClient is the dashboard's READ-WRITE view of the Docker daemon.
// Required for creating/scaling/deleting services. Mount is rw in compose.
type dockerClient struct{ http *http.Client }

func newDockerClient() *dockerClient {
	return &dockerClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", dockerSock)
				},
			},
		},
	}
}

func (c *dockerClient) do(ctx context.Context, method, path string, body any) (io.ReadCloser, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://docker/"+dockerAPI+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("docker %s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	return resp.Body, nil
}

func (c *dockerClient) get(ctx context.Context, path string) (io.ReadCloser, error) {
	return c.do(ctx, "GET", path, nil)
}

type dockerContainer struct {
	ID              string            `json:"Id"`
	Names           []string          `json:"Names"`
	Image           string            `json:"Image"`
	State           string            `json:"State"`
	Labels          map[string]string `json:"Labels"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (c *dockerContainer) name() string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID[:12]
}

func (c *dockerClient) listAll(ctx context.Context, filter string) ([]dockerContainer, error) {
	q := "/containers/json?all=true"
	if filter != "" {
		q += "&filters=" + url.QueryEscape(filter)
	}
	body, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []dockerContainer
	return out, json.NewDecoder(body).Decode(&out)
}

func (c *dockerClient) listRunning(ctx context.Context, filter string) ([]dockerContainer, error) {
	q := "/containers/json"
	if filter != "" {
		q += "?filters=" + url.QueryEscape(filter)
	}
	body, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []dockerContainer
	return out, json.NewDecoder(body).Decode(&out)
}

// ---- Container lifecycle (for service management) ----

type createBody struct {
	Image            string            `json:"Image"`
	Labels           map[string]string `json:"Labels,omitempty"`
	Env              []string          `json:"Env,omitempty"`
	HostConfig       hostConfig        `json:"HostConfig"`
	NetworkingConfig struct {
		EndpointsConfig map[string]struct{} `json:"EndpointsConfig"`
	} `json:"NetworkingConfig"`
}
type hostConfig struct {
	NetworkMode   string `json:"NetworkMode"`
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
}

// pullImage tries to pull from a registry. Errors are non-fatal: if the image
// exists locally (built on the host, never pushed), createContainer will succeed
// anyway. The pull just makes sure we have the latest if it IS in a registry.
func (c *dockerClient) pullImage(ctx context.Context, image string) {
	ref := image
	tag := "latest"
	if i := strings.LastIndex(image, ":"); i != -1 && !strings.Contains(image[i:], "/") {
		ref = image[:i]
		tag = image[i+1:]
	}
	body, err := c.do(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(ref)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		log.Printf("pull %s skipped (probably a local image): %v", image, err)
		return
	}
	defer body.Close()
	_, _ = io.Copy(io.Discard, body)
}

func (c *dockerClient) createContainer(ctx context.Context, name string, body createBody) (string, error) {
	body.HostConfig.NetworkMode = managedNetwork
	body.HostConfig.RestartPolicy.Name = "unless-stopped"
	body.NetworkingConfig.EndpointsConfig = map[string]struct{}{managedNetwork: {}}
	resp, err := c.do(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), body)
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var out struct{ ID string `json:"Id"` }
	return out.ID, json.NewDecoder(resp).Decode(&out)
}

func (c *dockerClient) startContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", "/containers/"+id+"/start", nil)
	if err != nil {
		return err
	}
	resp.Close()
	return nil
}

func (c *dockerClient) stopContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", "/containers/"+id+"/stop?t=5", nil)
	if err != nil {
		return err
	}
	resp.Close()
	return nil
}

func (c *dockerClient) removeContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "DELETE", "/containers/"+id+"?force=true", nil)
	if err != nil {
		return err
	}
	resp.Close()
	return nil
}

// inspectContainer returns just the Env slice for a given container — needed
// when scaling so the new replica gets the same runtime config as the template.
func (c *dockerClient) inspectEnv(ctx context.Context, id string) ([]string, error) {
	body, err := c.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Config.Env, nil
}

// ---- Service-level operations ----

type Service struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	Path            string            `json:"path,omitempty"`
	Replicas        int               `json:"replicas"`
	Unscalable      bool              `json:"unscalable,omitempty"`
	PreviousImage   string            `json:"previous_image,omitempty"`   // for one-click rollback
	UpdateAvailable bool              `json:"update_available,omitempty"` // set by image checker
	CanaryImage     string            `json:"canary_image,omitempty"`     // non-empty when a stage is in progress
	CanaryReplicas  int               `json:"canary_replicas,omitempty"`
	Onboarded       bool              `json:"onboarded,omitempty"`        // adopted from an unlabelled container
	Members         []dockerContainer `json:"-"`
	Labels          map[string]string `json:"labels,omitempty"`
}

func (c *dockerClient) listServices(ctx context.Context) ([]Service, error) {
	containers, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s"]}`, labelService))
	if err != nil {
		return nil, err
	}
	byName := map[string]*Service{}
	for _, ct := range containers {
		name := ct.Labels[labelService]
		isCanary := ct.Labels[labelCanary] == "true"
		s, ok := byName[name]
		if !ok {
			s = &Service{Name: name, Labels: ct.Labels}
			byName[name] = s
		}
		s.Members = append(s.Members, ct)
		if isCanary {
			s.CanaryImage = ct.Image
			s.CanaryReplicas++
		} else {
			port, _ := strconv.Atoi(ct.Labels[labelPort])
			s.Image = ct.Image
			s.Host = ct.Labels[labelHost]
			s.Port = port
			s.Path = ct.Labels[labelPath]
			s.Unscalable = ct.Labels[labelUnscalable] == "true"
			s.PreviousImage = ct.Labels[labelPrevImage]
			s.Replicas++
		}
	}
	out := make([]Service, 0, len(byName))
	for _, s := range byName {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type CreateServiceRequest struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Host       string            `json:"host"`
	Port       int               `json:"port"`
	Path       string            `json:"path,omitempty"`
	Strip      bool              `json:"strip,omitempty"`
	Replicas   int               `json:"replicas"`
	Unscalable bool              `json:"unscalable,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// ReplaceServiceRequest swaps a service's image (and optionally env) in place.
// Spins up new containers first, briefly waits, then removes the old ones —
// approximation of a rolling deploy on a single host.
type ReplaceServiceRequest struct {
	Image string            `json:"image"`         // required
	Env   map[string]string `json:"env,omitempty"` // if nil, copy from template
}

// guardUnscalable refuses to scale a service that has any container labeled
// proxy.unscalable=true above 1 replica. Returns nil if scaling is allowed.
func (c *dockerClient) guardUnscalable(ctx context.Context, name string, desired int) error {
	existing, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil || len(existing) == 0 {
		return nil
	}
	if existing[0].Labels[labelUnscalable] == "true" && desired != 1 {
		return fmt.Errorf("%q is marked unscalable — replica count must stay at 1", name)
	}
	return nil
}

func (c *dockerClient) scaleService(ctx context.Context, name string, desired int) error {
	if desired < 0 {
		return fmt.Errorf("replicas must be >= 0")
	}
	if err := c.guardUnscalable(ctx, name, desired); err != nil {
		return err
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found (no live replicas)", name)
	}
	tpl := existing[0]
	current := len(existing)
	switch {
	case current == desired:
		return nil
	case current < desired:
		// Pull the template's env so replicas get the same runtime config
		// (DATABASE_URL, API keys, etc.) — the listAll summary doesn't include Env.
		env, err := c.inspectEnv(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect template %s: %w", tpl.name(), err)
		}
		for i := 0; i < desired-current; i++ {
			n := nextReplicaIndex(existing, name) + i
			cname := fmt.Sprintf("goproxy-%s-%d", name, n)
			id, err := c.createContainer(ctx, cname, createBody{Image: tpl.Image, Labels: tpl.Labels, Env: env})
			if err != nil {
				return fmt.Errorf("create %s: %w", cname, err)
			}
			if err := c.startContainer(ctx, id); err != nil {
				return fmt.Errorf("start %s: %w", cname, err)
			}
		}
	default:
		// Scale down: only remove containers WE created (prefix "goproxy-").
		// Never touch docker-compose-managed originals — they hold the canonical
		// env vars / volumes and are the source of truth for re-scaling later.
		toRemove := current - desired
		var ours []dockerContainer
		for _, ct := range existing {
			if strings.HasPrefix(ct.name(), "goproxy-") {
				ours = append(ours, ct)
			}
		}
		// Remove highest-indexed first (e.g. goproxy-foo-5 before goproxy-foo-2).
		sort.Slice(ours, func(i, j int) bool { return ours[i].name() > ours[j].name() })
		if len(ours) < toRemove {
			return fmt.Errorf("can only scale down to %d (the original is not removable)", current-len(ours))
		}
		for i := 0; i < toRemove; i++ {
			_ = c.stopContainer(ctx, ours[i].ID)
			if err := c.removeContainer(ctx, ours[i].ID); err != nil {
				return fmt.Errorf("remove %s: %w", ours[i].name(), err)
			}
		}
	}
	return nil
}

func nextReplicaIndex(existing []dockerContainer, service string) int {
	max := 0
	prefix := "goproxy-" + service + "-"
	for _, ct := range existing {
		n := ct.name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if v, err := strconv.Atoi(n[len(prefix):]); err == nil && v > max {
			max = v
		}
	}
	return max + 1
}

func (c *dockerClient) createService(ctx context.Context, req CreateServiceRequest) error {
	if req.Name == "" || req.Image == "" || req.Host == "" || req.Port == 0 {
		return fmt.Errorf("name, image, host, and port are required")
	}
	if req.Replicas < 1 {
		req.Replicas = 1
	}
	c.pullImage(ctx, req.Image)
	labels := map[string]string{
		labelEnable:  "true",
		labelHost:    req.Host,
		labelPort:    strconv.Itoa(req.Port),
		labelService: req.Name,
		labelName:    req.Name,
	}
	if req.Path != "" {
		labels[labelPath] = req.Path
	}
	if req.Strip {
		labels[labelStrip] = "true"
	}
	if req.Unscalable {
		labels[labelUnscalable] = "true"
	}
	var env []string
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	for i := 1; i <= req.Replicas; i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", req.Name, i)
		id, err := c.createContainer(ctx, cname, createBody{Image: req.Image, Labels: labels, Env: env})
		if err != nil {
			return fmt.Errorf("create %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start %s: %w", cname, err)
		}
	}
	return nil
}

// liveOnly returns containers that aren't canary (the active production set).
func liveOnly(in []dockerContainer) []dockerContainer {
	out := in[:0:0]
	for _, ct := range in {
		if ct.Labels[labelCanary] != "true" {
			out = append(out, ct)
		}
	}
	return out
}
func canaryOnly(in []dockerContainer) []dockerContainer {
	out := in[:0:0]
	for _, ct := range in {
		if ct.Labels[labelCanary] == "true" {
			out = append(out, ct)
		}
	}
	return out
}

// replaceService creates fresh containers with a new image (and optionally new
// env), starts them, waits briefly, then removes the old containers. Replica
// count is preserved. Labels are inherited from the existing template.
func (c *dockerClient) replaceService(ctx context.Context, name string, req ReplaceServiceRequest) error {
	if req.Image == "" {
		return fmt.Errorf("image is required")
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	existing := liveOnly(all)
	if len(existing) == 0 {
		return fmt.Errorf("service %q not found (no live replicas)", name)
	}
	tpl := existing[0]

	// Resolve env. If the caller passed env, use only that. Otherwise copy from
	// the template container so the new replicas start with the same config.
	var env []string
	if len(req.Env) > 0 {
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
	} else {
		got, err := c.inspectEnv(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect template env: %w", err)
		}
		env = got
	}

	c.pullImage(ctx, req.Image)

	// Stamp the new containers' labels with the previous image for one-click rollback.
	newLabels := map[string]string{}
	for k, v := range tpl.Labels {
		newLabels[k] = v
	}
	if tpl.Image != "" && tpl.Image != req.Image {
		newLabels[labelPrevImage] = tpl.Image
	}

	// Create N replacements (same count as current).
	startIdx := nextReplicaIndex(existing, name)
	var newIDs []string
	for i := 0; i < len(existing); i++ {
		cname := fmt.Sprintf("goproxy-%s-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image:  req.Image,
			Labels: newLabels,
			Env:    env,
		})
		if err != nil {
			// Roll back: tear down any new ones we already created.
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

	// Give the new containers a few seconds to bind their ports / accept connections
	// before we tear down the old. Crude — production would health-check here.
	time.Sleep(5 * time.Second)

	for _, ct := range existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("replace %s: failed to remove old %s: %v (new ones are running)", name, ct.name(), err)
		}
	}
	return nil
}

// stageCanary creates additional replicas of a service with a new image. They
// share the live service's host/port labels, so the proxy round-robins traffic
// across BOTH live and canary while they coexist. No old containers removed.
func (c *dockerClient) stageCanary(ctx context.Context, name string, req ReplaceServiceRequest) error {
	if req.Image == "" {
		return fmt.Errorf("image is required")
	}
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	if len(canaryOnly(all)) > 0 {
		return fmt.Errorf("%q already has a canary — promote or discard it first", name)
	}
	live := liveOnly(all)
	if len(live) == 0 {
		return fmt.Errorf("service %q has no live replicas", name)
	}
	tpl := live[0]

	var env []string
	if len(req.Env) > 0 {
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
	} else {
		env, err = c.inspectEnv(ctx, tpl.ID)
		if err != nil {
			return fmt.Errorf("inspect template env: %w", err)
		}
	}

	canaryLabels := map[string]string{}
	for k, v := range tpl.Labels {
		canaryLabels[k] = v
	}
	canaryLabels[labelCanary] = "true"
	canaryLabels[labelPrevImage] = tpl.Image

	c.pullImage(ctx, req.Image)
	startIdx := nextReplicaIndex(all, name)
	for i := 0; i < len(live); i++ {
		cname := fmt.Sprintf("goproxy-%s-canary-%d", name, startIdx+i)
		id, err := c.createContainer(ctx, cname, createBody{
			Image: req.Image, Labels: canaryLabels, Env: env,
		})
		if err != nil {
			return fmt.Errorf("create canary %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start canary %s: %w", cname, err)
		}
	}
	return nil
}

// promoteCanary tears down the live containers and removes the canary label
// from the canary ones — they become the new live.
func (c *dockerClient) promoteCanary(ctx context.Context, name string) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	canary := canaryOnly(all)
	live := liveOnly(all)
	if len(canary) == 0 {
		return fmt.Errorf("no canary to promote for %q", name)
	}
	// Recreate each canary container WITHOUT the canary label (Docker doesn't allow
	// label edits on running containers). Same env, same image, new name.
	for _, ct := range canary {
		env, err := c.inspectEnv(ctx, ct.ID)
		if err != nil {
			return fmt.Errorf("inspect canary env: %w", err)
		}
		labels := map[string]string{}
		for k, v := range ct.Labels {
			if k == labelCanary {
				continue
			}
			labels[k] = v
		}
		startIdx := nextReplicaIndex(all, name)
		cname := fmt.Sprintf("goproxy-%s-%d", name, startIdx)
		id, err := c.createContainer(ctx, cname, createBody{Image: ct.Image, Labels: labels, Env: env})
		if err != nil {
			return fmt.Errorf("create promoted %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			return fmt.Errorf("start promoted %s: %w", cname, err)
		}
		// Now safe to drop the original canary container.
		_ = c.stopContainer(ctx, ct.ID)
		_ = c.removeContainer(ctx, ct.ID)
		// Refresh the all list so nextReplicaIndex sees the new container.
		all = append(all, dockerContainer{ID: id, Names: []string{"/" + cname}})
	}
	// Tear down the old live.
	for _, ct := range live {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			log.Printf("promote %s: failed to remove old live %s: %v", name, ct.name(), err)
		}
	}
	return nil
}

// discardCanary removes the canary containers; live keeps serving unchanged.
func (c *dockerClient) discardCanary(ctx context.Context, name string) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	canary := canaryOnly(all)
	if len(canary) == 0 {
		return fmt.Errorf("no canary to discard for %q", name)
	}
	for _, ct := range canary {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			return err
		}
	}
	return nil
}

func (c *dockerClient) deleteService(ctx context.Context, name string) error {
	existing, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	for _, ct := range existing {
		_ = c.stopContainer(ctx, ct.ID)
		if err := c.removeContainer(ctx, ct.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---- Routes view (independent of the proxy process) ----
//
// Reads container labels + the same routes.json file the proxy reads.
// Does NOT probe health — health is the proxy's job. Health column shows "unknown".

type RouteView struct {
	Host     string        `json:"host"`
	Path     string        `json:"path,omitempty"`
	Strip    bool          `json:"strip,omitempty"`
	Name     string        `json:"name,omitempty"`
	Service  string        `json:"service,omitempty"`
	Backends []BackendView `json:"backends"`
}
type BackendView struct {
	URL       string `json:"url"`
	Weight    int    `json:"weight"`
	Container string `json:"container,omitempty"`
}

type staticRoutesFile struct {
	Routes []struct {
		Host     string   `json:"host"`
		Path     string   `json:"path,omitempty"`
		Strip    bool     `json:"strip,omitempty"`
		Name     string   `json:"name,omitempty"`
		Backends []string `json:"backends"`
	} `json:"routes"`
}

func (c *dockerClient) listRoutes(ctx context.Context, configPath string) ([]RouteView, error) {
	groups := map[string]*RouteView{}
	add := func(key string, fresh func() *RouteView) *RouteView {
		g, ok := groups[key]
		if !ok {
			g = fresh()
			groups[key] = g
		}
		return g
	}

	// 1. Static config file.
	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			var cfg staticRoutesFile
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			for _, sr := range cfg.Routes {
				g := add(sr.Host+"|"+sr.Path, func() *RouteView {
					return &RouteView{Host: sr.Host, Path: sr.Path, Strip: sr.Strip, Name: sr.Name}
				})
				for _, u := range sr.Backends {
					g.Backends = append(g.Backends, BackendView{URL: u, Weight: 1, Container: "static"})
				}
			}
		}
	}

	// 2. Docker labels.
	containers, err := c.listRunning(ctx, fmt.Sprintf(`{"label":["%s=true"]}`, labelEnable))
	if err != nil {
		return nil, err
	}
	for _, ct := range containers {
		host := ct.Labels[labelHost]
		portStr := ct.Labels[labelPort]
		if host == "" || portStr == "" {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		var ip string
		if n, ok := ct.NetworkSettings.Networks[managedNetwork]; ok && n.IPAddress != "" {
			ip = n.IPAddress
		} else {
			for _, n := range ct.NetworkSettings.Networks {
				if n.IPAddress != "" {
					ip = n.IPAddress
					break
				}
			}
		}
		if ip == "" {
			continue
		}
		path := ct.Labels[labelPath]
		g := add(host+"|"+path, func() *RouteView {
			return &RouteView{
				Host: host, Path: path, Strip: ct.Labels[labelStrip] == "true",
				Name: ct.Labels[labelName], Service: ct.Labels[labelService],
			}
		})
		weight := 1
		if w, err := strconv.Atoi(ct.Labels[labelWeight]); err == nil && w > 0 {
			weight = w
		}
		g.Backends = append(g.Backends, BackendView{
			URL:       fmt.Sprintf("http://%s:%d", ip, port),
			Weight:    weight,
			Container: ct.name(),
		})
	}

	out := make([]RouteView, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.Backends, func(i, j int) bool { return g.Backends[i].URL < g.Backends[j].URL })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// dockerClient also exposes container event stream (used to invalidate caches if we add them later).
type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
}

func (c *dockerClient) streamEvents(ctx context.Context, onAction func(string)) {
	for {
		body, err := c.get(ctx, `/events?filters={"type":["container"]}`)
		if err != nil {
			log.Printf("event stream: %v — retry 2s", err)
			time.Sleep(2 * time.Second)
			continue
		}
		dec := json.NewDecoder(body)
		for {
			var ev dockerEvent
			if err := dec.Decode(&ev); err != nil {
				body.Close()
				break
			}
			onAction(ev.Action)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

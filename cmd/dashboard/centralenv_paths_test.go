package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// cenvFakeDocker is a stateful daemon stand-in for the central-env create
// path tests: it remembers what it created (so a path that re-lists sees its
// own writes), records every /containers/create body and every
// /networks/{n}/connect, and answers container inspect with a realistic
// template — non-empty Env, a Healthcheck, a custom edge alias and one extra
// compose network — so "was it carried forward?" is actually observable.
type cenvFakeDocker struct {
	mu       sync.Mutex
	seq      int
	items    map[string]dockerContainer
	inspect  map[string]cenvInspect
	creates  []cenvCreate
	connects []string
	// unhealthy, when set, marks a created container "(unhealthy)" in its
	// list Status if it returns true for the create body — how the
	// propagation tests fail a health gate.
	unhealthy func(body createBody) bool
	// onCreate, when set, is called (outside the lock) with every created
	// container's name — the mesh tests' cross-host ordering log.
	onCreate func(name string)
	// createErr, when set and returning non-empty, fails a create with that
	// text as Docker's error body — how the leak test makes a Docker error
	// echo env.
	createErr func(body createBody) string
	// imageID is what the (single) tag resolves to locally: stamped on
	// every created container and served by /images/{ref}/json. pullTo,
	// when set, is what a pull moves it to. pulls counts pulls.
	imageID string
	pullTo  string
	pulls   int
	// imageConfig is served as the image's Config (Env, User, WorkingDir…)
	// — what adopt subtracts and compares against.
	imageConfig map[string]any
}

type cenvInspect struct {
	// configImage overrides Config.Image (default: the list Image).
	configImage string
	env         []string
	health      *healthcheckSpec
	edge        []string
	networks    map[string][]string
	// restart is HostConfig.RestartPolicy.Name; config and hostConfig are
	// extra Config/HostConfig fields (the adopt-strict ones).
	restart    string
	config     map[string]any
	hostConfig map[string]any
}

type cenvCreate struct {
	name     string
	body     createBody
	aliases  []string
	networks []string // "<network>:<alias,alias>" per connect for this container
}

var cenvTemplateHealth = &healthcheckSpec{Test: []string{"CMD", "true"}, Interval: 1000000000, Retries: 3}

func newCenvFakeDocker() *cenvFakeDocker {
	return &cenvFakeDocker{items: map[string]dockerContainer{}, inspect: map[string]cenvInspect{}}
}

func (f *cenvFakeDocker) seed(c dockerContainer, in cenvInspect) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[c.ID] = c
	f.inspect[c.ID] = in
}

// seedTemplate adds the standard "app" live container: compose-created, so
// it carries com.docker.compose.* labels, with template env A=1/SECRET=tpl.
func (f *cenvFakeDocker) seedTemplate(extraLabels map[string]string) {
	labels := map[string]string{
		labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
		"com.docker.compose.project": "stack", "com.docker.compose.service": "app",
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	f.seed(dockerContainer{ID: "tpl1", Names: []string{"/app"}, Image: "ghcr.io/org/app:v1", State: "running", Labels: labels},
		cenvInspect{
			env:      []string{"A=1", "SECRET=tpl"},
			health:   cenvTemplateHealth,
			edge:     []string{"app", "app-alias"},
			networks: map[string][]string{"stack_default": {"app", "db-client"}},
		})
}

func (f *cenvFakeDocker) seedCanary(image string, extraLabels map[string]string) {
	labels := map[string]string{
		labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
		labelCanary: "true", labelPrevImage: "ghcr.io/org/app:v1",
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	f.seed(dockerContainer{ID: "can1", Names: []string{"/goproxy-app-canary-1"}, Image: image, State: "running", Labels: labels},
		cenvInspect{
			env:      []string{"A=canary", "SECRET=tpl"},
			health:   cenvTemplateHealth,
			edge:     []string{"goproxy-app-canary-1", "app-alias"},
			networks: map[string][]string{"stack_default": {"db-client"}},
		})
}

func (f *cenvFakeDocker) createsSnapshot() []cenvCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cenvCreate, len(f.creates))
	copy(out, f.creates)
	for i := range out {
		for _, c := range f.connects {
			if strings.HasPrefix(c, out[i].name+"|") {
				out[i].networks = append(out[i].networks, strings.TrimPrefix(c, out[i].name+"|"))
			}
		}
	}
	return out
}

func (f *cenvFakeDocker) list() []dockerContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerContainer, 0, len(f.items))
	for _, c := range f.items {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name() < out[j].name() })
	return out
}

func (f *cenvFakeDocker) client(t *testing.T) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/json"):
			json.NewEncoder(w).Encode(filterByLabels(f.list(), r.URL.Query().Get("filters")))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/containers/create"):
			var body createBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			name := r.URL.Query().Get("name")
			f.mu.Lock()
			if f.createErr != nil {
				if msg := f.createErr(body); msg != "" {
					f.mu.Unlock()
					http.Error(w, msg, http.StatusInternalServerError)
					return
				}
			}
			f.seq++
			id := fmt.Sprintf("gen-%d", f.seq)
			aliases := body.NetworkingConfig.EndpointsConfig[managedNetwork].Aliases
			f.creates = append(f.creates, cenvCreate{name: name, body: body, aliases: aliases})
			ct := dockerContainer{ID: id, Names: []string{"/" + name}, Image: body.Image, ImageID: f.imageID, State: "running", Status: "Up 1 second", Labels: body.Labels}
			if f.unhealthy != nil && f.unhealthy(body) {
				ct.Status = "Up 1 second (unhealthy)"
			}
			f.items[id] = ct
			f.inspect[id] = cenvInspect{env: body.Env, health: body.Healthcheck, edge: aliases, networks: map[string][]string{}, restart: body.HostConfig.RestartPolicy.Name}
			onCreate := f.onCreate
			f.mu.Unlock()
			if onCreate != nil {
				onCreate(name)
			}
			json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case r.Method == http.MethodPost && strings.Contains(p, "/networks/") && strings.HasSuffix(p, "/connect"):
			var body struct {
				Container      string
				EndpointConfig endpointSettings
			}
			json.NewDecoder(r.Body).Decode(&body)
			netName := strings.TrimSuffix(p[strings.Index(p, "/networks/")+len("/networks/"):], "/connect")
			f.mu.Lock()
			bc := f.items[body.Container]
			cname := bc.name()
			f.connects = append(f.connects, cname+"|"+netName+":"+strings.Join(body.EndpointConfig.Aliases, ","))
			if in, ok := f.inspect[body.Container]; ok {
				in.networks[netName] = body.EndpointConfig.Aliases
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && (strings.HasSuffix(p, "/start") || strings.HasSuffix(p, "/stop")):
			if strings.HasSuffix(p, "/stop") {
				id := idFromContainersPath(p)
				f.mu.Lock()
				if c, ok := f.items[id]; ok {
					c.State = "exited"
					f.items[id] = c
				}
				f.mu.Unlock()
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.Contains(p, "/containers/"):
			id := idFromContainersPath(p)
			f.mu.Lock()
			delete(f.items, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(p, "/containers/") && strings.HasSuffix(p, "/json"):
			id := idFromContainersPath(p)
			f.mu.Lock()
			in, ok := f.inspect[id]
			ct := f.items[id]
			f.mu.Unlock()
			if !ok {
				http.Error(w, "no such container", http.StatusNotFound)
				return
			}
			configImage := ct.Image
			if in.configImage != "" {
				configImage = in.configImage
			}
			nets := map[string]any{managedNetwork: map[string]any{"Aliases": in.edge}}
			for n, a := range in.networks {
				nets[n] = map[string]any{"Aliases": a}
			}
			config := map[string]any{"Env": in.env, "Healthcheck": in.health, "Image": configImage}
			for k, v := range in.config {
				config[k] = v
			}
			hostConfig := map[string]any{"Mounts": []mountSpec{}, "RestartPolicy": map[string]any{"Name": in.restart}}
			for k, v := range in.hostConfig {
				hostConfig[k] = v
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Name":            "/" + ct.name(),
				"Image":           "sha256:abc",
				"RestartCount":    0,
				"Config":          config,
				"HostConfig":      hostConfig,
				"NetworkSettings": map[string]any{"Networks": nets},
			})
		case strings.Contains(p, "/images/create"):
			f.mu.Lock()
			f.pulls++
			if f.pullTo != "" {
				f.imageID = f.pullTo
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(p, "/images/") && strings.HasSuffix(p, "/json"):
			f.mu.Lock()
			id, cfg := f.imageID, f.imageConfig
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": cfg})
		default:
			w.Write([]byte("{}"))
		}
	}))
}

// filterByLabels applies a Docker list filter's "label" clauses (key or
// key=value) the way the daemon does; other filter kinds are ignored.
func filterByLabels(in []dockerContainer, raw string) []dockerContainer {
	var filters struct {
		Label []string `json:"label"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &filters) != nil || len(filters.Label) == 0 {
		return in
	}
	out := []dockerContainer{}
	for _, c := range in {
		ok := true
		for _, l := range filters.Label {
			k, v, hasV := strings.Cut(l, "=")
			got, present := c.Labels[k]
			if !present || (hasV && got != v) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// withFastRecreate zeroes the settle/health delays for the duration of a
// test — every recreate path sleeps replaceSettleDelay between create and
// teardown.
func withFastRecreate(t *testing.T) {
	t.Helper()
	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })
}

// cenvPath is one create path under test plus the labels it is expected to
// add on top of the template's own (for the flag-off characterization).
type cenvPath struct {
	name   string
	canary bool // seed a staged canary as well as the live template
	run    func(ctx context.Context, dc *dockerClient) error
	// wantLabel adjusts the expected labels of the created containers
	// relative to the template's labels.
	wantLabel func(l map[string]string)
	// fromCanary: the created containers clone the canary (scaleCanary,
	// promoteCanary), so env/aliases come from the canary fixture.
	fromCanary bool
}

func cenvPaths() []cenvPath {
	stripOCI := func(l map[string]string) {}
	return []cenvPath{
		{name: "scaleService", run: func(ctx context.Context, dc *dockerClient) error { return dc.scaleService(ctx, "app", 2) },
			wantLabel: stripOCI},
		{name: "replaceService", run: func(ctx context.Context, dc *dockerClient) error {
			return dc.replaceService(ctx, "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"})
		}, wantLabel: func(l map[string]string) { l[labelPrevImage] = "ghcr.io/org/app:v1" }},
		{name: "setAutoUpdateLabel", run: func(ctx context.Context, dc *dockerClient) error { return dc.setAutoUpdateLabel(ctx, "app", true) },
			wantLabel: func(l map[string]string) { l[labelAutoUpdate] = "true" }},
		{name: "setUnscalableLabel", run: func(ctx context.Context, dc *dockerClient) error { return dc.setUnscalableLabel(ctx, "app", true) },
			wantLabel: func(l map[string]string) { l[labelUnscalable] = "true" }},
		{name: "setWeightLabel", run: func(ctx context.Context, dc *dockerClient) error { return dc.setWeightLabel(ctx, "app", 3) },
			wantLabel: func(l map[string]string) { l[labelWeight] = "3" }},
		{name: "createCanaryReplicas", run: func(ctx context.Context, dc *dockerClient) error {
			return dc.createCanaryReplicas(ctx, "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, 1)
		}, wantLabel: func(l map[string]string) { l[labelCanary] = "true"; l[labelPrevImage] = "ghcr.io/org/app:v1" }},
		{name: "scaleCanary", canary: true, fromCanary: true, run: func(ctx context.Context, dc *dockerClient) error { return dc.scaleCanary(ctx, "app", 2) }},
		{name: "promoteCanary", canary: true, fromCanary: true, run: func(ctx context.Context, dc *dockerClient) error { return dc.promoteCanary(ctx, "app") }},
	}
}

func cenvExpectedLabels(f *cenvFakeDocker, p cenvPath) map[string]string {
	src := "tpl1"
	if p.fromCanary {
		src = "can1"
	}
	f.mu.Lock()
	base := f.items[src].Labels
	f.mu.Unlock()
	want := map[string]string{}
	for k, v := range base {
		want[k] = v
	}
	if p.name == "promoteCanary" {
		delete(want, labelCanary)
	}
	if p.wantLabel != nil {
		p.wantLabel(want)
	}
	return want
}

// TestCreatePathsFlagOffCharacterization pins what every §6 create path
// sends to Docker with central env OFF: template env verbatim, template
// labels (plus the path's own label change), healthcheck, and — the one
// intentional change in this PR — the template's custom edge aliases and
// extra networks, which only replaceService/replaceServiceRolling carried
// before.
func TestCreatePathsFlagOffCharacterization(t *testing.T) {
	withFastRecreate(t)
	for _, p := range cenvPaths() {
		t.Run(p.name, func(t *testing.T) {
			f := newCenvFakeDocker()
			f.seedTemplate(nil)
			if p.canary {
				f.seedCanary("ghcr.io/org/app:v2", nil)
			}
			dc := f.client(t)
			wantLabels := cenvExpectedLabels(f, p)
			if err := p.run(context.Background(), dc); err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			creates := f.createsSnapshot()
			if len(creates) == 0 {
				t.Fatalf("%s created nothing", p.name)
			}
			wantEnv := []string{"A=1", "SECRET=tpl"}
			wantAliases := []string{"app-alias"}
			wantNets := []string{"stack_default:db-client"}
			if p.fromCanary {
				wantEnv = []string{"A=canary", "SECRET=tpl"}
			}
			for _, c := range creates {
				if !reflect.DeepEqual(c.body.Env, wantEnv) {
					t.Errorf("%s env = %v, want %v", c.name, c.body.Env, wantEnv)
				}
				if !reflect.DeepEqual(c.body.Labels, wantLabels) {
					t.Errorf("%s labels = %v, want %v", c.name, c.body.Labels, wantLabels)
				}
				if !reflect.DeepEqual(c.body.Healthcheck, cenvTemplateHealth) {
					t.Errorf("%s healthcheck = %+v, want %+v", c.name, c.body.Healthcheck, cenvTemplateHealth)
				}
				if !reflect.DeepEqual(c.aliases, wantAliases) {
					t.Errorf("%s edge aliases = %v, want %v", c.name, c.aliases, wantAliases)
				}
				if !reflect.DeepEqual(c.networks, wantNets) {
					t.Errorf("%s extra networks = %v, want %v", c.name, c.networks, wantNets)
				}
			}
		})
	}
}

// cenvOrigin returns an enabled origin resolver for dashboard-a owning "app"
// with a base and a dashboard-a override, so the expected env for a replica
// on this host differs from both the template env and the plain base.
func cenvOrigin(t *testing.T) *centralEnv {
	t.Helper()
	ce := newTestCentralEnv(t, "dashboard-a")
	if _, err := ce.store.Create("app", "dashboard-a", map[string]string{"A": "central", "B": "base"},
		map[string]map[string]string{"dashboard-a": {"B": "over-a"}, "dashboard-b": {"B": "over-b"}}, "test", ""); err != nil {
		t.Fatal(err)
	}
	return ce
}

// TestCreatePathsManagedUseCentralEnv: with central env on and "app"
// owned by this host, every §6 path creates from the central env (never the
// template's A=1/SECRET=tpl), stamps provenance, drops compose labels, and
// still carries healthcheck/aliases/networks.
func TestCreatePathsManagedUseCentralEnv(t *testing.T) {
	withFastRecreate(t)
	for _, p := range cenvPaths() {
		t.Run(p.name, func(t *testing.T) {
			f := newCenvFakeDocker()
			f.seedTemplate(nil)
			if p.canary {
				// Staged on the current central version — promoteCanary
				// refuses a canary staged on any other.
				f.seedCanary("ghcr.io/org/app:v2", map[string]string{labelEnvOrigin: "dashboard-a", labelEnvVersion: "1"})
			}
			dc := f.client(t)
			dc.central = cenvOrigin(t)
			wantLabels := stampEnvLabels(cenvExpectedLabels(f, p), centralEnvResult{Origin: "dashboard-a", Version: 1})
			if err := p.run(context.Background(), dc); err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			creates := f.createsSnapshot()
			if len(creates) == 0 {
				t.Fatalf("%s created nothing", p.name)
			}
			wantEnv := []string{"A=central", "B=over-a"}
			for _, c := range creates {
				if !reflect.DeepEqual(c.body.Env, wantEnv) {
					t.Errorf("%s env = %v, want central %v", c.name, c.body.Env, wantEnv)
				}
				if !reflect.DeepEqual(c.body.Labels, wantLabels) {
					t.Errorf("%s labels = %v, want %v", c.name, c.body.Labels, wantLabels)
				}
				if !reflect.DeepEqual(c.body.Healthcheck, cenvTemplateHealth) {
					t.Errorf("%s healthcheck = %+v", c.name, c.body.Healthcheck)
				}
				if !reflect.DeepEqual(c.aliases, []string{"app-alias"}) || !reflect.DeepEqual(c.networks, []string{"stack_default:db-client"}) {
					t.Errorf("%s aliases/networks = %v / %v", c.name, c.aliases, c.networks)
				}
			}
			// The template's shared label map must be untouched.
			f.mu.Lock()
			tplLabels := f.items["tpl1"].Labels
			f.mu.Unlock()
			if tplLabels != nil && tplLabels[labelEnvOrigin] != "" {
				t.Fatalf("template labels were mutated: %v", tplLabels)
			}
		})
	}
}

// TestCreatePathsFlagOnUnmanagedUnchanged: central env on but "app" not in
// the store/cache and unlabeled — every path behaves exactly as flag-off.
func TestCreatePathsFlagOnUnmanagedUnchanged(t *testing.T) {
	withFastRecreate(t)
	for _, p := range cenvPaths() {
		t.Run(p.name, func(t *testing.T) {
			off := newCenvFakeDocker()
			on := newCenvFakeDocker()
			for _, f := range []*cenvFakeDocker{off, on} {
				f.seedTemplate(nil)
				if p.canary {
					f.seedCanary("ghcr.io/org/app:v2", nil)
				}
			}
			dcOff := off.client(t)
			dcOn := on.client(t)
			ce := newTestCentralEnv(t, "dashboard-a")
			ce.store.Create("other", "dashboard-a", map[string]string{"A": "nope"}, nil, "", "")
			dcOn.central = ce
			if err := p.run(context.Background(), dcOff); err != nil {
				t.Fatal(err)
			}
			if err := p.run(context.Background(), dcOn); err != nil {
				t.Fatal(err)
			}
			a, b := off.createsSnapshot(), on.createsSnapshot()
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("flag-on unmanaged differs from flag-off:\n off=%+v\n on=%+v", a, b)
			}
		})
	}
}

// TestCreatePathsRefuseWhenCentralUnavailable: a broken origin record, or a
// peer-origin label with no cache and no reachable origin, refuses every
// path with zero containers created — never a fallback to local env.
func TestCreatePathsRefuseWhenCentralUnavailable(t *testing.T) {
	withFastRecreate(t)
	cases := map[string]func(t *testing.T) (*centralEnv, map[string]string){
		"broken-record": func(t *testing.T) (*centralEnv, map[string]string) {
			ce := newTestCentralEnv(t, "dashboard-a")
			writeFileAtomic(filepath.Join(ce.store.dir, "app.json"), []byte("{not json"), 0o600)
			store, _ := loadCentralEnvStore(ce.store.dir)
			ce.store = store
			return ce, nil
		},
		"peer-origin-no-cache": func(t *testing.T) (*centralEnv, map[string]string) {
			return newTestCentralEnv(t, "dashboard-b"), map[string]string{labelEnvOrigin: "dashboard-a", labelEnvVersion: "3"}
		},
	}
	for cname, setup := range cases {
		for _, p := range cenvPaths() {
			t.Run(cname+"/"+p.name, func(t *testing.T) {
				ce, extra := setup(t)
				f := newCenvFakeDocker()
				f.seedTemplate(extra)
				if p.canary {
					f.seedCanary("ghcr.io/org/app:v2", extra)
				}
				dc := f.client(t)
				dc.central = ce
				if err := p.run(context.Background(), dc); err == nil {
					t.Fatalf("%s succeeded without a central env", p.name)
				}
				if n := len(f.createsSnapshot()); n != 0 {
					t.Fatalf("%s created %d containers before refusing", p.name, n)
				}
			})
		}
	}
}

func TestCreatePathsManagedRefuseEnvEdits(t *testing.T) {
	withFastRecreate(t)
	edit := ReplaceServiceRequest{Image: "ghcr.io/org/app:v2", Env: map[string]string{"A": "local"}}
	runs := map[string]func(dc *dockerClient) error{
		"replaceService": func(dc *dockerClient) error { return dc.replaceService(context.Background(), "app", edit) },
		"replaceServiceRolling": func(dc *dockerClient) error {
			return dc.replaceServiceRolling(context.Background(), "app", edit, nil, rollingOpts{})
		},
		"createCanaryReplicas": func(dc *dockerClient) error { return dc.createCanaryReplicas(context.Background(), "app", edit, 1) },
		"stageCanary":          func(dc *dockerClient) error { return dc.stageCanary(context.Background(), "app", edit) },
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			f := newCenvFakeDocker()
			f.seedTemplate(nil)
			dc := f.client(t)
			dc.central = cenvOrigin(t)
			var managed errEnvCentrallyManaged
			if err := run(dc); !errors.As(err, &managed) {
				t.Fatalf("err = %v, want errEnvCentrallyManaged", err)
			}
			if n := len(f.createsSnapshot()); n != 0 {
				t.Fatalf("created %d containers", n)
			}
		})
	}
}

// TestScaleServiceNonOriginUsesCache: a peer host with the env cached (as
// spread leaves it) scales from the cached copy, stamped with the cache's
// origin/version, even with the origin unreachable.
func TestScaleServiceNonOriginUsesCache(t *testing.T) {
	f := newCenvFakeDocker()
	f.seedTemplate(map[string]string{labelEnvOrigin: "dashboard-a", labelEnvVersion: "3"})
	dc := f.client(t)
	ce := newTestCentralEnv(t, "dashboard-b")
	ce.cache.Put("app", "dashboard-a", 4, []string{"A=cached"})
	dc.central = ce
	if err := dc.scaleService(context.Background(), "app", 2); err != nil {
		t.Fatal(err)
	}
	c := f.createsSnapshot()[0]
	if !reflect.DeepEqual(c.body.Env, []string{"A=cached"}) || c.body.Labels[labelEnvVersion] != "4" || c.body.Labels[labelEnvOrigin] != "dashboard-a" {
		t.Fatalf("create = env %v labels %v", c.body.Env, c.body.Labels)
	}
}

// newCentralSpreadTarget is newSpreadTargetServer with a central-env
// resolver attached to the target's dockerClient (nil = flag off there).
func newCentralSpreadTarget(t *testing.T, ce *centralEnv) (*httptest.Server, *cenvFakeDocker) {
	t.Helper()
	f := newCenvFakeDocker()
	dc := f.client(t)
	if ce != nil {
		dc.central = ce
	}
	srv := httptest.NewServer(peerSpreadHandler("s3cret", "dashboard-b", dc, true))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestSpreadManagedShipsTargetEnvAndStamps(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	target := newTestCentralEnv(t, "dashboard-b")
	srv, tf := newCentralSpreadTarget(t, target)

	origin := newCenvFakeDocker()
	origin.seedTemplate(nil)
	dc := origin.client(t)
	dc.central = cenvOrigin(t)

	rec := postCentralSpread(t, dc, srv, `{"target":"dashboard-b","replicas":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	creates := tf.createsSnapshot()
	if len(creates) != 2 {
		t.Fatalf("target created %d, want 2", len(creates))
	}
	wantEnv := []string{"A=central", "B=over-b"}
	for _, c := range creates {
		if !reflect.DeepEqual(c.body.Env, wantEnv) {
			t.Errorf("%s env = %v, want dashboard-b's effective env %v", c.name, c.body.Env, wantEnv)
		}
		if c.body.Labels[labelEnvOrigin] != "dashboard-a" || c.body.Labels[labelEnvVersion] != "1" || c.body.Labels[labelSpread] != "true" {
			t.Errorf("%s labels = %v", c.name, c.body.Labels)
		}
		if !reflect.DeepEqual(c.body.Healthcheck, cenvTemplateHealth) {
			t.Errorf("%s healthcheck = %+v, want the origin template's", c.name, c.body.Healthcheck)
		}
		if len(c.networks) != 0 {
			t.Errorf("%s joined origin-host networks %v on the target", c.name, c.networks)
		}
	}
	cached, ok := target.cache.Get("app")
	if !ok || cached.Origin != "dashboard-a" || cached.Version != 1 || !reflect.DeepEqual(cached.Env, wantEnv) {
		t.Fatalf("target cache = %+v, %v", cached, ok)
	}
	// Origin's template never read its local env for this spread: the only
	// origin inspect calls were for the clone spec. (Env A=1 never shipped.)
	for _, c := range creates {
		for _, e := range c.body.Env {
			if e == "A=1" || e == "SECRET=tpl" {
				t.Fatalf("template env leaked into spread: %v", c.body.Env)
			}
		}
	}
}

func TestSpreadManagedRefusals(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	t.Run("target-flag-off", func(t *testing.T) {
		srv, tf := newCentralSpreadTarget(t, nil)
		origin := newCenvFakeDocker()
		origin.seedTemplate(nil)
		dc := origin.client(t)
		dc.central = cenvOrigin(t)
		rec := postCentralSpread(t, dc, srv, `{"target":"dashboard-b"}`)
		if rec.Code == http.StatusOK || len(tf.createsSnapshot()) != 0 {
			t.Fatalf("status = %d, creates = %d", rec.Code, len(tf.createsSnapshot()))
		}
	})
	t.Run("target-not-advertising-central-env", func(t *testing.T) {
		// The target has central env on, but its handshake didn't say so
		// (an older dashboard would silently drop the provenance).
		srv, tf := newCentralSpreadTarget(t, newTestCentralEnv(t, "dashboard-b"))
		origin := newCenvFakeDocker()
		origin.seedTemplate(nil)
		dc := origin.client(t)
		dc.central = cenvOrigin(t)
		rec := postSpread(t, dc, srv, `{"target":"dashboard-b"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), centralEnvFeature) || len(tf.createsSnapshot()) != 0 {
			t.Fatalf("status = %d body %s creates %d", rec.Code, rec.Body.String(), len(tf.createsSnapshot()))
		}
	})
	t.Run("non-origin-spreader", func(t *testing.T) {
		srv, tf := newCentralSpreadTarget(t, newTestCentralEnv(t, "dashboard-b"))
		origin := newCenvFakeDocker()
		origin.seedTemplate(map[string]string{labelEnvOrigin: "dashboard-z", labelEnvVersion: "2"})
		dc := origin.client(t)
		ce := newTestCentralEnv(t, "dashboard-a")
		ce.cache.Put("app", "dashboard-z", 2, []string{"A=z"})
		dc.central = ce
		rec := postCentralSpread(t, dc, srv, `{"target":"dashboard-b"}`)
		if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "centrally managed") || len(tf.createsSnapshot()) != 0 {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("older-version", func(t *testing.T) {
		target := newTestCentralEnv(t, "dashboard-b")
		target.cache.Put("app", "dashboard-a", 9, []string{"A=newer"})
		srv, tf := newCentralSpreadTarget(t, target)
		origin := newCenvFakeDocker()
		origin.seedTemplate(nil)
		dc := origin.client(t)
		dc.central = cenvOrigin(t)
		rec := postCentralSpread(t, dc, srv, `{"target":"dashboard-b"}`)
		if rec.Code == http.StatusOK || len(tf.createsSnapshot()) != 0 {
			t.Fatalf("status = %d, creates = %d", rec.Code, len(tf.createsSnapshot()))
		}
	})
}

// TestPeerSpreadHandlerCentralValidation drives /peer/spread directly with
// hand-built bodies: a bogus origin or zero version is a 400, and a target
// that is itself the origin refuses the peer-supplied copy.
func TestPeerSpreadHandlerCentralValidation(t *testing.T) {
	post := func(srv *httptest.Server, body peerSpreadRequest) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer s3cret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	base := peerSpreadRequest{Service: "app", Image: "ghcr.io/org/app:v1", Host: "app.example", Port: 8080, Replicas: 1, Env: []string{"A=1"}}

	srv, tf := newCentralSpreadTarget(t, newTestCentralEnv(t, "dashboard-b"))
	bad := base
	bad.CentralOrigin, bad.CentralVersion = "dashboard-a", 0
	if code := post(srv, bad); code != http.StatusBadRequest {
		t.Fatalf("zero version = %d", code)
	}
	bad.CentralOrigin, bad.CentralVersion = "not a host!", 1
	if code := post(srv, bad); code != http.StatusBadRequest {
		t.Fatalf("bad origin = %d", code)
	}
	self := base
	self.CentralOrigin, self.CentralVersion = "dashboard-b", 1
	if code := post(srv, self); code != http.StatusConflict {
		t.Fatalf("self-origin = %d", code)
	}
	if n := len(tf.createsSnapshot()); n != 0 {
		t.Fatalf("created %d", n)
	}
	// Plain (non-central) spreads are untouched and carry no provenance.
	if code := post(srv, base); code != http.StatusOK {
		t.Fatalf("plain spread = %d", code)
	}
	if l := tf.createsSnapshot()[0].body.Labels; l[labelEnvOrigin] != "" {
		t.Fatalf("plain spread stamped: %v", l)
	}
}

func TestDuplicateAndOnboardedRefuseManaged(t *testing.T) {
	f := newCenvFakeDocker()
	f.seedTemplate(nil)
	dc := f.client(t)
	dc.central = cenvOrigin(t)
	var managed errEnvCentrallyManaged

	reg := newTestPeerRegistry("http://peer-b:8098", true)
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	_, err := runServiceDuplicate(context.Background(), dc, reg, nil, "", "app", DuplicateServiceRequest{Target: "dashboard-b"}, "")
	if !errors.As(err, &managed) || !strings.Contains(err.Error(), "use spread") {
		t.Fatalf("duplicate err = %v", err)
	}

	onb := newTestOnboardedStore(t)
	onb.Put(OnboardedService{Name: "app", Host: "app.example", Port: 8080, Image: "ghcr.io/org/app:v1", Replicas: 1, CanaryImage: ""})
	req := ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}
	checks := map[string]error{
		"scaleOnboarded":       dc.scaleOnboarded(context.Background(), "app", 3, onb, ""),
		"stageOnboarded":       dc.stageOnboarded(context.Background(), "app", req, onb, ""),
		"stageOnboardedCanary": dc.stageOnboardedCanary(context.Background(), "app", req, 1, onb, ""),
		"replaceOnboarded":     dc.replaceOnboarded(context.Background(), "app", req, onb, ""),
	}
	onb.Put(OnboardedService{Name: "app", Host: "app.example", Port: 8080, Image: "ghcr.io/org/app:v1", Replicas: 1, CanaryImage: "ghcr.io/org/app:v2"})
	checks["scaleOnboardedCanary"] = dc.scaleOnboardedCanary(context.Background(), "app", 2, onb, "")
	for name, err := range checks {
		if !errors.As(err, &managed) {
			t.Errorf("%s err = %v, want errEnvCentrallyManaged", name, err)
		}
	}
	if n := len(f.createsSnapshot()); n != 0 {
		t.Fatalf("created %d containers", n)
	}
}

// TestPeerReceiversRefusePlainSeedOfManagedService: a spread or duplicate
// that carries no central env must not create a replica of a service the
// receiving host already treats as centrally managed — it would be built
// from the sender's local env.
func TestPeerReceiversRefusePlainSeedOfManagedService(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-b")
	ce.cache.Put("app", "dashboard-a", 2, []string{"A=central"})
	srv, tf := newCentralSpreadTarget(t, ce)
	b, _ := json.Marshal(peerSpreadRequest{Service: "app", Image: "ghcr.io/org/app:v1", Host: "app.example", Port: 8080, Replicas: 1, Env: []string{"A=local"}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || len(tf.createsSnapshot()) != 0 {
		t.Fatalf("plain spread of managed service: status %d, %d creates", resp.StatusCode, len(tf.createsSnapshot()))
	}

	df := newCenvFakeDocker()
	ddc := df.client(t)
	ddc.central = ce
	dsrv := httptest.NewServer(peerDuplicateHandler("s3cret", "dashboard-b", ddc, true))
	t.Cleanup(dsrv.Close)
	b, _ = json.Marshal(peerDuplicateRequest{Name: "app", Service: "app", Image: "ghcr.io/org/app:v1", Port: 8080, PublishPort: 18080, Env: []string{"A=local"}})
	req, _ = http.NewRequest(http.MethodPost, dsrv.URL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || len(df.createsSnapshot()) != 0 {
		t.Fatalf("duplicate of managed service: status %d, %d creates", resp.StatusCode, len(df.createsSnapshot()))
	}
}

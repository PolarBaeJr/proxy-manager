package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/labels"
	"github.com/redis/go-redis/v9"
)

func ov(services map[string]map[string]string) *labelOverlay {
	return &labelOverlay{Version: 1, Source: overlaySourceRedis, Services: services}
}

func TestApplyLabelOverlay(t *testing.T) {
	webLabels := map[string]string{
		labelEnable: "true", labelHost: "web.example", labelPort: "80", labelService: "web",
		labelWeight: "3", labelSticky: "true", labelAuth: "true", labelABVariant: "B", labelABID: "abcd1234",
		"com.example.other": "x",
	}
	otherLabels := map[string]string{labelService: "other", labelWeight: "7"}
	in := []dockerContainer{
		{ID: "1", Labels: webLabels},
		{ID: "2", Labels: otherLabels},
		{ID: "3", Labels: map[string]string{labelHost: "nosvc.example"}},
	}
	snapshot := func(m map[string]string) map[string]string {
		c := map[string]string{}
		for k, v := range m {
			c[k] = v
		}
		return c
	}
	webBefore, otherBefore := snapshot(webLabels), snapshot(otherLabels)

	out := applyLabelOverlay(in, ov(map[string]map[string]string{
		"web": {labelWeight: "9", labelCache: "30s"},
	}))

	got := out[0].Labels
	if got[labelWeight] != "9" || got[labelCache] != "30s" {
		t.Errorf("managed keys not set: %v", got)
	}
	if _, ok := got[labelSticky]; ok {
		t.Errorf("managed key absent from overlay must be unset: %v", got)
	}
	for _, k := range []string{labelEnable, labelHost, labelPort, labelService, labelAuth, labelABVariant, labelABID, "com.example.other"} {
		if got[k] != webLabels[k] {
			t.Errorf("non-managed %s changed: %q -> %q", k, webLabels[k], got[k])
		}
	}
	if !reflect.DeepEqual(webLabels, webBefore) || !reflect.DeepEqual(otherLabels, otherBefore) {
		t.Error("input label maps were mutated")
	}
	if in[0].Labels[labelWeight] != "3" {
		t.Error("input container slice was mutated")
	}
	if !reflect.DeepEqual(out[1].Labels, otherBefore) || !reflect.DeepEqual(out[2].Labels, in[2].Labels) {
		t.Error("unadopted containers must be untouched")
	}
}

func TestApplyLabelOverlayNeverTouchesNonManaged(t *testing.T) {
	// Even an overlay carrying identity/ab keys (a validation bug upstream)
	// can't leak them: apply only walks the registry's managed keys.
	in := []dockerContainer{{Labels: map[string]string{labelService: "web", labelHost: "a.example", labelABSplit: "10"}}}
	out := applyLabelOverlay(in, ov(map[string]map[string]string{
		"web": {labelHost: "evil.example", labelABSplit: "90", labelService: "other", labelAuth: "false"},
	}))
	l := out[0].Labels
	if l[labelHost] != "a.example" || l[labelABSplit] != "10" || l[labelService] != "web" {
		t.Errorf("non-managed keys leaked: %v", l)
	}
	if _, ok := l[labelAuth]; ok {
		t.Errorf("auth key leaked: %v", l)
	}
}

func TestApplyLabelOverlayNil(t *testing.T) {
	in := []dockerContainer{{Labels: map[string]string{labelService: "web"}}}
	if out := applyLabelOverlay(in, nil); &out[0] != &in[0] {
		t.Error("nil overlay should return input as-is")
	}
}

func TestAssembleGroupsWithOverlay(t *testing.T) {
	webA := map[string]string{labelEnable: "true", labelHost: "web.example", labelPort: "80", labelService: "web", labelWeight: "2", labelSticky: "true"}
	webB := map[string]string{labelEnable: "true", labelHost: "web.example", labelPort: "80", labelService: "web", labelCanary: "true", labelABVariant: "B", labelABID: "abcd1234", labelABSplit: "40"}
	api := map[string]string{labelEnable: "true", labelHost: "static.example", labelPort: "80", labelService: "api", labelWeight: "1", labelRateLimit: "true"}
	dc := fakeDocker(t, dockerJSON(
		container("1", "web-a", "running", webA, map[string]string{managedNetwork: "10.0.0.1"}),
		container("2", "web-b", "running", webB, map[string]string{managedNetwork: "10.0.0.2"}),
		container("3", "api-1", "running", api, map[string]string{managedNetwork: "10.0.0.3"}),
	))
	cfgPath := filepath.Join(t.TempDir(), "routes.json")
	data, _ := json.Marshal(staticConfig{Routes: []staticRoute{{Host: "static.example", Service: "api"}}})
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	base, baseBySvc, err := assembleGroupsWithOverlay(context.Background(), dc, cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	bg := findGroup(base, "web.example", "")
	if bg == nil || !bg.Sticky || bg.Backends[0].Weight != 2 || bg.Backends[1].Weight != 1 {
		t.Fatalf("baseline group = %+v", bg)
	}
	if baseBySvc["api"][0].Weight != 1 {
		t.Fatalf("baseline api weight = %d", baseBySvc["api"][0].Weight)
	}

	overlay := ov(map[string]map[string]string{
		"web": {labelWeight: "5"},
		"api": {labelWeight: "4"},
	})
	groups, bySvc, err := assembleGroupsWithOverlay(context.Background(), dc, cfgPath, overlay)
	if err != nil {
		t.Fatal(err)
	}
	g := findGroup(groups, "web.example", "")
	if g == nil || len(g.Backends) != 2 {
		t.Fatalf("group = %+v", g)
	}
	if g.Sticky {
		t.Error("proxy.sticky unset by overlay must disable stickiness")
	}
	for _, b := range g.Backends {
		if b.Weight != 5 {
			t.Errorf("backend %s weight = %d, want 5 (B replicas too)", b.URL, b.Weight)
		}
	}
	if g.abCfg == nil || g.abCfg.Split != 40 || g.Backends[1].Variant != abVariantB {
		t.Errorf("A/B config must survive the overlay: cfg=%+v variant=%q", g.abCfg, g.Backends[1].Variant)
	}
	s := findGroup(groups, "static.example", "")
	if s == nil || !s.static || s.RateLimit {
		t.Errorf("static group must ignore container labels: %+v", s)
	}
	if len(bySvc["api"]) != 1 || bySvc["api"][0].Weight != 4 {
		t.Errorf("backendsByService must carry the overlaid weight: %+v", bySvc["api"])
	}
	if len(s.Backends) != 1 || s.Backends[0].Weight != 4 {
		t.Errorf("static backfill weight = %+v", s.Backends)
	}
}

// fakeOverlaySource is an in-memory Redis stand-in.
type fakeOverlaySource struct {
	mu       sync.Mutex
	version  uint64
	exists   bool
	services map[string]map[string]string
	err      error
	loads    int
	notify   chan struct{}
}

func (f *fakeOverlaySource) Version(context.Context) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.exists, f.err
}

func (f *fakeOverlaySource) Load(context.Context) (*labelOverlay, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.exists {
		return nil, true, nil
	}
	cp := map[string]map[string]string{}
	for s, m := range f.services {
		cp[s] = map[string]string{}
		for k, v := range m {
			cp[s][k] = v
		}
	}
	return &labelOverlay{Version: f.version, Source: overlaySourceRedis, LoadedAt: time.Now(), Services: sanitizeOverlayServices(cp)}, false, nil
}

func (f *fakeOverlaySource) Watch(ctx context.Context, onChange func()) {
	if f.notify == nil {
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.notify:
			onChange()
		}
	}
}

func (f *fakeOverlaySource) set(fn func(f *fakeOverlaySource)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func TestOverlayManagerStartupFromDiskWhenRedisDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels-overlay.json")
	if err := saveOverlayCache(path, &labelOverlay{Version: 7, Source: overlaySourceRedis, Services: map[string]map[string]string{
		"web": {labelWeight: "5", labelHost: "bad.example"},
	}}); err != nil {
		t.Fatal(err)
	}
	m := newOverlayManager(&fakeOverlaySource{err: errors.New("dial tcp 100.1.2.3:6379: refused")}, path)
	m.Init(context.Background())
	cur := m.Current()
	if cur == nil || cur.Source != overlaySourceDisk || cur.Version != 7 || cur.Services["web"][labelWeight] != "5" {
		t.Fatalf("current = %+v", cur)
	}
	if _, ok := cur.Services["web"][labelHost]; ok {
		t.Error("invalid key from disk must be dropped")
	}
	st := m.status()
	if st.RedisOK || st.Source != overlaySourceDisk {
		t.Errorf("status = %+v", st)
	}
}

func TestOverlayManagerRedisErrorKeepsOverlay(t *testing.T) {
	src := &fakeOverlaySource{version: 3, exists: true, services: map[string]map[string]string{"web": {labelWeight: "5"}}}
	m := newOverlayManager(src, filepath.Join(t.TempDir(), "c.json"))
	m.Init(context.Background())
	if m.Current().Version != 3 || !m.redisOK.Load() {
		t.Fatalf("init = %+v", m.Current())
	}
	src.set(func(f *fakeOverlaySource) { f.err = errors.New("boom") })
	changed, err := m.check(context.Background())
	if err == nil || changed {
		t.Fatalf("check = %v, %v", changed, err)
	}
	if m.Current().Services["web"][labelWeight] != "5" || m.redisOK.Load() {
		t.Errorf("overlay must be kept with redis_ok=false: %+v ok=%v", m.Current(), m.redisOK.Load())
	}
}

func TestOverlayManagerWipedKeepsOverlay(t *testing.T) {
	src := &fakeOverlaySource{version: 57, exists: true, services: map[string]map[string]string{"web": {labelWeight: "5"}}}
	m := newOverlayManager(src, filepath.Join(t.TempDir(), "c.json"))
	m.Init(context.Background())
	src.set(func(f *fakeOverlaySource) { f.exists, f.version, f.services = false, 0, nil })
	for i := 0; i < 3; i++ {
		if changed, err := m.check(context.Background()); err != nil || changed {
			t.Fatalf("wiped check %d = %v, %v", i, changed, err)
		}
	}
	cur := m.Current()
	if cur.Services["web"][labelWeight] != "5" || cur.Source != overlaySourceDisk || cur.Version != 57 {
		t.Fatalf("wiped must keep overlay as source=disk: %+v", cur)
	}
	// Reseed restarts the counter at 1: != not > must pick it up.
	src.set(func(f *fakeOverlaySource) {
		f.exists, f.version, f.services = true, 1, map[string]map[string]string{"web": {labelWeight: "6"}}
	})
	if changed, err := m.check(context.Background()); err != nil || !changed {
		t.Fatalf("reseed check = %v, %v", changed, err)
	}
	if cur := m.Current(); cur.Version != 1 || cur.Source != overlaySourceRedis || cur.Services["web"][labelWeight] != "6" {
		t.Fatalf("after reseed = %+v", cur)
	}
}

func TestOverlayManagerWipedEmpty(t *testing.T) {
	m := newOverlayManager(&fakeOverlaySource{}, "")
	m.Init(context.Background())
	if cur := m.Current(); cur == nil || cur.Source != overlaySourceRedis || len(cur.Services) != 0 {
		t.Fatalf("empty wiped = %+v", cur)
	}
}

func TestOverlayManagerVersionBumpRefreshesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	src := &fakeOverlaySource{version: 1, exists: true, services: map[string]map[string]string{}, notify: make(chan struct{}, 4)}
	m := newOverlayManager(src, path)
	m.pollEvery = 10 * time.Millisecond
	m.Init(context.Background())
	var refreshes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, func() { refreshes.Add(1) }); close(done) }()

	time.Sleep(50 * time.Millisecond)
	if n := refreshes.Load(); n != 0 {
		t.Fatalf("refreshes with no change = %d", n)
	}
	src.set(func(f *fakeOverlaySource) {
		f.version = 2
		f.services = map[string]map[string]string{"web": {labelWeight: "4"}}
	})
	src.notify <- struct{}{}
	src.notify <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for refreshes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", n)
	}
	disk := loadOverlayCache(path)
	if disk == nil || disk.Version != 2 || disk.Services["web"][labelWeight] != "4" {
		t.Fatalf("disk cache = %+v", disk)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file left behind: %v", err)
	}
}

func TestSaveOverlayCacheAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "c.json")
	for v := uint64(1); v <= 3; v++ {
		if err := saveOverlayCache(path, &labelOverlay{Version: v, Services: map[string]map[string]string{"web": {labelWeight: "2"}}}); err != nil {
			t.Fatal(err)
		}
	}
	got := loadOverlayCache(path)
	if got == nil || got.Version != 3 || got.Source != overlaySourceDisk {
		t.Fatalf("got %+v", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loadOverlayCache(path) != nil {
		t.Error("corrupt cache must load as nil")
	}
}

func TestLabelsHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	labelsHandler(nil)(rec, httptest.NewRequest("GET", "/labels", nil))
	var off labelsStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &off); err != nil {
		t.Fatal(err)
	}
	if off.Source != overlaySourceNone || off.RedisOK || off.LoadedAt != nil || len(off.Services) != 0 {
		t.Errorf("off status = %+v", off)
	}

	m := newOverlayManager(&fakeOverlaySource{version: 4, exists: true, services: map[string]map[string]string{"web": {labelWeight: "3"}}}, "")
	m.Init(context.Background())
	rec = httptest.NewRecorder()
	labelsHandler(m)(rec, httptest.NewRequest("GET", "/labels", nil))
	body := rec.Body.String()
	var on labelsStatus
	if err := json.Unmarshal([]byte(body), &on); err != nil {
		t.Fatal(err)
	}
	if on.Version != 4 || on.Source != overlaySourceRedis || !on.RedisOK || on.LoadedAt == nil || on.Services["web"][labelWeight] != "3" {
		t.Errorf("on status = %s", body)
	}
	for _, bad := range []string{"6379", "password", "addr"} {
		if strings.Contains(body, bad) {
			t.Errorf("/labels leaks %q: %s", bad, body)
		}
	}
}

func TestOverlayRace(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		container("1", "web-1", "running", map[string]string{labelEnable: "true", labelHost: "web.example", labelPort: "80", labelService: "web"}, map[string]string{managedNetwork: "10.0.0.1"}),
	))
	src := &fakeOverlaySource{version: 1, exists: true, services: map[string]map[string]string{"web": {labelWeight: "2"}}}
	m := newOverlayManager(src, filepath.Join(t.TempDir(), "c.json"))
	m.Init(context.Background())
	router := &Router{}
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			v := uint64(i + 2)
			src.set(func(f *fakeOverlaySource) {
				f.version = v
				f.services = map[string]map[string]string{"web": {labelWeight: "3", labelSticky: "true"}}
			})
			_, _ = m.check(ctx)
		}
	}()
	for j := 0; j < 2; j++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				groups, _, err := assembleGroupsWithOverlay(ctx, dc, "", m.Current())
				if err != nil {
					t.Error(err)
					return
				}
				router.Set(groups)
				_ = m.status()
			}
		}()
	}
	wg.Wait()
}

// TestRedisOverlaySourceReal runs against a real Redis when REDIS_TEST_ADDR
// is set. It uses its own key names, so it doesn't touch a live index.
func TestRedisOverlaySourceReal(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Username: os.Getenv("REDIS_TEST_USERNAME"), Password: os.Getenv("REDIS_TEST_PASSWORD"), DB: 15})
	t.Cleanup(func() { client.Close() })
	ctx := context.Background()
	keys := []string{labels.RedisVersion, labels.RedisIndex, labels.RedisServiceKey("ovtest")}
	if n, _ := client.Exists(ctx, keys...).Result(); n != 0 {
		t.Skip("DB 15 already has pmgr:labels keys; refusing to clobber")
	}
	t.Cleanup(func() { client.Del(ctx, keys...) })
	src := &redisOverlaySource{client: client}
	if _, wiped, err := src.Load(ctx); err != nil || !wiped {
		t.Fatalf("empty Load = wiped %v err %v", wiped, err)
	}
	client.Set(ctx, labels.RedisVersion, 9, 0)
	client.SAdd(ctx, labels.RedisIndex, "ovtest", "index")
	client.HSet(ctx, labels.RedisServiceKey("ovtest"), labelWeight, "4", labelHost, "evil")
	got, wiped, err := src.Load(ctx)
	if err != nil || wiped {
		t.Fatal(err, wiped)
	}
	want := map[string]map[string]string{"ovtest": {labelWeight: "4"}}
	if got.Version != 9 || !reflect.DeepEqual(got.Services, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseCacheTTLParity(t *testing.T) {
	for _, s := range []string{"", "0", "false", "off", "OFF", " 5s ", "1m30s", "-1s", "junk"} {
		d1, e1 := parseCacheTTL(s)
		d2, e2 := labels.ParseCacheTTL(s)
		if d1 != d2 || (e1 == nil) != (e2 == nil) {
			t.Errorf("%q: proxy=%v,%v labels=%v,%v", s, d1, e1, d2, e2)
		}
		if err := labels.ValidateValue(labelCache, s); s != "" && (err == nil) != (e1 == nil) {
			t.Errorf("%q: validator disagrees with parser: %v vs %v", s, err, e1)
		}
	}
}

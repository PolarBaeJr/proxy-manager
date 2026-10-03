// Central labels overlay (Phase 1a of docs/CENTRAL_LABELS_PLAN.md): for a
// service adopted into Redis, its managed proxy.* keys come from Redis
// instead of the containers' own labels. Read-only here — the dashboard is
// the only writer. Fail-static: on any Redis trouble the last good overlay
// (in memory, then the disk cache) keeps applying.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/labels"
	"github.com/redis/go-redis/v9"
)

const (
	overlaySourceRedis = "redis"
	overlaySourceDisk  = "disk"
	overlaySourceNone  = "none"

	overlayPollInterval = 5 * time.Second
	overlayMaxBackoff   = 30 * time.Second
	overlayInitTimeout  = 2 * time.Second
	overlayCheckTimeout = 5 * time.Second
)

// labelOverlay is one immutable snapshot of every adopted service's managed
// keys. Never mutated after it is published through overlayManager.
type labelOverlay struct {
	Version  uint64                       `json:"version"`
	Source   string                       `json:"source"`
	LoadedAt time.Time                    `json:"loaded_at"`
	Services map[string]map[string]string `json:"services"`
}

var managedLabelKeys = labels.ManagedKeys()

// applyLabelOverlay returns containers with each adopted service's managed
// keys replaced by the overlay: present → set, absent → deleted. Only
// registry-managed keys are ever touched, so identity, auth, proxy.service
// and proxy.ab.* labels pass through unchanged. The input slice and its
// label maps are never mutated (they may be shared).
func applyLabelOverlay(cs []dockerContainer, ov *labelOverlay) []dockerContainer {
	if ov == nil || len(ov.Services) == 0 {
		return cs
	}
	out := make([]dockerContainer, len(cs))
	copy(out, cs)
	for i := range out {
		svc := out[i].Labels[labels.ServiceKey]
		m, ok := ov.Services[svc]
		if svc == "" || !ok {
			continue
		}
		nl := make(map[string]string, len(out[i].Labels)+len(m))
		for k, v := range out[i].Labels {
			nl[k] = v
		}
		for _, k := range managedLabelKeys {
			if v, ok := m[k]; ok {
				nl[k] = v
			} else {
				delete(nl, k)
			}
		}
		out[i].Labels = nl
	}
	return out
}

var (
	overlayWarnMu   sync.Mutex
	overlayWarnSeen = map[string]bool{}
)

func overlayWarnOnce(key, format string, args ...any) {
	overlayWarnMu.Lock()
	defer overlayWarnMu.Unlock()
	if overlayWarnSeen[key] {
		return
	}
	overlayWarnSeen[key] = true
	log.Printf(format, args...)
}

// sanitizeOverlayServices re-validates everything read from Redis or disk.
// An invalid value is dropped, which under the authoritative-overlay rule
// means "unset" for that key.
func sanitizeOverlayServices(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for svc, m := range in {
		if !labels.ValidService(svc) {
			overlayWarnOnce("svc|"+svc, "labels overlay: ignoring invalid service name %q", svc)
			continue
		}
		clean := make(map[string]string, len(m))
		for k, v := range m {
			if err := labels.ValidateValue(k, v); err != nil {
				overlayWarnOnce(svc+"|"+k+"|"+v, "labels overlay: %s: dropping %v", svc, err)
				continue
			}
			clean[k] = v
		}
		out[svc] = clean
	}
	return out
}

// overlaySource is where the overlay comes from. Load reports wiped=true
// when the global version key is missing (Redis lost its data). Version is
// the cheap poll; Watch blocks until ctx is done, calling onChange on every
// change notification.
type overlaySource interface {
	Load(ctx context.Context) (ov *labelOverlay, wiped bool, err error)
	Version(ctx context.Context) (v uint64, exists bool, err error)
	Watch(ctx context.Context, onChange func())
}

type redisOverlaySource struct {
	client *redis.Client
}

func (s *redisOverlaySource) Version(ctx context.Context) (uint64, bool, error) {
	v, err := s.client.Get(ctx, labels.RedisVersion).Uint64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func (s *redisOverlaySource) Load(ctx context.Context) (*labelOverlay, bool, error) {
	// Version first: a write landing mid-load leaves us with newer data
	// under an older version, and the next poll reloads.
	v, exists, err := s.Version(ctx)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, true, nil
	}
	members, err := s.client.SMembers(ctx, labels.RedisIndex).Result()
	if err != nil {
		return nil, false, err
	}
	pipe := s.client.Pipeline()
	cmds := map[string]*redis.MapStringStringCmd{}
	for _, svc := range members {
		if !labels.ValidService(svc) {
			overlayWarnOnce("svc|"+svc, "labels overlay: ignoring invalid service name %q", svc)
			continue
		}
		cmds[svc] = pipe.HGetAll(ctx, labels.RedisServiceKey(svc))
	}
	if len(cmds) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, false, err
		}
	}
	raw := make(map[string]map[string]string, len(cmds))
	for svc, cmd := range cmds {
		raw[svc] = cmd.Val()
	}
	return &labelOverlay{Version: v, Source: overlaySourceRedis, LoadedAt: time.Now(), Services: sanitizeOverlayServices(raw)}, false, nil
}

// Watch relies on go-redis re-subscribing on its own after a disconnect;
// it never surfaces errors, so redis_ok is driven by the version poll.
func (s *redisOverlaySource) Watch(ctx context.Context, onChange func()) {
	ps := s.client.Subscribe(ctx, labels.RedisChannel)
	defer ps.Close()
	ch := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			onChange()
		}
	}
}

// loadOverlayCache reads the disk cache. Missing is normal (first boot);
// corruption logs and returns nil. Never fatal.
func loadOverlayCache(path string) *labelOverlay {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("labels overlay: read %s: %v", path, err)
		}
		return nil
	}
	var ov labelOverlay
	if err := json.Unmarshal(b, &ov); err != nil {
		log.Printf("labels overlay: parse %s: %v (ignoring cache)", path, err)
		return nil
	}
	ov.Source = overlaySourceDisk
	ov.Services = sanitizeOverlayServices(ov.Services)
	return &ov
}

// saveOverlayCache writes atomically (sibling tmp + rename), like
// saveMetricsState.
func saveOverlayCache(path string, ov *labelOverlay) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// overlayManager owns the live overlay. Current() is lock-free for the
// refresh path; check() is serialised so a pub/sub message and a poll tick
// can't both reload the same version.
type overlayManager struct {
	src       overlaySource
	cachePath string
	cur       atomic.Pointer[labelOverlay]
	redisOK   atomic.Bool

	mu          sync.Mutex
	wipedLogged bool

	pollEvery  time.Duration
	maxBackoff time.Duration
}

func newOverlayManager(src overlaySource, cachePath string) *overlayManager {
	return &overlayManager{src: src, cachePath: cachePath, pollEvery: overlayPollInterval, maxBackoff: overlayMaxBackoff}
}

// Current is nil-safe so main can pass a nil manager when Redis is off.
func (m *overlayManager) Current() *labelOverlay {
	if m == nil {
		return nil
	}
	return m.cur.Load()
}

// Init loads the disk cache, then syncs from Redis with a short timeout.
// Runs before the first refresh(), so it never calls refresh itself.
func (m *overlayManager) Init(ctx context.Context) {
	if ov := loadOverlayCache(m.cachePath); ov != nil {
		m.cur.Store(ov)
		log.Printf("labels overlay: loaded %d service(s) at version %d from %s", len(ov.Services), ov.Version, m.cachePath)
	}
	ictx, cancel := context.WithTimeout(ctx, overlayInitTimeout)
	defer cancel()
	if _, err := m.check(ictx); err != nil {
		log.Printf("labels overlay: initial Redis sync failed (%v) — using %s", err, m.sourceName())
	}
}

func (m *overlayManager) sourceName() string {
	if ov := m.Current(); ov != nil {
		return ov.Source
	}
	return overlaySourceNone
}

// check reloads when Redis's version differs from the current overlay's (not
// "is greater": a reseed after a wipe restarts the counter at 1). Reports
// whether the routing-relevant overlay changed.
func (m *overlayManager) check(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.cur.Load()
	v, exists, err := m.src.Version(ctx)
	if err != nil {
		m.redisOK.Store(false)
		return false, err
	}
	if exists {
		m.wipedLogged = false
		if cur != nil && cur.Source == overlaySourceRedis && cur.Version == v {
			m.redisOK.Store(true)
			return false, nil
		}
	}
	var ov *labelOverlay
	wiped := !exists
	if exists {
		ov, wiped, err = m.src.Load(ctx)
		if err != nil {
			m.redisOK.Store(false)
			return false, err
		}
	}
	m.redisOK.Store(true)
	if wiped {
		return m.handleWipedLocked(cur), nil
	}
	m.cur.Store(ov)
	if err := saveOverlayCache(m.cachePath, ov); err != nil {
		log.Printf("labels overlay: save %s: %v", m.cachePath, err)
	}
	log.Printf("labels overlay: version %d, %d service(s) from Redis", ov.Version, len(ov.Services))
	return true, nil
}

// handleWipedLocked: Redis has no version key. A non-empty overlay is kept
// (marked source=disk so the dashboard can reseed from it) rather than
// silently reverting every adopted service to its compose labels.
func (m *overlayManager) handleWipedLocked(cur *labelOverlay) bool {
	if cur != nil && len(cur.Services) > 0 {
		if !m.wipedLogged {
			m.wipedLogged = true
			log.Printf("labels overlay: WARNING Redis has no %s (wiped?) — KEEPING the last overlay (version %d, %d service(s)) until the dashboard reseeds it", labels.RedisVersion, cur.Version, len(cur.Services))
		}
		if cur.Source != overlaySourceDisk {
			kept := *cur
			kept.Source = overlaySourceDisk
			m.cur.Store(&kept)
		}
		return false
	}
	// Nothing to keep: swap in an empty redis-sourced overlay so /labels stops
	// reporting a stale source. Routing is unchanged (empty before and after),
	// hence no refresh.
	if cur == nil || cur.Source != overlaySourceRedis || cur.Version != 0 {
		m.cur.Store(&labelOverlay{Source: overlaySourceRedis, LoadedAt: time.Now(), Services: map[string]map[string]string{}})
	}
	return false
}

// Run watches pub/sub and polls the version, calling refresh once per
// observed change. Errors keep the current overlay and back off to
// maxBackoff.
func (m *overlayManager) Run(ctx context.Context, refresh func()) {
	notify := make(chan struct{}, 1)
	go m.src.Watch(ctx, func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	})
	delay := m.pollEvery
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-timer.C:
		}
		wasOK := m.redisOK.Load()
		cctx, cancel := context.WithTimeout(ctx, overlayCheckTimeout)
		changed, err := m.check(cctx)
		cancel()
		if err != nil {
			if wasOK {
				log.Printf("labels overlay: Redis unavailable (%v) — keeping current overlay (%s)", err, m.sourceName())
			}
			delay = min(delay*2, m.maxBackoff)
		} else {
			if !wasOK {
				log.Printf("labels overlay: Redis reachable again")
			}
			delay = m.pollEvery
			if changed {
				refresh()
			}
		}
		timer.Reset(delay)
	}
}

// labelsStatus is the GET /labels body. Deliberately carries no Redis
// address, credentials or error text.
type labelsStatus struct {
	Version  uint64                       `json:"version"`
	Source   string                       `json:"source"`
	LoadedAt *time.Time                   `json:"loaded_at"`
	RedisOK  bool                         `json:"redis_ok"`
	Services map[string]map[string]string `json:"services"`
}

func (m *overlayManager) status() labelsStatus {
	st := labelsStatus{Source: overlaySourceNone, Services: map[string]map[string]string{}}
	if m == nil {
		return st
	}
	st.RedisOK = m.redisOK.Load()
	if ov := m.Current(); ov != nil {
		st.Version, st.Source = ov.Version, ov.Source
		t := ov.LoadedAt
		st.LoadedAt = &t
		if ov.Services != nil {
			st.Services = ov.Services
		}
	}
	return st
}

func labelsHandler(m *overlayManager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.status())
	}
}

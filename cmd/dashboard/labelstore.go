// Central labels store (Phase 1b of docs/CENTRAL_LABELS_PLAN.md): the
// dashboard is the only writer of the pmgr:labels:* keys the proxies overlay
// onto their containers. Every write goes through one Lua script (CAS on the
// service's meta.version, request_id replay, prev, DEL+HSET, HINCRBY, SADD,
// INCR global, PUBLISH). Reads on hot paths (effectiveLabels) come from an
// in-memory snapshot fed by pub/sub plus a 5s version poll. None of these
// keys ever gets a TTL: a partially evicted hash would read as "every
// managed key unset" and could not be told apart from a real unset.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/labels"
	"github.com/redis/go-redis/v9"
)

const (
	labelsPollInterval = 5 * time.Second
	labelsMaxBackoff   = 30 * time.Second
	labelsCheckTimeout = 5 * time.Second
	labelsReqIDRing    = 32
)

var managedLabelKeys = labels.ManagedKeys()

// errLabelsUnavailable: Redis could not be reached (or answered with an
// error). Nothing was written.
var errLabelsUnavailable = errors.New("labels store unavailable (Redis unreachable) — no change made")

// errLabelsWiped: pmgr:labels:version vanished between the wipe check and
// the write. The script refused, so nothing was written.
var errLabelsWiped = errors.New("labels store has no " + labels.RedisVersion + " (Redis wiped?) — no change made; retry to reseed first")

// errLabelsVersionConflict is a stale if_version.
type errLabelsVersionConflict struct{ Current uint64 }

func (e errLabelsVersionConflict) Error() string {
	return fmt.Sprintf("labels changed since if_version (current version %d) — re-read and retry", e.Current)
}

// labelSnapshot is one immutable view of every adopted service's managed
// keys. Never mutated after it is published.
type labelSnapshot struct {
	Version  uint64
	LoadedAt time.Time
	Services map[string]map[string]string
	// Wiped: kept from before Redis lost pmgr:labels:version. Its Version
	// must never match a post-reseed counter by coincidence.
	Wiped bool
}

// labelRecord is one adopted service as stored, read fresh from Redis.
type labelRecord struct {
	Version   uint64                       `json:"version"`
	Labels    map[string]string            `json:"labels"`
	AdoptedAt string                       `json:"adopted_at,omitempty"`
	AdoptedBy string                       `json:"adopted_by,omitempty"`
	UpdatedAt string                       `json:"updated_at,omitempty"`
	UpdatedBy string                       `json:"updated_by,omitempty"`
	Imported  map[string]map[string]string `json:"imported,omitempty"`
	Prev      map[string]string            `json:"prev,omitempty"`
}

// labelWrite replaces svc's whole managed map, compare-and-swapped on
// IfVersion (0 = not adopted yet). AllowInit lets the script create a
// missing global version key; only a reseed or a genuine first write sets it.
type labelWrite struct {
	Service   string
	IfVersion uint64
	RequestID string
	Labels    map[string]string
	Actor     string
	Imported  map[string]map[string]string
	AllowInit bool
}

type labelWriteResult struct {
	Version  uint64
	Global   uint64
	Replayed bool
}

// labelStore is the dashboard's view of the central labels. Get/Index/
// GlobalVersion/Write always go to Redis; Snapshot is the in-memory copy for
// hot paths and is nil until the first successful load.
type labelStore interface {
	Get(ctx context.Context, svc string) (labelRecord, bool, error)
	Write(ctx context.Context, w labelWrite) (labelWriteResult, error)
	Index(ctx context.Context) ([]string, error)
	GlobalVersion(ctx context.Context) (uint64, bool, error)
	Snapshot() *labelSnapshot
	// EverLoaded: at least one successful round-trip to Redis since start.
	// Until then adoption status is unknown and the snapshot is empty.
	EverLoaded() bool
	RedisOK() bool
	// Refresh reloads the snapshot now if the global version moved.
	Refresh(ctx context.Context)
}

// labelsWriteScript: KEYS = hash, meta, index, global version. ARGV = svc,
// if_version, request_id, labels JSON object, now, actor, imported JSON
// ("" = none), channel, allow_init ("1"), ring size.
var labelsWriteScript = redis.NewScript(`
local meta = KEYS[2]
local rid = ARGV[3]
if rid ~= '' then
  local ring = redis.call('HGET', meta, 'req_ids')
  if ring then
    local ok, list = pcall(cjson.decode, ring)
    if ok and type(list) == 'table' then
      for _, e in ipairs(list) do
        if e.request_id == rid then return {'replay', tostring(e.version)} end
      end
    end
  end
end
if redis.call('EXISTS', KEYS[4]) == 0 and ARGV[9] ~= '1' then
  return {'wiped'}
end
local cur = tonumber(redis.call('HGET', meta, 'version') or '0') or 0
if cur ~= tonumber(ARGV[2]) then return {'conflict', tostring(cur)} end
local old = redis.call('HGETALL', KEYS[1])
local prev = {}
for i = 1, #old, 2 do prev[old[i]] = old[i + 1] end
local newm = cjson.decode(ARGV[4])
redis.call('DEL', KEYS[1])
local args = {}
for k, v in pairs(newm) do
  table.insert(args, k)
  table.insert(args, v)
end
if #args > 0 then redis.call('HSET', KEYS[1], unpack(args)) end
local v = redis.call('HINCRBY', meta, 'version', 1)
redis.call('HSET', meta, 'prev', cjson.encode(prev), 'updated_at', ARGV[5], 'updated_by', ARGV[6])
if cur == 0 then
  redis.call('HSET', meta, 'adopted_at', ARGV[5], 'adopted_by', ARGV[6])
  if ARGV[7] ~= '' then redis.call('HSET', meta, 'imported', ARGV[7]) end
end
if rid ~= '' then
  local list = {}
  local ring = redis.call('HGET', meta, 'req_ids')
  if ring then
    local ok, l = pcall(cjson.decode, ring)
    if ok and type(l) == 'table' then list = l end
  end
  table.insert(list, {request_id = rid, version = v})
  while #list > tonumber(ARGV[10]) do table.remove(list, 1) end
  redis.call('HSET', meta, 'req_ids', cjson.encode(list))
end
redis.call('SADD', KEYS[3], ARGV[1])
local g
if redis.call('EXISTS', KEYS[4]) == 0 then
  redis.call('SET', KEYS[4], 1)
  g = 1
else
  g = redis.call('INCR', KEYS[4])
end
redis.call('PUBLISH', ARGV[8], ARGV[1] .. ' ' .. g)
return {'ok', tostring(v), tostring(g)}
`)

type redisLabelStore struct {
	client     *redis.Client
	cur        atomic.Pointer[labelSnapshot]
	everLoaded atomic.Bool
	redisOK    atomic.Bool

	mu          sync.Mutex
	wipedLogged bool

	pollEvery  time.Duration
	maxBackoff time.Duration
}

func newRedisLabelStore(client *redis.Client) *redisLabelStore {
	return &redisLabelStore{client: client, pollEvery: labelsPollInterval, maxBackoff: labelsMaxBackoff}
}

func (s *redisLabelStore) Snapshot() *labelSnapshot { return s.cur.Load() }
func (s *redisLabelStore) EverLoaded() bool         { return s.everLoaded.Load() }
func (s *redisLabelStore) RedisOK() bool            { return s.redisOK.Load() }

func (s *redisLabelStore) GlobalVersion(ctx context.Context) (uint64, bool, error) {
	v, err := s.client.Get(ctx, labels.RedisVersion).Uint64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func (s *redisLabelStore) Index(ctx context.Context) ([]string, error) {
	members, err := s.client.SMembers(ctx, labels.RedisIndex).Result()
	if err != nil {
		return nil, err
	}
	out := members[:0]
	for _, svc := range members {
		if labels.ValidService(svc) {
			out = append(out, svc)
		}
	}
	return out, nil
}

func (s *redisLabelStore) Get(ctx context.Context, svc string) (labelRecord, bool, error) {
	if !labels.ValidService(svc) {
		return labelRecord{}, false, fmt.Errorf("invalid service name %q", svc)
	}
	pipe := s.client.Pipeline()
	h := pipe.HGetAll(ctx, labels.RedisServiceKey(svc))
	m := pipe.HGetAll(ctx, labels.RedisMetaKey(svc))
	if _, err := pipe.Exec(ctx); err != nil {
		return labelRecord{}, false, err
	}
	meta := m.Val()
	v, _ := strconv.ParseUint(meta["version"], 10, 64)
	if v == 0 {
		return labelRecord{}, false, nil
	}
	rec := labelRecord{
		Version: v, Labels: sanitizeLabelMap(svc, h.Val()),
		AdoptedAt: meta["adopted_at"], AdoptedBy: meta["adopted_by"],
		UpdatedAt: meta["updated_at"], UpdatedBy: meta["updated_by"],
	}
	if raw := meta["imported"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &rec.Imported)
	}
	if raw := meta["prev"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &rec.Prev)
	}
	return rec, true, nil
}

func (s *redisLabelStore) Write(ctx context.Context, w labelWrite) (labelWriteResult, error) {
	if !labels.ValidService(w.Service) {
		return labelWriteResult{}, fmt.Errorf("invalid service name %q", w.Service)
	}
	m := w.Labels
	if m == nil {
		m = map[string]string{}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return labelWriteResult{}, err
	}
	imported := ""
	if w.Imported != nil {
		b, err := json.Marshal(w.Imported)
		if err != nil {
			return labelWriteResult{}, err
		}
		imported = string(b)
	}
	allow := "0"
	if w.AllowInit {
		allow = "1"
	}
	keys := []string{labels.RedisServiceKey(w.Service), labels.RedisMetaKey(w.Service), labels.RedisIndex, labels.RedisVersion}
	res, err := labelsWriteScript.Run(ctx, s.client, keys,
		w.Service, strconv.FormatUint(w.IfVersion, 10), w.RequestID, string(body),
		time.Now().UTC().Format(time.RFC3339), w.Actor, imported, labels.RedisChannel, allow, labelsReqIDRing).StringSlice()
	if err != nil {
		return labelWriteResult{}, fmt.Errorf("%w: %v", errLabelsUnavailable, err)
	}
	return parseLabelsWriteResult(res)
}

func parseLabelsWriteResult(res []string) (labelWriteResult, error) {
	if len(res) == 0 {
		return labelWriteResult{}, fmt.Errorf("%w: empty script result", errLabelsUnavailable)
	}
	num := func(i int) uint64 {
		if i >= len(res) {
			return 0
		}
		n, _ := strconv.ParseUint(res[i], 10, 64)
		return n
	}
	switch res[0] {
	case "ok":
		return labelWriteResult{Version: num(1), Global: num(2)}, nil
	case "replay":
		return labelWriteResult{Version: num(1), Replayed: true}, nil
	case "conflict":
		return labelWriteResult{}, errLabelsVersionConflict{Current: num(1)}
	case "wiped":
		return labelWriteResult{}, errLabelsWiped
	}
	return labelWriteResult{}, fmt.Errorf("%w: unexpected script result %q", errLabelsUnavailable, res[0])
}

// load reads every adopted service. Version first: a write landing mid-load
// leaves newer data under an older version, and the next poll reloads.
func (s *redisLabelStore) load(ctx context.Context, v uint64) (*labelSnapshot, error) {
	members, err := s.Index(ctx)
	if err != nil {
		return nil, err
	}
	pipe := s.client.Pipeline()
	cmds := make(map[string]*redis.MapStringStringCmd, len(members))
	for _, svc := range members {
		cmds[svc] = pipe.HGetAll(ctx, labels.RedisServiceKey(svc))
	}
	if len(cmds) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
	}
	out := make(map[string]map[string]string, len(cmds))
	for svc, cmd := range cmds {
		out[svc] = sanitizeLabelMap(svc, cmd.Val())
	}
	return &labelSnapshot{Version: v, LoadedAt: time.Now(), Services: out}, nil
}

var (
	labelsWarnMu   sync.Mutex
	labelsWarnSeen = map[string]bool{}
)

// sanitizeLabelMap drops (with a deduped log) anything a hand edit of Redis
// could have left invalid; dropped means unset, exactly as the proxy reads it.
func sanitizeLabelMap(svc string, in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if err := labels.ValidateValue(k, v); err != nil {
			labelsWarnMu.Lock()
			key := svc + "|" + k + "|" + v
			if !labelsWarnSeen[key] {
				labelsWarnSeen[key] = true
				log.Printf("central labels: %s: dropping %v", svc, err)
			}
			labelsWarnMu.Unlock()
			continue
		}
		out[k] = v
	}
	return out
}

// check reloads the snapshot when the global version differs from it (not
// "is greater": a reseed after a wipe restarts the counter at 1). A wiped
// Redis keeps the current snapshot — it is what a write would reseed from.
func (s *redisLabelStore) check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, exists, err := s.GlobalVersion(ctx)
	if err != nil {
		s.redisOK.Store(false)
		return err
	}
	cur := s.cur.Load()
	if !exists {
		s.redisOK.Store(true)
		s.everLoaded.Store(true)
		if cur != nil && len(cur.Services) > 0 {
			if !s.wipedLogged {
				s.wipedLogged = true
				log.Printf("central labels: WARNING Redis has no %s (wiped?) — keeping the last snapshot (version %d, %d service(s)); the next label write reseeds it", labels.RedisVersion, cur.Version, len(cur.Services))
			}
			if !cur.Wiped {
				kept := *cur
				kept.Wiped = true
				s.cur.Store(&kept)
			}
			return nil
		}
		if cur == nil {
			s.cur.Store(&labelSnapshot{LoadedAt: time.Now(), Services: map[string]map[string]string{}})
		}
		return nil
	}
	s.wipedLogged = false
	if cur != nil && !cur.Wiped && cur.Version == v && s.everLoaded.Load() {
		s.redisOK.Store(true)
		return nil
	}
	snap, err := s.load(ctx, v)
	if err != nil {
		s.redisOK.Store(false)
		return err
	}
	s.cur.Store(snap)
	s.redisOK.Store(true)
	s.everLoaded.Store(true)
	return nil
}

func (s *redisLabelStore) Refresh(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, labelsCheckTimeout)
	defer cancel()
	_ = s.check(cctx)
}

// Run watches pub/sub and polls the version. Errors keep the current
// snapshot and back off up to maxBackoff. go-redis re-subscribes on its own
// after a disconnect, so redis_ok is driven by the poll only.
func (s *redisLabelStore) Run(ctx context.Context) {
	notify := make(chan struct{}, 1)
	go func() {
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
				select {
				case notify <- struct{}{}:
				default:
				}
			}
		}
	}()
	delay := s.pollEvery
	first := true
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-timer.C:
		}
		wasOK := s.redisOK.Load()
		cctx, cancel := context.WithTimeout(ctx, labelsCheckTimeout)
		err := s.check(cctx)
		cancel()
		if err != nil {
			if wasOK || first {
				log.Printf("central labels: Redis unavailable (%v) — keeping the current snapshot", err)
			}
			delay = min(delay*2, s.maxBackoff)
		} else {
			if !wasOK {
				log.Printf("central labels: Redis reachable")
			}
			delay = s.pollEvery
		}
		first = false
		timer.Reset(delay)
	}
}

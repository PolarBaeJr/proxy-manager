// Central per-service env: the ORIGIN side.
//
// An adopted service has exactly one authoritative env, owned by the
// dashboard it was adopted on (its origin). Every replica on every host is
// created from it rather than cloned from whatever a local template
// container happens to be running, which is how spread replicas used to
// drift. This file is the origin's durable record of that env: one JSON file
// per service under dir, written atomically, versioned monotonically, with a
// one-step undo (prev) and request-id replay so a retried write can't apply
// twice.
//
// Values are secrets as often as not. Nothing in here — errors, logs — ever
// names a value; only service names, key names and versions.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultCentralEnvDir = "/data/central-env"

	centralEnvStateActive   = "active"
	centralEnvStateReverted = "reverted"
	centralEnvStateDegraded = "degraded"
	// releasing: un-adopt in progress. Creates keep resolving from the record
	// (so a scale or recreate mid-release still works) while every replica is
	// recreated without the pmgr.env.* stamp; the record is deleted last.
	centralEnvStateReleasing = "releasing"

	// centralEnvRequestRing bounds how many recent request ids a record
	// remembers for replay — enough to absorb any realistic retry storm
	// without the file growing forever.
	centralEnvRequestRing = 32
)

// centralEnvSnapshot is one prior content+version, kept for Revert.
type centralEnvSnapshot struct {
	Version   uint64                       `json:"version"`
	Base      map[string]string            `json:"base,omitempty"`
	Overrides map[string]map[string]string `json:"overrides,omitempty"`
}

type centralEnvRequest struct {
	RequestID string `json:"request_id"`
	Version   uint64 `json:"version"`
}

// centralEnvRecord is the on-disk (and in-memory) origin record for one
// service. Base values may be literal or "ref:NAME" (resolved from the
// service's secrets file at effectiveEnv time, never stored resolved).
// Overrides are keyed by peer identity (DASHBOARD_HOST) and layered over
// Base for replicas on that host only.
type centralEnvRecord struct {
	Service        string                       `json:"service"`
	Origin         string                       `json:"origin"`
	Version        uint64                       `json:"version"`
	Base           map[string]string            `json:"base,omitempty"`
	Overrides      map[string]map[string]string `json:"overrides,omitempty"`
	Prev           *centralEnvSnapshot          `json:"prev,omitempty"`
	LastRequestIDs []centralEnvRequest          `json:"last_request_ids,omitempty"`
	// ResolvedHash is a content hash of Base+Overrides (unresolved — refs
	// stay refs), so two hosts can compare "same central env?" by hash
	// without ever exchanging values.
	ResolvedHash string `json:"resolved_hash"`
	State        string `json:"state"`
	UpdatedAt    int64  `json:"updated_at"`
	UpdatedBy    string `json:"updated_by,omitempty"`
	AdoptedFrom  string `json:"adopted_from,omitempty"`
	// Adopting: the record was seeded from a live service whose replicas
	// (unstamped, often compose-created) are still being rolled onto it, so
	// the propagation job rolls foreign members too. Cleared once every host
	// converged. A flag, not a State, so revert/degrade keep their meaning
	// while an adopt is still finishing.
	Adopting bool `json:"adopting,omitempty"`
}

// centralEnvChange is a full desired-content write (PUT semantics, not a
// patch), compare-and-swapped against IfVersion.
type centralEnvChange struct {
	IfVersion uint64                       `json:"if_version"`
	RequestID string                       `json:"request_id,omitempty"`
	Base      map[string]string            `json:"base"`
	Overrides map[string]map[string]string `json:"overrides,omitempty"`
}

// centralEnvApplyResult reports what Apply did. Replayed means request_id
// matched an earlier call and nothing was written; NoOp means the content was
// already identical and the version did not move.
type centralEnvApplyResult struct {
	Version  uint64 `json:"version"`
	NoOp     bool   `json:"no_op,omitempty"`
	Replayed bool   `json:"replayed,omitempty"`
}

// errEnvVersionConflict is Apply's compare-and-swap failure — the API layer
// maps it to 409 and hands Current back so the caller can re-read and retry.
type errEnvVersionConflict struct {
	Current uint64
}

func (e errEnvVersionConflict) Error() string {
	return fmt.Sprintf("central env version conflict (current version is %d)", e.Current)
}

var (
	errCentralEnvNotFound = errors.New("service has no central env")
	errCentralEnvExists   = errors.New("service already has a central env")
	errCentralEnvNoPrev   = errors.New("central env has no previous version to revert to")
)

// errCentralEnvBroken marks a service whose record exists on disk but could
// not be loaded. It fails CLOSED: the service is still treated as centrally
// managed, and every create for it is refused until an operator repairs the
// file (see Delete for why deleting alone isn't enough) — never silently
// treated as unmanaged (which would let
// replicas quietly fall back to whatever local env a template carries).
type errCentralEnvBroken struct {
	Service string
}

func (e errCentralEnvBroken) Error() string {
	return fmt.Sprintf("central env record for %q is unreadable — refusing to guess; repair the file", e.Service)
}

type centralEnvStore struct {
	dir    string
	mu     sync.Mutex
	items  map[string]*centralEnvRecord
	broken map[string]bool
}

// loadCentralEnvStore creates dir (0700) if needed and loads every
// <service>.json in it. A file that fails to parse, or whose service field
// disagrees with its file name, is recorded as broken rather than failing
// the whole load — one bad record must not take every other service down.
func loadCentralEnvStore(dir string) (*centralEnvStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &centralEnvStore{dir: dir, items: map[string]*centralEnvRecord{}, broken: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		svc := strings.TrimSuffix(name, ".json")
		if !validServiceName(svc) {
			log.Printf("central env: ignoring %s (not a valid service name)", name)
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			log.Printf("central env: %s unreadable — %q fails closed until fixed", name, svc)
			s.broken[svc] = true
			continue
		}
		var rec centralEnvRecord
		// The decode error itself is deliberately not logged: a syntax error
		// can quote a fragment of the file, and the file holds env values.
		if err := json.Unmarshal(data, &rec); err != nil || rec.Service != svc || rec.Version == 0 {
			log.Printf("central env: %s is corrupt — %q fails closed until fixed", name, svc)
			s.broken[svc] = true
			continue
		}
		s.items[svc] = &rec
	}
	return s, nil
}

func (s *centralEnvStore) path(svc string) string {
	return filepath.Join(s.dir, svc+".json")
}

// Counts reports how many records loaded cleanly and how many are broken —
// for the startup log line, which must carry counts only.
func (s *centralEnvStore) Counts() (ok, broken int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items), len(s.broken)
}

// Has reports whether svc is centrally managed by this store at all —
// including a broken record, which still counts as managed (fail closed).
func (s *centralEnvStore) Has(svc string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[svc] != nil || s.broken[svc]
}

// Get returns a deep copy of svc's record. A broken record returns
// errCentralEnvBroken; a missing one returns ok=false.
func (s *centralEnvStore) Get(svc string) (centralEnvRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[svc] {
		return centralEnvRecord{}, true, errCentralEnvBroken{Service: svc}
	}
	rec := s.items[svc]
	if rec == nil {
		return centralEnvRecord{}, false, nil
	}
	return copyCentralEnvRecord(rec), true, nil
}

// Create writes svc's first record at version 1.
func (s *centralEnvStore) Create(svc, origin string, base map[string]string, overrides map[string]map[string]string, actor, adoptedFrom string) (uint64, error) {
	return s.create(svc, origin, base, overrides, actor, adoptedFrom, false, "")
}

// CreateAdopted is Create for an adopt: the record starts Adopting and
// remembers requestID, so a retried adopt replays instead of refusing on its
// own record.
func (s *centralEnvStore) CreateAdopted(svc, origin string, base map[string]string, overrides map[string]map[string]string, actor, adoptedFrom, requestID string) (uint64, error) {
	return s.create(svc, origin, base, overrides, actor, adoptedFrom, true, requestID)
}

func (s *centralEnvStore) create(svc, origin string, base map[string]string, overrides map[string]map[string]string, actor, adoptedFrom string, adopting bool, requestID string) (uint64, error) {
	if !validServiceName(svc) {
		return 0, fmt.Errorf("invalid service name %q", svc)
	}
	if err := validateCentralEnvContent(base, overrides); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[svc] {
		return 0, errCentralEnvBroken{Service: svc}
	}
	if s.items[svc] != nil {
		return 0, errCentralEnvExists
	}
	rec := &centralEnvRecord{
		Service:     svc,
		Origin:      origin,
		Version:     1,
		Base:        copyStringMap(base),
		Overrides:   copyOverrides(overrides),
		State:       centralEnvStateActive,
		UpdatedAt:   time.Now().Unix(),
		UpdatedBy:   actor,
		AdoptedFrom: adoptedFrom,
		Adopting:    adopting,
	}
	if requestID != "" {
		rec.LastRequestIDs = appendRequestID(nil, requestID, 1)
	}
	rec.ResolvedHash = centralEnvContentHash(rec.Base, rec.Overrides)
	if err := s.persist(rec); err != nil {
		return 0, err
	}
	s.items[svc] = rec
	return rec.Version, nil
}

// Apply replaces svc's content with change, provided change.IfVersion still
// matches. Order matters: a replayed request_id wins before the CAS check (a
// retry of a request that already succeeded would otherwise see its own
// write as a conflict), and identical content is a success that doesn't bump
// the version (so an idempotent re-submit doesn't restart every replica).
func (s *centralEnvStore) Apply(svc string, change centralEnvChange, actor string) (centralEnvApplyResult, error) {
	if err := validateCentralEnvContent(change.Base, change.Overrides); err != nil {
		return centralEnvApplyResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[svc] {
		return centralEnvApplyResult{}, errCentralEnvBroken{Service: svc}
	}
	cur := s.items[svc]
	if cur == nil {
		return centralEnvApplyResult{}, errCentralEnvNotFound
	}
	if change.RequestID != "" {
		for _, r := range cur.LastRequestIDs {
			if r.RequestID == change.RequestID {
				return centralEnvApplyResult{Version: r.Version, Replayed: true}, nil
			}
		}
	}
	if change.IfVersion != cur.Version {
		return centralEnvApplyResult{}, errEnvVersionConflict{Current: cur.Version}
	}
	hash := centralEnvContentHash(change.Base, change.Overrides)
	next := copyCentralEnvRecord(cur)
	if hash == cur.ResolvedHash {
		if change.RequestID == "" {
			return centralEnvApplyResult{Version: cur.Version, NoOp: true}, nil
		}
		next.LastRequestIDs = appendRequestID(next.LastRequestIDs, change.RequestID, cur.Version)
		if err := s.persist(&next); err != nil {
			return centralEnvApplyResult{}, err
		}
		s.items[svc] = &next
		return centralEnvApplyResult{Version: cur.Version, NoOp: true}, nil
	}
	next.Prev = &centralEnvSnapshot{Version: cur.Version, Base: copyStringMap(cur.Base), Overrides: copyOverrides(cur.Overrides)}
	next.Version = cur.Version + 1
	next.Base = copyStringMap(change.Base)
	next.Overrides = copyOverrides(change.Overrides)
	next.ResolvedHash = hash
	next.State = keepTransitionState(cur.State)
	next.UpdatedAt = time.Now().Unix()
	next.UpdatedBy = actor
	if change.RequestID != "" {
		next.LastRequestIDs = appendRequestID(next.LastRequestIDs, change.RequestID, next.Version)
	}
	if err := s.persist(&next); err != nil {
		return centralEnvApplyResult{}, err
	}
	s.items[svc] = &next
	return centralEnvApplyResult{Version: next.Version}, nil
}

// Revert writes the previous content back as a NEW version (current+1, never
// a rewind — caches on other hosts reject a lower version) and makes the
// content being reverted away from the new prev, so a revert is itself
// revertable.
func (s *centralEnvStore) Revert(svc, reason string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[svc] {
		return 0, errCentralEnvBroken{Service: svc}
	}
	cur := s.items[svc]
	if cur == nil {
		return 0, errCentralEnvNotFound
	}
	if cur.Prev == nil {
		return 0, errCentralEnvNoPrev
	}
	next := copyCentralEnvRecord(cur)
	next.Prev = &centralEnvSnapshot{Version: cur.Version, Base: copyStringMap(cur.Base), Overrides: copyOverrides(cur.Overrides)}
	next.Version = cur.Version + 1
	next.Base = copyStringMap(cur.Prev.Base)
	next.Overrides = copyOverrides(cur.Prev.Overrides)
	next.ResolvedHash = centralEnvContentHash(next.Base, next.Overrides)
	next.State = centralEnvStateReverted
	next.UpdatedAt = time.Now().Unix()
	next.UpdatedBy = "revert: " + reason
	if err := s.persist(&next); err != nil {
		return 0, err
	}
	s.items[svc] = &next
	return next.Version, nil
}

// Services lists every service this store owns, broken records included —
// the reconcile loop walks them (a broken one just fails its resolve).
func (s *centralEnvStore) Services() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items)+len(s.broken))
	for svc := range s.items {
		out = append(out, svc)
	}
	for svc := range s.broken {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}

// Touch bumps svc's version without changing its content — for a "ref:NAME"
// whose resolved value changed underneath an unchanged record (a rotated
// secret): every replica must be recreated to pick it up, and only a version
// move tells the other hosts so. The previous content is snapshotted like any
// other change, so a Revert after a Touch is a content no-op.
func (s *centralEnvStore) Touch(svc, actor string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[svc] {
		return 0, errCentralEnvBroken{Service: svc}
	}
	cur := s.items[svc]
	if cur == nil {
		return 0, errCentralEnvNotFound
	}
	next := copyCentralEnvRecord(cur)
	next.Prev = &centralEnvSnapshot{Version: cur.Version, Base: copyStringMap(cur.Base), Overrides: copyOverrides(cur.Overrides)}
	next.Version = cur.Version + 1
	next.State = keepTransitionState(cur.State)
	next.UpdatedAt = time.Now().Unix()
	next.UpdatedBy = actor
	if err := s.persist(&next); err != nil {
		return 0, err
	}
	s.items[svc] = &next
	return next.Version, nil
}

// keepTransitionState is the state a content change leaves behind: active,
// unless a release is in flight — a rotated secret mid-release must not
// flip the record back to active and make the next job re-stamp.
func keepTransitionState(cur string) string {
	if cur == centralEnvStateReleasing {
		return cur
	}
	return centralEnvStateActive
}

// MarkDegraded records that svc's propagation could not be brought back to a
// healthy state (a revert that itself failed its health gate). Content and
// version are untouched; the state is what stops any further automatic
// attempt until an operator acts.
func (s *centralEnvStore) MarkDegraded(svc string) error {
	return s.SetState(svc, centralEnvStateDegraded)
}

// SetState records svc's lifecycle state (e.g. → releasing) without
// touching content or version.
func (s *centralEnvStore) SetState(svc, state string) error {
	return s.mutateMeta(svc, func(r *centralEnvRecord) { r.State = state })
}

// FinishAdopt clears the Adopting flag: every host converged onto the record.
func (s *centralEnvStore) FinishAdopt(svc string) error {
	return s.mutateMeta(svc, func(r *centralEnvRecord) { r.Adopting = false })
}

// HasRequestID reports whether svc's record remembers requestID — how a
// retried adopt recognizes the record it created itself.
func (s *centralEnvStore) HasRequestID(svc, requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.items[svc]
	if cur == nil || requestID == "" {
		return false
	}
	for _, r := range cur.LastRequestIDs {
		if r.RequestID == requestID {
			return true
		}
	}
	return false
}

func (s *centralEnvStore) mutateMeta(svc string, fn func(*centralEnvRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[svc] {
		return errCentralEnvBroken{Service: svc}
	}
	cur := s.items[svc]
	if cur == nil {
		return errCentralEnvNotFound
	}
	next := copyCentralEnvRecord(cur)
	fn(&next)
	next.UpdatedAt = time.Now().Unix()
	if err := s.persist(&next); err != nil {
		return err
	}
	s.items[svc] = &next
	return nil
}

// Delete drops svc's record, broken or not. A missing file is not an error.
// Deleting is NOT by itself a way out for a service whose replicas still
// carry pmgr.env.origin=<this host>: Resolve fails closed on that (labels
// claim an origin record that no longer exists), so every create stays
// refused. Recover by repairing the file, or recreate the replicas without
// the stamp first and delete after — un-adopt has to do the same.
func (s *centralEnvStore) Delete(svc string) error {
	if !validServiceName(svc) {
		return fmt.Errorf("invalid service name %q", svc)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(svc)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(s.items, svc)
	delete(s.broken, svc)
	return nil
}

// effectiveEnv is what a replica of svc on host should run: Base, with
// Overrides[host] layered on top, every "ref:NAME" resolved from svc's
// secrets file. Sorted by key so identical content always produces an
// identical Env slice. A ref that can't be resolved fails the whole call —
// creating a replica with the key silently missing is worse than refusing.
func (s *centralEnvStore) effectiveEnv(svc, host string, secrets *secretsStore) ([]string, error) {
	env, _, err := s.effectiveEnvAt(svc, host, secrets)
	return env, err
}

// effectiveEnvAt is effectiveEnv plus the version the env was built from,
// read from the same snapshot so the two can't disagree under a concurrent
// Apply.
func (s *centralEnvStore) effectiveEnvAt(svc, host string, secrets *secretsStore) ([]string, uint64, error) {
	rec, ok, err := s.Get(svc)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, errCentralEnvNotFound
	}
	merged := make(map[string]string, len(rec.Base))
	for k, v := range rec.Base {
		merged[k] = v
	}
	for k, v := range rec.Overrides[host] {
		merged[k] = v
	}
	resolved, _, err := resolveSecretRefs(svc, merged, secrets)
	if err != nil {
		return nil, 0, fmt.Errorf("central env for %q: %w", svc, err)
	}
	return envMapToSlice(resolved), rec.Version, nil
}

func (s *centralEnvStore) persist(rec *centralEnvRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(rec.Service), data, 0o600)
}

func validateCentralEnvContent(base map[string]string, overrides map[string]map[string]string) error {
	for k := range base {
		if !validEnvKey(k) {
			return fmt.Errorf("invalid env key %q", k)
		}
	}
	for host, m := range overrides {
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("override host identity must not be empty")
		}
		for k := range m {
			if !validEnvKey(k) {
				return fmt.Errorf("invalid env key %q in override for %s", k, host)
			}
		}
	}
	return nil
}

// validEnvKey mirrors the only hard constraint Docker puts on an env entry's
// key: non-empty and no "=" (splitEnvEntry would mis-split it otherwise).
func validEnvKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}

func appendRequestID(ring []centralEnvRequest, id string, version uint64) []centralEnvRequest {
	ring = append(ring, centralEnvRequest{RequestID: id, Version: version})
	if len(ring) > centralEnvRequestRing {
		ring = append([]centralEnvRequest(nil), ring[len(ring)-centralEnvRequestRing:]...)
	}
	return ring
}

// centralEnvContentHash hashes a canonical encoding of base+overrides.
// encoding/json sorts map keys, which is what makes it canonical; an empty
// and a nil map hash identically.
func centralEnvContentHash(base map[string]string, overrides map[string]map[string]string) string {
	if len(base) == 0 {
		base = nil
	}
	var ov map[string]map[string]string
	for h, m := range overrides {
		if len(m) == 0 {
			continue
		}
		if ov == nil {
			ov = map[string]map[string]string{}
		}
		ov[h] = m
	}
	data, _ := json.Marshal(struct {
		Base      map[string]string            `json:"b"`
		Overrides map[string]map[string]string `json:"o"`
	}{base, ov})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func envMapToSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyOverrides(m map[string]map[string]string) map[string]map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]map[string]string, len(m))
	for h, kv := range m {
		out[h] = copyStringMap(kv)
	}
	return out
}

func copyCentralEnvRecord(r *centralEnvRecord) centralEnvRecord {
	out := *r
	out.Base = copyStringMap(r.Base)
	out.Overrides = copyOverrides(r.Overrides)
	if r.Prev != nil {
		p := *r.Prev
		p.Base = copyStringMap(r.Prev.Base)
		p.Overrides = copyOverrides(r.Prev.Overrides)
		out.Prev = &p
	}
	out.LastRequestIDs = append([]centralEnvRequest(nil), r.LastRequestIDs...)
	return out
}

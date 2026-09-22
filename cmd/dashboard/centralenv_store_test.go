package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func newTestCentralStore(t *testing.T) *centralEnvStore {
	t.Helper()
	s, err := loadCentralEnvStore(filepath.Join(t.TempDir(), "central-env"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newTestSecrets writes a per-service secrets file (KEY=VALUE lines) and
// returns a store pointing at its directory.
func newTestSecrets(t *testing.T, service string, lines ...string) *secretsStore {
	t.Helper()
	dir := t.TempDir()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, service+".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &secretsStore{dir: dir}
}

func TestCentralEnvStoreCreatePersistsWithPerms(t *testing.T) {
	s := newTestCentralStore(t)
	v, err := s.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "alice", "tpl1")
	if err != nil || v != 1 {
		t.Fatalf("Create = %d, %v", v, err)
	}
	st, err := os.Stat(s.dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %v, %v", st.Mode().Perm(), err)
	}
	st, err = os.Stat(filepath.Join(s.dir, "app.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file perm = %v, %v", st.Mode().Perm(), err)
	}
	if _, err := s.Create("app", "dashboard-a", nil, nil, "alice", ""); !errors.Is(err, errCentralEnvExists) {
		t.Fatalf("second Create = %v, want errCentralEnvExists", err)
	}
	rec, ok, err := s.Get("app")
	if err != nil || !ok || rec.State != centralEnvStateActive || rec.AdoptedFrom != "tpl1" || rec.UpdatedBy != "alice" || rec.ResolvedHash == "" {
		t.Fatalf("Get = %+v, %v, %v", rec, ok, err)
	}
}

func TestCentralEnvStoreGetIsDeepCopy(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{"A": "1"}, map[string]map[string]string{"h": {"B": "2"}}, "", "")
	rec, _, _ := s.Get("app")
	rec.Base["A"] = "mutated"
	rec.Overrides["h"]["B"] = "mutated"
	again, _, _ := s.Get("app")
	if again.Base["A"] != "1" || again.Overrides["h"]["B"] != "2" {
		t.Fatalf("Get leaked internal maps: %+v", again)
	}
}

func TestCentralEnvStoreApplyCAS(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{"A": "1"}, nil, "", "")

	res, err := s.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"A": "2"}}, "bob")
	if err != nil || res.Version != 2 || res.NoOp || res.Replayed {
		t.Fatalf("winning Apply = %+v, %v", res, err)
	}
	_, err = s.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"A": "3"}}, "carol")
	var conflict errEnvVersionConflict
	if !errors.As(err, &conflict) || conflict.Current != 2 {
		t.Fatalf("losing Apply = %v, want errEnvVersionConflict{2}", err)
	}
	rec, _, _ := s.Get("app")
	if rec.Base["A"] != "2" || rec.UpdatedBy != "bob" || rec.Prev == nil || rec.Prev.Version != 1 || rec.Prev.Base["A"] != "1" {
		t.Fatalf("record after CAS = %+v", rec)
	}
	if _, err := s.Apply("missing", centralEnvChange{IfVersion: 1}, ""); !errors.Is(err, errCentralEnvNotFound) {
		t.Fatalf("Apply on missing = %v", err)
	}
	if _, err := s.Apply("app", centralEnvChange{IfVersion: 2, Base: map[string]string{"BAD=KEY": "x"}}, ""); err == nil {
		t.Fatal("Apply accepted a key containing '='")
	}
}

func TestCentralEnvStoreApplyRequestIDReplay(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{"A": "1"}, nil, "", "")
	first, err := s.Apply("app", centralEnvChange{IfVersion: 1, RequestID: "req-1", Base: map[string]string{"A": "2"}}, "")
	if err != nil || first.Version != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	s.Apply("app", centralEnvChange{IfVersion: 2, Base: map[string]string{"A": "3"}}, "")
	// Same request id, now-stale if_version and different body: the replay
	// must return the recorded result, not a conflict and not a new write.
	again, err := s.Apply("app", centralEnvChange{IfVersion: 1, RequestID: "req-1", Base: map[string]string{"A": "999"}}, "")
	if err != nil || !again.Replayed || again.Version != 2 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	rec, _, _ := s.Get("app")
	if rec.Version != 3 || rec.Base["A"] != "3" {
		t.Fatalf("replay wrote: %+v", rec)
	}
}

func TestCentralEnvStoreRequestRingBounded(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{}, nil, "", "")
	for i := 0; i < centralEnvRequestRing+5; i++ {
		v := uint64(i + 1)
		if _, err := s.Apply("app", centralEnvChange{IfVersion: v, RequestID: fmt.Sprintf("r%d", i), Base: map[string]string{"N": fmt.Sprint(i)}}, ""); err != nil {
			t.Fatal(err)
		}
	}
	rec, _, _ := s.Get("app")
	if len(rec.LastRequestIDs) != centralEnvRequestRing || rec.LastRequestIDs[0].RequestID != "r5" {
		t.Fatalf("ring = %d entries, first %+v", len(rec.LastRequestIDs), rec.LastRequestIDs[0])
	}
}

func TestCentralEnvStoreSameContentIsNoOp(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{"A": "1"}, map[string]map[string]string{"h": {"B": "2"}}, "", "")
	res, err := s.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"A": "1"}, Overrides: map[string]map[string]string{"h": {"B": "2"}, "empty": {}}}, "")
	if err != nil || !res.NoOp || res.Version != 1 {
		t.Fatalf("no-op Apply = %+v, %v", res, err)
	}
	rec, _, _ := s.Get("app")
	if rec.Version != 1 || rec.Prev != nil {
		t.Fatalf("no-op moved the record: %+v", rec)
	}
	// A no-op with a request id is still replayable.
	res, _ = s.Apply("app", centralEnvChange{IfVersion: 1, RequestID: "x", Base: map[string]string{"A": "1"}, Overrides: map[string]map[string]string{"h": {"B": "2"}}}, "")
	again, _ := s.Apply("app", centralEnvChange{IfVersion: 7, RequestID: "x"}, "")
	if !res.NoOp || !again.Replayed || again.Version != 1 {
		t.Fatalf("no-op replay = %+v / %+v", res, again)
	}
}

func TestCentralEnvStoreRevertIsMonotonic(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{"A": "1"}, nil, "", "")
	if _, err := s.Revert("app", "nothing yet"); !errors.Is(err, errCentralEnvNoPrev) {
		t.Fatalf("Revert with no prev = %v", err)
	}
	s.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"A": "2"}}, "")
	v, err := s.Revert("app", "bad deploy")
	if err != nil || v != 3 {
		t.Fatalf("Revert = %d, %v, want version 3", v, err)
	}
	rec, _, _ := s.Get("app")
	if rec.Base["A"] != "1" || rec.State != centralEnvStateReverted || rec.Prev.Version != 2 || rec.Prev.Base["A"] != "2" || rec.UpdatedBy != "revert: bad deploy" {
		t.Fatalf("after revert = %+v", rec)
	}
	// Reverting a revert goes forward again, to version 4 with A=2.
	v, _ = s.Revert("app", "undo")
	rec, _, _ = s.Get("app")
	if v != 4 || rec.Base["A"] != "2" {
		t.Fatalf("revert of revert = %d %+v", v, rec)
	}
}

func TestCentralEnvStoreEffectiveEnv(t *testing.T) {
	s := newTestCentralStore(t)
	sec := newTestSecrets(t, "app", "DB_PASS=hunter2", "OTHER=x")
	s.Create("app", "o", map[string]string{"A": "base", "B": "base", "PASS": "ref:DB_PASS"},
		map[string]map[string]string{"host-b": {"B": "over", "C": "only-b"}}, "", "")

	env, err := s.effectiveEnv("app", "host-b", sec)
	want := []string{"A=base", "B=over", "C=only-b", "PASS=hunter2"}
	if err != nil || !reflect.DeepEqual(env, want) {
		t.Fatalf("host-b env = %v, %v; want %v", env, err, want)
	}
	env, err = s.effectiveEnv("app", "host-a", sec)
	want = []string{"A=base", "B=base", "PASS=hunter2"}
	if err != nil || !reflect.DeepEqual(env, want) {
		t.Fatalf("host-a env = %v, %v; want %v", env, err, want)
	}

	s.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"PASS": "ref:MISSING"}}, "")
	if _, err := s.effectiveEnv("app", "host-a", sec); err == nil {
		t.Fatal("unresolvable ref did not fail")
	}
	if _, err := s.effectiveEnv("app", "host-a", nil); err == nil {
		t.Fatal("ref with no secrets store did not fail")
	}
	if _, err := s.effectiveEnv("nope", "host-a", sec); !errors.Is(err, errCentralEnvNotFound) {
		t.Fatalf("missing service = %v", err)
	}
}

func TestCentralEnvStoreCorruptFileFailsClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ce")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"service":"app","version":1,"base":{"A":`), 0o600)
	os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"service":"not-other","version":1}`), 0o600)
	good := centralEnvRecord{Service: "good", Origin: "o", Version: 4, Base: map[string]string{"A": "1"}, State: centralEnvStateActive}
	data, _ := json.Marshal(good)
	os.WriteFile(filepath.Join(dir, "good.json"), data, 0o600)

	s, err := loadCentralEnvStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, broken := s.Counts(); ok != 1 || broken != 2 {
		t.Fatalf("counts = %d ok / %d broken", ok, broken)
	}
	for _, svc := range []string{"app", "other"} {
		if !s.Has(svc) {
			t.Fatalf("%s: broken record not treated as managed", svc)
		}
		var broken errCentralEnvBroken
		if _, err := s.effectiveEnv(svc, "h", nil); !errors.As(err, &broken) {
			t.Fatalf("%s: effectiveEnv = %v, want errCentralEnvBroken", svc, err)
		}
		if _, err := s.Apply(svc, centralEnvChange{IfVersion: 1}, ""); !errors.As(err, &broken) {
			t.Fatalf("%s: Apply = %v, want errCentralEnvBroken", svc, err)
		}
	}
	if env, err := s.effectiveEnv("good", "h", nil); err != nil || !reflect.DeepEqual(env, []string{"A=1"}) {
		t.Fatalf("good record = %v, %v", env, err)
	}
	// Delete is the operator's way out.
	if err := s.Delete("app"); err != nil || s.Has("app") {
		t.Fatalf("Delete broken = %v, still has=%v", err, s.Has("app"))
	}
}

// TestCentralEnvStoreConcurrentApply: 50 writers racing on the same service.
// Round one — all racing from version 1 — must produce exactly one winner;
// round two — each retrying until it lands — must hand out every version
// 2..51 exactly once, and the file on disk must equal memory afterwards.
func TestCentralEnvStoreConcurrentApply(t *testing.T) {
	s := newTestCentralStore(t)
	s.Create("app", "o", map[string]string{"N": "init"}, nil, "", "")
	const n = 50

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"N": fmt.Sprint(i)}}, "")
			var conflict errEnvVersionConflict
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.As(err, &conflict):
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("round one: %d winners, want exactly 1", wins)
	}

	seen := map[uint64]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				rec, _, _ := s.Get("app")
				res, err := s.Apply("app", centralEnvChange{IfVersion: rec.Version, Base: map[string]string{"N": fmt.Sprintf("r2-%d", i)}}, "")
				if err == nil {
					mu.Lock()
					seen[res.Version]++
					mu.Unlock()
					return
				}
				var conflict errEnvVersionConflict
				if !errors.As(err, &conflict) {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for v := uint64(3); v < 3+n; v++ {
		if seen[v] != 1 {
			t.Fatalf("version %d assigned %d times", v, seen[v])
		}
	}
	mem, _, _ := s.Get("app")
	reloaded, err := loadCentralEnvStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	disk, _, _ := reloaded.Get("app")
	if mem.Version != 2+n || !reflect.DeepEqual(mem, disk) {
		t.Fatalf("memory %+v != disk %+v", mem, disk)
	}
}

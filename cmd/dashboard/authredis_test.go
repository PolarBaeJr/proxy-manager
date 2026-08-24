package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// fakeTxRunner is an in-process stand-in for txRunner, matching redisrl_test.go's
// established fake-over-hand-rolled-interface style. gen bumps on every
// successful commit so RunTx can detect a lost race the same way a real
// Redis WATCH would.
type fakeTxRunner struct {
	mu    sync.Mutex
	data  []byte
	gen   int
	calls int

	// beforeCommit, if set, fires once (then clears itself) while RunTx
	// still holds the lock, right before it would otherwise commit —
	// letting a test simulate a peer's write landing between this
	// attempt's read and its commit. Must not call back into the fake's
	// own locked methods (Get/RunTx) — the lock is already held.
	beforeCommit func()

	// err, if set, makes both Get and RunTx fail immediately — simulates
	// Redis being unreachable.
	err error
}

func (f *fakeTxRunner) Get(ctx context.Context, key string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		return nil, nil
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out, nil
}

func (f *fakeTxRunner) RunTx(ctx context.Context, key string, fn func(current []byte) (newVal []byte, err error)) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.calls++
	cur := f.data
	genAtRead := f.gen
	f.mu.Unlock()

	newVal, err := fn(cur)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeCommit != nil {
		hook := f.beforeCommit
		f.beforeCommit = nil
		hook()
	}
	if f.gen != genAtRead {
		return errTxConflict
	}
	f.data = newVal
	f.gen++
	return nil
}

func (f *fakeTxRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newRedisBackedStore builds a fresh file-backed AuthStore (so save() still
// has somewhere to write) with txRunner wired to fake.
func newRedisBackedStore(t *testing.T, fake *fakeTxRunner) *AuthStore {
	t.Helper()
	s, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("loadAuthStore: %v", err)
	}
	s.txRunner = fake
	return s
}

func TestMutateUsersLocalPathUnchanged(t *testing.T) {
	s, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("loadAuthStore: %v", err)
	}
	newUser := User{Username: "alice"}
	if err := s.mutateUsers(func(users []User) ([]User, error) {
		return append(users, newUser), nil
	}); err != nil {
		t.Fatalf("mutateUsers: %v", err)
	}
	if len(s.data.Users) != 1 || s.data.Users[0].Username != "alice" {
		t.Fatalf("s.data.Users = %+v, want one user alice", s.data.Users)
	}
	// Confirm it actually persisted to disk (today's exact behavior).
	s2, err := loadAuthStore(s.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(s2.data.Users) != 1 || s2.data.Users[0].Username != "alice" {
		t.Fatalf("reloaded Users = %+v, want one user alice", s2.data.Users)
	}
}

func TestMutateUsersRedisAppend(t *testing.T) {
	fake := &fakeTxRunner{}
	s := newRedisBackedStore(t, fake)
	newUser := User{Username: "alice"}
	if err := s.mutateUsers(func(users []User) ([]User, error) {
		return append(users, newUser), nil
	}); err != nil {
		t.Fatalf("mutateUsers: %v", err)
	}
	if len(s.data.Users) != 1 || s.data.Users[0].Username != "alice" {
		t.Fatalf("s.data.Users = %+v, want one user alice", s.data.Users)
	}
	var stored []User
	if err := json.Unmarshal(fake.data, &stored); err != nil {
		t.Fatalf("unmarshal fake.data: %v", err)
	}
	if len(stored) != 1 || stored[0].Username != "alice" {
		t.Fatalf("fake.data = %+v, want one user alice", stored)
	}
}

func TestMutateUsersRedisConflictRetries(t *testing.T) {
	fake := &fakeTxRunner{}
	s := newRedisBackedStore(t, fake)
	// Simulate a peer's commit landing between this attempt's read and its
	// commit: bump gen and replace the stored data out from under us, once.
	fake.beforeCommit = func() {
		fake.gen++
		fake.data, _ = json.Marshal([]User{{Username: "peer"}})
	}
	newUser := User{Username: "alice"}
	if err := s.mutateUsers(func(users []User) ([]User, error) {
		return append(users, newUser), nil
	}); err != nil {
		t.Fatalf("mutateUsers: %v", err)
	}
	if fake.callCount() != 2 {
		t.Fatalf("RunTx calls = %d, want 2 (one conflict, one retry)", fake.callCount())
	}
	var stored []User
	if err := json.Unmarshal(fake.data, &stored); err != nil {
		t.Fatalf("unmarshal fake.data: %v", err)
	}
	// The retry must have read the peer's committed state (not the stale
	// pre-conflict read) and applied fn on top of it.
	if len(stored) != 2 || stored[0].Username != "peer" || stored[1].Username != "alice" {
		t.Fatalf("fake.data = %+v, want [peer alice]", stored)
	}
}

func TestMutateUsersBusinessErrorNotRetried(t *testing.T) {
	fake := &fakeTxRunner{}
	s := newRedisBackedStore(t, fake)
	wantErr := errors.New("boom: business logic rejected this mutation")
	err := s.mutateUsers(func(users []User) ([]User, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("mutateUsers error = %v, want %v", err, wantErr)
	}
	if fake.callCount() != 1 {
		t.Fatalf("RunTx calls = %d, want 1 (business error must not be retried)", fake.callCount())
	}
	if fake.data != nil {
		t.Fatalf("fake.data = %q, want untouched (no write should have happened)", fake.data)
	}
}

func TestSyncFromRedisImportsOnFirstRun(t *testing.T) {
	fake := &fakeTxRunner{} // key absent
	s := newRedisBackedStore(t, fake)
	s.data.Users = []User{{Username: "local-user"}}
	if err := s.syncFromRedisOrImport(context.Background()); err != nil {
		t.Fatalf("syncFromRedisOrImport: %v", err)
	}
	var stored []User
	if err := json.Unmarshal(fake.data, &stored); err != nil {
		t.Fatalf("unmarshal fake.data: %v", err)
	}
	if len(stored) != 1 || stored[0].Username != "local-user" {
		t.Fatalf("redis was not seeded from local Users, got %+v", stored)
	}
	if len(s.data.Users) != 1 || s.data.Users[0].Username != "local-user" {
		t.Fatalf("s.data.Users changed unexpectedly: %+v", s.data.Users)
	}
}

func TestSyncFromRedisAdoptsExistingOverridesLocal(t *testing.T) {
	fake := &fakeTxRunner{}
	fake.data, _ = json.Marshal([]User{{Username: "redis-user"}})
	s := newRedisBackedStore(t, fake)
	s.data.Users = []User{{Username: "stale-local-user"}}
	if err := s.syncFromRedisOrImport(context.Background()); err != nil {
		t.Fatalf("syncFromRedisOrImport: %v", err)
	}
	if len(s.data.Users) != 1 || s.data.Users[0].Username != "redis-user" {
		t.Fatalf("s.data.Users = %+v, want [redis-user] (adopted over local)", s.data.Users)
	}
}

func TestSyncFromRedisUnreachableKeepsLocalUsers(t *testing.T) {
	fake := &fakeTxRunner{err: errors.New("connection refused")}
	s := newRedisBackedStore(t, fake)
	s.data.Users = []User{{Username: "local-user"}}
	if err := s.syncFromRedisOrImport(context.Background()); err == nil {
		t.Fatal("syncFromRedisOrImport: expected a connectivity error, got nil")
	}
	if len(s.data.Users) != 1 || s.data.Users[0].Username != "local-user" {
		t.Fatalf("s.data.Users = %+v, want unchanged [local-user]", s.data.Users)
	}
}

// TestTwoInstancesSharedFakeBackend exercises the concurrency story this
// whole feature exists for: two independent *AuthStore instances (standing
// in for the Pi and Mac mini) sharing one Redis backend, mutating at the
// same time. No update should be lost, and after each side re-syncs from
// Redis both must agree on the final state.
func TestTwoInstancesSharedFakeBackend(t *testing.T) {
	fake := &fakeTxRunner{}
	ctx := context.Background()

	s1 := newRedisBackedStore(t, fake)
	secretA, _, err := s1.BeginSetup("alice", "password123")
	if err != nil {
		t.Fatalf("BeginSetup: %v", err)
	}
	if err := s1.ConfirmPending("alice", codeNow(secretA)); err != nil {
		t.Fatalf("ConfirmPending alice: %v", err)
	}
	secretB, _, err := s1.BeginCreateUser("bob", "password123")
	if err != nil {
		t.Fatalf("BeginCreateUser: %v", err)
	}
	if err := s1.ConfirmPending("bob", codeNow(secretB)); err != nil {
		t.Fatalf("ConfirmPending bob: %v", err)
	}

	s2 := newRedisBackedStore(t, fake)
	if err := s2.syncFromRedisOrImport(ctx); err != nil {
		t.Fatalf("s2 syncFromRedisOrImport: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var createErr, deleteErr error
	go func() {
		defer wg.Done()
		_, _, createErr = s1.CreateToken("alice", "tok1")
	}()
	go func() {
		defer wg.Done()
		deleteErr = s2.DeleteUser("bob")
	}()
	wg.Wait()

	if createErr != nil {
		t.Fatalf("CreateToken: %v", createErr)
	}
	if deleteErr != nil {
		t.Fatalf("DeleteUser: %v", deleteErr)
	}

	// Manual refresh: both sides re-adopt the converged Redis state.
	if err := s1.syncFromRedisOrImport(ctx); err != nil {
		t.Fatalf("s1 re-sync: %v", err)
	}
	if err := s2.syncFromRedisOrImport(ctx); err != nil {
		t.Fatalf("s2 re-sync: %v", err)
	}

	for name, s := range map[string]*AuthStore{"s1": s1, "s2": s2} {
		if len(s.data.Users) != 1 || s.data.Users[0].Username != "alice" {
			t.Fatalf("%s.data.Users = %+v, want just [alice] (bob deleted)", name, s.data.Users)
		}
		if len(s.data.Users[0].Tokens) != 1 {
			t.Fatalf("%s: alice's tokens = %+v, want 1 (created concurrently, must not be lost)", name, s.data.Users[0].Tokens)
		}
	}
}

// TestMutateUsersConcurrentWithinOneStore drives concurrent mutateUsers
// writers on a single *AuthStore against a concurrent reader doing the same
// read-modify-write-under-lock step refreshLoop uses. Under -race this
// catches an unlocked s.save() racing s.data — the class of bug the
// original Redis-path snippet had (save() called after s.mu.Unlock()).
func TestMutateUsersConcurrentWithinOneStore(t *testing.T) {
	fake := &fakeTxRunner{}
	s := newRedisBackedStore(t, fake)
	secret, _, err := s.BeginSetup("alice", "password123")
	if err != nil {
		t.Fatalf("BeginSetup: %v", err)
	}
	if err := s.ConfirmPending("alice", codeNow(secret)); err != nil {
		t.Fatalf("ConfirmPending: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if _, _, err := s.CreateToken("alice", fmt.Sprintf("tok%d", i)); err != nil {
				t.Errorf("CreateToken %d: %v", i, err)
			}
		}(i)
	}

	stop := make(chan struct{})
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := s.txRunner.Get(context.Background(), authRedisKey)
			if err != nil || raw == nil {
				continue
			}
			var users []User
			if json.Unmarshal(raw, &users) != nil {
				continue
			}
			s.mu.Lock()
			s.data.Users = users
			_ = s.save()
			s.mu.Unlock()
		}
	}()

	wg.Wait()
	close(stop)
	<-refreshDone

	if err := s.syncFromRedisOrImport(context.Background()); err != nil {
		t.Fatalf("final sync: %v", err)
	}
	if len(s.data.Users) != 1 || len(s.data.Users[0].Tokens) != n {
		t.Fatalf("alice's tokens = %+v, want %d (none lost to the concurrent refresh reader)", s.data.Users[0].Tokens, n)
	}
}

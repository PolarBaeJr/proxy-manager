package main

import (
	"errors"
	"reflect"
	"testing"
)

// The auto-updater replaces services with no env edits at all. Any
// normalization here would silently rewrite the environment of every
// auto-updating service on each release, so this must be an exact passthrough.
func TestMergeEnvNoEditsIsExactPassthrough(t *testing.T) {
	base := []string{"B=2", "A=1", "A=override", "NOEQUALS", "EMPTY="}
	for _, edits := range []map[string]string{nil, {}} {
		got, err := mergeEnv(base, edits, nil)
		if err != nil {
			t.Fatalf("mergeEnv(%v): %v", edits, err)
		}
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("env = %v, want %v (unchanged, unreordered, not deduped)", got, base)
		}
		if len(got) > 0 && &got[0] != &base[0] {
			t.Errorf("base was copied; want the same backing slice so nothing can diverge")
		}
	}
}

func TestMergeEnvAppendsNewKeys(t *testing.T) {
	base := []string{"KEEP=yes", "ALSO=kept"}
	got, err := mergeEnv(base, map[string]string{"NEW": "1", "ANOTHER": "2"}, nil)
	if err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	// Existing entries keep their order; additions are sorted for determinism.
	want := []string{"KEEP=yes", "ALSO=kept", "ANOTHER=2", "NEW=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	if reflect.DeepEqual(base, want) {
		t.Error("base slice was mutated in place")
	}
}

// Re-setting a key to the value it already has is not a question.
func TestMergeEnvSameValueIsNoOp(t *testing.T) {
	base := []string{"PORT=8080", "X=1"}
	got, err := mergeEnv(base, map[string]string{"PORT": "8080"}, nil)
	if err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("env = %v, want %v", got, base)
	}
}

func TestMergeEnvConflictsUntilAcked(t *testing.T) {
	base := []string{"PORT=8080", "DEBUG=false"}
	edits := map[string]string{"PORT": "3000"}

	_, err := mergeEnv(base, edits, nil)
	var ce *envConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *envConflictError", err)
	}
	if len(ce.Conflicts) != 1 || ce.Conflicts[0].Key != "PORT" ||
		ce.Conflicts[0].Current != "8080" || ce.Conflicts[0].Incoming != "3000" {
		t.Fatalf("conflicts = %+v", ce.Conflicts)
	}

	got, err := mergeEnv(base, edits, []string{"PORT"})
	if err != nil {
		t.Fatalf("acked merge: %v", err)
	}
	// Overwrites in place rather than appending a duplicate.
	want := []string{"PORT=3000", "DEBUG=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

// The dialog needs every conflict at once, or the user gets one round-trip per
// variable.
func TestMergeEnvReportsAllConflictsSorted(t *testing.T) {
	base := []string{"A=1", "B=2", "C=3"}
	_, err := mergeEnv(base, map[string]string{"C": "z", "A": "x", "B": "y"}, nil)
	var ce *envConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *envConflictError", err)
	}
	if len(ce.Conflicts) != 3 {
		t.Fatalf("got %d conflicts, want 3: %+v", len(ce.Conflicts), ce.Conflicts)
	}
	for i, k := range []string{"A", "B", "C"} {
		if ce.Conflicts[i].Key != k {
			t.Errorf("conflict[%d] = %q, want %q (sorted)", i, ce.Conflicts[i].Key, k)
		}
	}
}

// Acking one key must not suppress the question for another.
func TestMergeEnvPartialAckStillConflicts(t *testing.T) {
	base := []string{"A=1", "B=2"}
	_, err := mergeEnv(base, map[string]string{"A": "x", "B": "y"}, []string{"A"})
	var ce *envConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *envConflictError", err)
	}
	if len(ce.Conflicts) != 1 || ce.Conflicts[0].Key != "B" {
		t.Fatalf("conflicts = %+v, want only B", ce.Conflicts)
	}
}

// Docker keeps the LAST occurrence of a repeated key, so that is the value the
// container is running — and the one to compare and overwrite.
func TestMergeEnvUsesLastOccurrenceOfRepeatedKey(t *testing.T) {
	base := []string{"K=first", "OTHER=x", "K=last"}
	_, err := mergeEnv(base, map[string]string{"K": "new"}, nil)
	var ce *envConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *envConflictError", err)
	}
	if ce.Conflicts[0].Current != "last" {
		t.Fatalf("current = %q, want %q", ce.Conflicts[0].Current, "last")
	}
	got, err := mergeEnv(base, map[string]string{"K": "new"}, []string{"K"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"K=first", "OTHER=x", "K=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

// An entry with no "=" asks Docker to inherit from the daemon; it is not a
// KEY=VALUE assignment and must survive untouched.
func TestMergeEnvLeavesBareEntriesAlone(t *testing.T) {
	base := []string{"INHERIT", "=weird", "A=1"}
	got, err := mergeEnv(base, map[string]string{"B": "2"}, nil)
	if err != nil {
		t.Fatalf("mergeEnv: %v", err)
	}
	want := []string{"INHERIT", "=weird", "A=1", "B=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	// A bare name must not be addressable as a key.
	if _, err := mergeEnv(base, map[string]string{"INHERIT": "x"}, nil); err != nil {
		t.Fatalf("INHERIT should append, not conflict: %v", err)
	}
}

func TestMergeEnvEmptyValues(t *testing.T) {
	base := []string{"A=1"}
	// Setting an existing key to empty is a real change, so it must be asked
	// about rather than silently applied or ignored.
	_, err := mergeEnv(base, map[string]string{"A": ""}, nil)
	var ce *envConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want conflict for A=''", err)
	}
	if ce.Conflicts[0].Incoming != "" {
		t.Errorf("incoming = %q, want empty", ce.Conflicts[0].Incoming)
	}
	got, err := mergeEnv(base, map[string]string{"A": ""}, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"A="}) {
		t.Fatalf("env = %v, want [A=]", got)
	}
}

func TestSplitEnvEntry(t *testing.T) {
	cases := []struct {
		in       string
		k, v     string
		splitsOK bool
	}{
		{"A=1", "A", "1", true},
		{"A=", "A", "", true},
		{"A=b=c", "A", "b=c", true}, // only the first "=" separates
		{"NOEQ", "", "", false},
		{"=novalue", "", "", false}, // empty key is not addressable
		{"", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := splitEnvEntry(c.in)
		if ok != c.splitsOK || k != c.k || v != c.v {
			t.Errorf("splitEnvEntry(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, k, v, ok, c.k, c.v, c.splitsOK)
		}
	}
}

func TestEnvConflictErrorMessage(t *testing.T) {
	e := &envConflictError{Conflicts: []EnvConflict{{Key: "A"}, {Key: "B"}}}
	if got := e.Error(); got != "env conflict on A, B: choose which value to keep" {
		t.Fatalf("Error() = %q", got)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicWritesContentAndPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("content = %q, %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", st.Mode().Perm())
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFileAtomicCleansUpTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	// Renaming a file over a non-empty directory fails, which exercises the
	// post-write failure path (temp file already created and filled).
	target := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("data"), 0o600); err == nil {
		t.Fatal("expected rename over a non-empty directory to fail")
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFileAtomicMissingDirFails(t *testing.T) {
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "nope", "x.json"), []byte("d"), 0o600); err == nil {
		t.Fatal("expected an error for a missing parent directory")
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

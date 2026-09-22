package main

import (
	"os"
	"path/filepath"
)

// writeFileAtomic replaces path with data so a reader (or a crash) only ever
// sees the old file or the complete new one, never a torn write: the bytes go
// to a temp file in the SAME directory (so the rename is a same-filesystem
// atomic replace), are fsynced, and only then renamed over path. The
// directory itself is fsynced afterwards so the rename survives power loss.
// Any failure before the rename removes the temp file; after the rename there
// is nothing left to clean up.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	renamed = true
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

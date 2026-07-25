package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Property test backing the table-driven cases in maintenance_test.go: for ANY
// input, a successful (*maintStore).path must land strictly inside the store's
// directory. The table covers the traversal payloads we thought of; this covers
// the ones we didn't, by combining adversarial atoms pairwise.
//
// The host reaches path() from a URL segment and becomes a FILENAME, so an
// escape here is a write-anywhere bug. Kept separate from the table so a
// failure names the exact input that got through.
func TestMaintPathNeverEscapesDir(t *testing.T) {
	dir := t.TempDir()
	m := &maintStore{dir: dir}
	clean := filepath.Clean(dir)

	atoms := []string{
		"..", ".", "/", "\\", "%2e", "%2f", "\x00", "a", "..;", "..%00",
		"....//", "..\\..", "a/../../b", "~", "$HOME", "*", "\n", "\r",
		"..\ufeff", "\uff41", "\uff0e\uff0e", "a..b",
	}
	corpus := append([]string{}, atoms...)
	for _, a := range atoms {
		for _, b := range atoms {
			corpus = append(corpus, a+b, a+"/"+b, a+"."+b, strings.ToUpper(a)+b)
		}
	}
	corpus = append(corpus,
		strings.Repeat("../", 40)+"etc/passwd",
		strings.Repeat("a", 300),
	)

	for _, in := range corpus {
		p, err := m.path(in)
		if err != nil {
			if p != "" {
				t.Errorf("path(%q): error returned with non-empty path %q", in, p)
			}
			continue
		}
		if filepath.Dir(p) != clean {
			t.Errorf("escape: path(%q) = %q — parent %q, want %q", in, p, filepath.Dir(p), clean)
		}
		if !strings.HasPrefix(filepath.Clean(p), clean+string(filepath.Separator)) {
			t.Errorf("escape: path(%q) = %q is not under %q", in, p, clean)
		}
	}
}

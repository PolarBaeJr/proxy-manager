// Per-user UI preferences — the dashboard mirrors its pmgr-* localStorage
// keys (collapsed cards, filter chips, sort order) to /data/prefs.json so
// cosmetic state follows the user across browsers. Values are opaque raw
// strings; the client does its own JSON.parse/stringify.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	prefsKeyPrefix  = "pmgr-"
	prefsMaxKeyLen  = 64
	prefsMaxValLen  = 4096
	prefsMaxPerUser = 128
)

type PrefsStore struct {
	path string
	mu   sync.RWMutex
	data map[string]map[string]string // username (lowercased) → key → raw value
}

func loadPrefsStore(path string) (*PrefsStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &PrefsStore{path: path, data: map[string]map[string]string{}}
	b, err := os.ReadFile(path)
	if err == nil {
		// Corrupt file → start fresh; prefs are cosmetic, not worth a fatal.
		_ = json.Unmarshal(b, &s.data)
		if s.data == nil {
			s.data = map[string]map[string]string{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *PrefsStore) save() error {
	b, _ := json.MarshalIndent(s.data, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

// Get returns a copy of the user's prefs — never nil.
func (s *PrefsStore) Get(username string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for k, v := range s.data[strings.ToLower(username)] {
		out[k] = v
	}
	return out
}

// Merge validates and upserts the given keys into the user's prefs.
func (s *PrefsStore) Merge(username string, kv map[string]string) error {
	for k, v := range kv {
		if !strings.HasPrefix(k, prefsKeyPrefix) {
			return fmt.Errorf("pref keys must start with %q", prefsKeyPrefix)
		}
		if len(k) > prefsMaxKeyLen {
			return fmt.Errorf("pref key too long (max %d)", prefsMaxKeyLen)
		}
		if len(v) > prefsMaxValLen {
			return fmt.Errorf("pref value too long (max %d)", prefsMaxValLen)
		}
	}
	u := strings.ToLower(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.data[u]
	if cur == nil {
		cur = map[string]string{}
		s.data[u] = cur
	}
	for k, v := range kv {
		if _, exists := cur[k]; !exists && len(cur) >= prefsMaxPerUser {
			return fmt.Errorf("too many prefs (max %d per user)", prefsMaxPerUser)
		}
		cur[k] = v
	}
	return s.save()
}

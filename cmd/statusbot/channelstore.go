package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultChannelStoreFile = "/data/alert-channel.json"

type channelStoreData struct {
	ChannelID string `json:"channel_id"`
}

// channelStore persists the alert channel chosen at runtime via
// /set-alert-channel so it survives a container restart. A nil *channelStore
// (the /data volume isn't mounted) degrades: Get always returns "", and Set
// returns a descriptive error instead of pretending the value persisted.
type channelStore struct {
	path string
	mu   sync.RWMutex
	data channelStoreData
}

// newChannelStoreFromEnv resolves CHANNEL_STORE_FILE (default
// /data/alert-channel.json). Returns nil, with a message to log, if the
// file's parent directory doesn't exist. Deliberately does NOT MkdirAll —
// mirrors cmd/dashboard/maintenance.go's newMaintFromEnv: on a host without
// the volume mount that would create the directory inside the container's
// ephemeral layer, and every persisted "here" would silently vanish on the
// next restart while claiming to have worked.
func newChannelStoreFromEnv(getenv func(string) string) (*channelStore, []string) {
	path := strings.TrimSpace(getenv("CHANNEL_STORE_FILE"))
	if path == "" {
		path = defaultChannelStoreFile
	}
	dir := filepath.Dir(path)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, []string{"⚠ alert-channel persistence disabled: " + dir + " not available: " + err.Error()}
	}
	if !st.IsDir() {
		return nil, []string{"⚠ alert-channel persistence disabled: " + dir + " is not a directory"}
	}
	s := &channelStore{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
	} else if !os.IsNotExist(err) {
		return nil, []string{"⚠ alert-channel persistence disabled: read " + path + ": " + err.Error()}
	}
	return s, nil
}

// Get returns the persisted channel ID, or "" if unset or unavailable.
func (s *channelStore) Get() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.ChannelID
}

// Set persists channelID as the new alert channel.
func (s *channelStore) Set(channelID string) error {
	if s == nil {
		return fmt.Errorf("alert-channel persistence not configured (no /data volume mounted for statusbot)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.ChannelID = channelID
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

// initialAlertChannel resolves the alert channel to use at boot: the
// persisted value always wins once one exists. DISCORD_CHANNEL_ID only seeds
// the store on a genuinely empty store (first run, or persistence
// unavailable) — never overrides an existing persisted value, otherwise a
// stale env var would silently revert whatever /set-alert-channel moved
// alerts to on every restart. Nil-safe: s may be nil (no volume mounted).
func initialAlertChannel(s *channelStore, getenv func(string) string) (string, []string) {
	var msgs []string
	persisted := s.Get()
	envVal := strings.TrimSpace(getenv("DISCORD_CHANNEL_ID"))

	if persisted != "" {
		if envVal != "" && envVal != persisted {
			msgs = append(msgs, fmt.Sprintf(
				"alert channel: using persisted value %s (DISCORD_CHANNEL_ID=%s ignored — "+
					"use /set-alert-channel to change it)", persisted, envVal))
		}
		return persisted, msgs
	}
	if envVal == "" {
		msgs = append(msgs, "no alert channel configured yet — proactive alerts are suppressed until "+
			"/set-alert-channel is run in the target channel")
		return "", msgs
	}
	if err := s.Set(envVal); err != nil {
		msgs = append(msgs, "seeded alert channel from DISCORD_CHANNEL_ID but could not persist it: "+err.Error())
	}
	return envVal, msgs
}

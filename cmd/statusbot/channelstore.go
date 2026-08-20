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
	// MessageID belongs to whatever channel ChannelID currently names — the
	// live-status message being edited in place there. It is cleared
	// whenever the channel changes (see Set).
	MessageID string `json:"message_id,omitempty"`
	// StatusMessageID is the separate, independently-edited per-service
	// status embed message (see servicestatus.go/statusembed.go) — kept
	// apart from MessageID so the ~15s status tick and the up/down
	// transition alerts don't fight over the same message. Cleared on a
	// channel change alongside MessageID, for the same reason.
	StatusMessageID string `json:"status_message_id,omitempty"`
}

// channelStore persists the alert channel chosen at runtime via
// /set-alert-channel, and the live-status/service-status message IDs being
// edited in place within it, so both survive a container restart. A nil
// *channelStore (the /data volume isn't mounted) degrades:
// Get/GetMessageID/GetStatusMessageID always return "", and
// Set/SetMessageID/SetStatusMessageID return a descriptive error instead of
// pretending the value persisted.
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

// Set persists channelID as the new alert channel. Always unconditionally
// clears the persisted MessageID — it belonged to the previous channel, if
// any, and is meaningless for the new one. The caller re-seeds MessageID via
// a second SetMessageID call right after, in main.go, once it has posted the
// new channel's live-status message.
func (s *channelStore) Set(channelID string) error {
	if s == nil {
		return fmt.Errorf("alert-channel persistence not configured (no /data volume mounted for statusbot)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.ChannelID = channelID
	s.data.MessageID = ""
	s.data.StatusMessageID = ""
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

// GetMessageID returns the persisted live-status message ID for the
// current alert channel, or "" if none is known yet (fresh deploy, just
// after Set changed the channel, or a pre-existing store file from
// before this field existed).
func (s *channelStore) GetMessageID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.MessageID
}

// SetMessageID persists the live-status message ID for the
// already-established alert channel, without touching ChannelID. Does
// not validate that a channel is actually set — a MessageID recorded
// without a ChannelID is harmless dead data, never consulted on its own.
func (s *channelStore) SetMessageID(messageID string) error {
	if s == nil {
		return fmt.Errorf("alert-channel persistence not configured (no /data volume mounted for statusbot)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.MessageID = messageID
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

// GetStatusMessageID returns the persisted service-status embed message ID
// for the current alert channel, or "" if none is known yet — mirrors
// GetMessageID but for the independent per-service status tick.
func (s *channelStore) GetStatusMessageID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.StatusMessageID
}

// SetStatusMessageID persists the service-status embed message ID for the
// already-established alert channel, without touching ChannelID or
// MessageID — mirrors SetMessageID but for the independent per-service
// status tick.
func (s *channelStore) SetStatusMessageID(messageID string) error {
	if s == nil {
		return fmt.Errorf("alert-channel persistence not configured (no /data volume mounted for statusbot)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.StatusMessageID = messageID
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

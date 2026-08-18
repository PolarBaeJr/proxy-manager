package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChannelStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	getenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, msgs := newChannelStoreFromEnv(getenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, msgs=%v; want a store", msgs)
	}
	if err := s.Set("123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	fresh, msgs := newChannelStoreFromEnv(getenv)
	if fresh == nil {
		t.Fatalf("newChannelStoreFromEnv (reload) = nil, msgs=%v; want a store", msgs)
	}
	if got := fresh.Get(); got != "123" {
		t.Fatalf("Get after reload = %q, want %q", got, "123")
	}
}

func TestChannelStoreUnconfigured(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "alert-channel.json")
	getenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return missing
		}
		return ""
	}
	s, msgs := newChannelStoreFromEnv(getenv)
	if s != nil {
		t.Fatalf("newChannelStoreFromEnv = %v, want nil for a missing parent dir", s)
	}
	if len(msgs) == 0 {
		t.Errorf("newChannelStoreFromEnv returned no log message for a missing parent dir")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("newChannelStoreFromEnv created %q (stat err = %v), want it left alone", filepath.Dir(missing), err)
	}
	if got := s.Get(); got != "" {
		t.Errorf("nil store Get = %q, want empty", got)
	}
	err := s.Set("x")
	if err == nil {
		t.Fatalf("nil store Set = nil error, want error")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("nil store Set error = %q, want it to name the missing mount", err.Error())
	}
}

func TestChannelStoreEmptyFileIsEmptyValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	getenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, msgs := newChannelStoreFromEnv(getenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, msgs=%v; want a store", msgs)
	}
	if got := s.Get(); got != "" {
		t.Errorf("Get on fresh store = %q, want empty", got)
	}
}

func TestInitialAlertChannelSeedsEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	storeGetenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, _ := newChannelStoreFromEnv(storeGetenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, want a store")
	}

	envGetenv := func(k string) string {
		if k == "DISCORD_CHANNEL_ID" {
			return "123"
		}
		return ""
	}
	got, msgs := initialAlertChannel(s, envGetenv)
	if got != "123" {
		t.Fatalf("initialAlertChannel = %q, want %q", got, "123")
	}
	_ = msgs

	fresh, _ := newChannelStoreFromEnv(storeGetenv)
	if fresh == nil {
		t.Fatalf("newChannelStoreFromEnv (reload) = nil, want a store")
	}
	if got := fresh.Get(); got != "123" {
		t.Fatalf("Get after reload = %q, want %q (seed must be persisted)", got, "123")
	}
}

func TestInitialAlertChannelPersistedWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	storeGetenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, _ := newChannelStoreFromEnv(storeGetenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, want a store")
	}
	if err := s.Set("111"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	envGetenv := func(k string) string {
		if k == "DISCORD_CHANNEL_ID" {
			return "222"
		}
		return ""
	}
	got, msgs := initialAlertChannel(s, envGetenv)
	if got != "111" {
		t.Fatalf("initialAlertChannel = %q, want persisted value %q", got, "111")
	}
	if got := s.Get(); got != "111" {
		t.Fatalf("store Get after initialAlertChannel = %q, want unchanged %q", got, "111")
	}
	if len(msgs) == 0 {
		t.Errorf("initialAlertChannel returned no msgs for a persisted/env divergence")
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "111") && strings.Contains(m, "222") {
			found = true
		}
	}
	if !found {
		t.Errorf("initialAlertChannel msgs = %v, want one mentioning both 111 and 222", msgs)
	}
}

func TestInitialAlertChannelBothEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	storeGetenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, _ := newChannelStoreFromEnv(storeGetenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, want a store")
	}

	got, msgs := initialAlertChannel(s, func(string) string { return "" })
	if got != "" {
		t.Fatalf("initialAlertChannel = %q, want empty", got)
	}
	if len(msgs) == 0 {
		t.Errorf("initialAlertChannel returned no msgs when both persisted and env are empty")
	}
}

func TestChannelStoreMessageIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	getenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, _ := newChannelStoreFromEnv(getenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, want a store")
	}
	if err := s.Set("123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.SetMessageID("m1"); err != nil {
		t.Fatalf("SetMessageID: %v", err)
	}

	fresh, _ := newChannelStoreFromEnv(getenv)
	if fresh == nil {
		t.Fatalf("newChannelStoreFromEnv (reload) = nil, want a store")
	}
	if got := fresh.Get(); got != "123" {
		t.Fatalf("Get after reload = %q, want %q", got, "123")
	}
	if got := fresh.GetMessageID(); got != "m1" {
		t.Fatalf("GetMessageID after reload = %q, want %q", got, "m1")
	}
}

func TestChannelStoreSetClearsMessageID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	getenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, _ := newChannelStoreFromEnv(getenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, want a store")
	}
	if err := s.Set("123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.SetMessageID("m1"); err != nil {
		t.Fatalf("SetMessageID: %v", err)
	}
	if err := s.Set("456"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.GetMessageID(); got != "" {
		t.Fatalf("GetMessageID after channel change = %q, want empty", got)
	}

	fresh, _ := newChannelStoreFromEnv(getenv)
	if fresh == nil {
		t.Fatalf("newChannelStoreFromEnv (reload) = nil, want a store")
	}
	if got := fresh.GetMessageID(); got != "" {
		t.Fatalf("GetMessageID after reload = %q, want empty", got)
	}
}

func TestChannelStoreOldFormatFileMessageIDEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alert-channel.json")
	if err := os.WriteFile(path, []byte(`{"channel_id":"123"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	getenv := func(k string) string {
		if k == "CHANNEL_STORE_FILE" {
			return path
		}
		return ""
	}
	s, msgs := newChannelStoreFromEnv(getenv)
	if s == nil {
		t.Fatalf("newChannelStoreFromEnv = nil, msgs=%v; want a store", msgs)
	}
	if got := s.Get(); got != "123" {
		t.Fatalf("Get = %q, want %q", got, "123")
	}
	if got := s.GetMessageID(); got != "" {
		t.Fatalf("GetMessageID = %q, want empty", got)
	}
}

func TestChannelStoreNilStoreMessageID(t *testing.T) {
	var s *channelStore
	if got := s.GetMessageID(); got != "" {
		t.Errorf("nil store GetMessageID = %q, want empty", got)
	}
	err := s.SetMessageID("x")
	if err == nil {
		t.Fatalf("nil store SetMessageID = nil error, want error")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("nil store SetMessageID error = %q, want it to name the missing mount", err.Error())
	}
}

func TestInitialAlertChannelNilStore(t *testing.T) {
	envGetenv := func(k string) string {
		if k == "DISCORD_CHANNEL_ID" {
			return "123"
		}
		return ""
	}
	got, msgs := initialAlertChannel(nil, envGetenv)
	if got != "123" {
		t.Fatalf("initialAlertChannel(nil, ...) = %q, want %q", got, "123")
	}
	if len(msgs) == 0 {
		t.Errorf("initialAlertChannel(nil, ...) returned no msgs, want a persistence-failed message")
	}
}

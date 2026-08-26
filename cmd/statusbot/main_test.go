package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestIsUnknownMessageErr(t *testing.T) {
	unknownMessage := &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage, Message: "Unknown Message"},
	}
	if !isUnknownMessageErr(unknownMessage) {
		t.Error("a genuine 404 Unknown Message must report the message as gone")
	}

	otherRESTErr := &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Message:  &discordgo.APIErrorMessage{Code: 50001, Message: "Missing Access"},
	}
	if isUnknownMessageErr(otherRESTErr) {
		t.Error("a different Discord API error code must not be treated as the message being gone")
	}

	restErrNoMessage := &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusInternalServerError}}
	if isUnknownMessageErr(restErrNoMessage) {
		t.Error("a RESTError with no decoded Message body must not be treated as the message being gone")
	}

	// The failure mode this guards against: a plain network error (e.g. the
	// "connection reset by peer" that triggered the real spam loop) must
	// never be mistaken for proof the message was deleted.
	if isUnknownMessageErr(errors.New("read tcp: connection reset by peer")) {
		t.Error("a transient network error must not be treated as the message being gone")
	}

	if isUnknownMessageErr(nil) {
		t.Error("nil error must not be treated as the message being gone")
	}
}

func TestResolveDashboardToken(t *testing.T) {
	t.Run("env overrides file", func(t *testing.T) {
		t.Setenv("DASHBOARD_API_TOKEN", "pmt_env-token")
		path := filepath.Join(t.TempDir(), "statusbot.token")
		if err := os.WriteFile(path, []byte("pmt_file-token"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got := resolveDashboardToken(path); got != "pmt_env-token" {
			t.Fatalf("resolveDashboardToken = %q, want %q", got, "pmt_env-token")
		}
	})

	t.Run("falls back to file, trimmed", func(t *testing.T) {
		t.Setenv("DASHBOARD_API_TOKEN", "")
		path := filepath.Join(t.TempDir(), "statusbot.token")
		if err := os.WriteFile(path, []byte("pmt_file-token\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got := resolveDashboardToken(path); got != "pmt_file-token" {
			t.Fatalf("resolveDashboardToken = %q, want %q", got, "pmt_file-token")
		}
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		t.Setenv("DASHBOARD_API_TOKEN", "")
		path := filepath.Join(t.TempDir(), "nope.token")
		if got := resolveDashboardToken(path); got != "" {
			t.Fatalf("resolveDashboardToken = %q, want empty", got)
		}
	})
}

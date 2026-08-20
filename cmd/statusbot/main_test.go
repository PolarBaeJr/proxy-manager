package main

import (
	"os"
	"path/filepath"
	"testing"
)

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

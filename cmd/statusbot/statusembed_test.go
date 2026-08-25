package main

import (
	"strings"
	"testing"
)

func intPtr(n int) *int { return &n }

func TestStateIcon(t *testing.T) {
	cases := map[string]string{"up": "✅", "degraded": "⚠️", "down": "🔴", "unknown-state": "❔"}
	for state, icon := range cases {
		if got := stateIcon(state); got != icon {
			t.Errorf("stateIcon(%q) = %q, want %q", state, got, icon)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1536, "1.5KiB"},
		{1073741824, "1.0GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestStatusUnavailableEmbed(t *testing.T) {
	embed := statusUnavailableEmbed(errNoDashboardToken)
	if embed.Color != colorUnreachable {
		t.Errorf("Color = %#x, want %#x", embed.Color, colorUnreachable)
	}
	if !strings.Contains(embed.Description, "DASHBOARD_API_TOKEN") {
		t.Errorf("Description = %q, want it to mention the underlying error", embed.Description)
	}
}

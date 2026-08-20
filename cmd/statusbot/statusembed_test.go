package main

import (
	"fmt"
	"strings"
	"testing"
)

func intPtr(n int) *int { return &n }

func TestBuildServiceStatusEmbedGrouping(t *testing.T) {
	resp := serviceStatusResp{
		Groups: []serviceStatusGroup{
			{Group: "badminton", Services: []serviceStatusEntry{
				{Name: "player", Routed: true, HealthyReplicas: 2, TotalReplicas: 2, State: "up", Requests5m: intPtr(134)},
				{Name: "badminton-db", Routed: false, Requests5m: nil, State: "up"},
			}},
			{Group: "market-tracker", Services: []serviceStatusEntry{
				{Name: "web", Routed: true, HealthyReplicas: 1, TotalReplicas: 2, State: "degraded", Requests5m: intPtr(9)},
			}},
		},
	}
	embed := buildServiceStatusEmbed(resp)
	if len(embed.Fields) != 2 {
		t.Fatalf("Fields = %d, want one field per group (2)", len(embed.Fields))
	}
	if embed.Fields[0].Name != "badminton" {
		t.Errorf("Fields[0].Name = %q, want %q", embed.Fields[0].Name, "badminton")
	}
	if !strings.Contains(embed.Fields[0].Value, "player") || !strings.Contains(embed.Fields[0].Value, "badminton-db") {
		t.Errorf("Fields[0].Value = %q, want both services listed as lines", embed.Fields[0].Value)
	}
	if strings.Count(embed.Fields[0].Value, "\n") != 1 {
		t.Errorf("Fields[0].Value = %q, want exactly 2 lines (one per service)", embed.Fields[0].Value)
	}
	if !strings.Contains(embed.Fields[1].Value, "web") {
		t.Errorf("Fields[1].Value = %q, want it to mention web", embed.Fields[1].Value)
	}
}

func TestBuildServiceStatusEmbedDashForNullOrUnroutedRate(t *testing.T) {
	resp := serviceStatusResp{
		Groups: []serviceStatusGroup{
			{Group: "g", Services: []serviceStatusEntry{
				{Name: "unrouted", Routed: false, State: "up", Requests5m: intPtr(0)}, // even if the backend somehow sent a number, Routed:false wins
				{Name: "null-rate", Routed: true, State: "up", Requests5m: nil},
				{Name: "routed", Routed: true, State: "up", Requests5m: intPtr(0)},
			}},
		},
	}
	embed := buildServiceStatusEmbed(resp)
	lines := strings.Split(embed.Fields[0].Value, "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want 3", lines)
	}
	if !strings.Contains(lines[0], "— req/5m") {
		t.Errorf("unrouted line = %q, want a dash, not 0", lines[0])
	}
	if !strings.Contains(lines[1], "— req/5m") {
		t.Errorf("null-rate line = %q, want a dash, not 0", lines[1])
	}
	if !strings.Contains(lines[2], "0 req/5m") {
		t.Errorf("routed/zero line = %q, want an explicit 0 (confirmed idle)", lines[2])
	}
}

func TestBuildServiceStatusEmbedEmpty(t *testing.T) {
	embed := buildServiceStatusEmbed(serviceStatusResp{})
	if len(embed.Fields) != 0 {
		t.Errorf("Fields = %+v, want none for an empty response", embed.Fields)
	}
	if !strings.Contains(strings.ToLower(embed.Description), "unavailable") {
		t.Errorf("Description = %q, want it to say status is unavailable", embed.Description)
	}
}

// TestBuildServiceStatusEmbedLargeFleetStaysWithinDiscordCaps synthesizes a
// fleet far bigger than the live stack (30 groups x 10 services = 300
// services) to prove the embed truncates deterministically instead of
// exceeding Discord's 25-field / 1024-char-per-field / 6000-total-char
// embed limits.
func TestBuildServiceStatusEmbedLargeFleetStaysWithinDiscordCaps(t *testing.T) {
	var groups []serviceStatusGroup
	for g := 0; g < 30; g++ {
		var svcs []serviceStatusEntry
		for s := 0; s < 10; s++ {
			svcs = append(svcs, serviceStatusEntry{
				Name: fmt.Sprintf("service-%d-%d-with-a-fairly-long-name", g, s), Routed: true,
				HealthyReplicas: 2, TotalReplicas: 2, State: "up",
				Requests5m: intPtr(1234), CPUPercent: 12.3,
				MemUsedBytes: 123456789, MemLimitBytes: 536870912,
			})
		}
		groups = append(groups, serviceStatusGroup{Group: fmt.Sprintf("group-%d", g), Services: svcs})
	}
	resp := serviceStatusResp{Groups: groups}
	embed := buildServiceStatusEmbed(resp)

	if len(embed.Fields) > discordMaxFields {
		t.Errorf("Fields = %d, want <= %d", len(embed.Fields), discordMaxFields)
	}
	total := len(embed.Title) + len(embed.Description)
	for i, f := range embed.Fields {
		if len(f.Value) > discordMaxFieldValueLen {
			t.Errorf("Fields[%d] (%s) value len = %d, want <= %d", i, f.Name, len(f.Value), discordMaxFieldValueLen)
		}
		total += len(f.Name) + len(f.Value)
	}
	if total > discordMaxTotalLen {
		t.Errorf("total embed length = %d, want <= %d", total, discordMaxTotalLen)
	}
	// The last field should be the deterministic "+N more" truncation note,
	// since 30 groups can't fit within discordMaxFields.
	last := embed.Fields[len(embed.Fields)-1]
	if !strings.Contains(last.Value, "more") {
		t.Errorf("last field = %+v, want a truncation note", last)
	}
}

// TestBuildServiceStatusEmbedManyServicesInOneGroupTruncatesField covers the
// other truncation axis: a single group with too many services to fit in
// one field's 1024-char value.
func TestBuildServiceStatusEmbedManyServicesInOneGroupTruncatesField(t *testing.T) {
	var svcs []serviceStatusEntry
	for i := 0; i < 60; i++ {
		svcs = append(svcs, serviceStatusEntry{
			Name: fmt.Sprintf("service-with-a-somewhat-long-descriptive-name-%d", i), Routed: true,
			HealthyReplicas: 1, TotalReplicas: 1, State: "up", Requests5m: intPtr(1), CPUPercent: 1.0,
		})
	}
	resp := serviceStatusResp{Groups: []serviceStatusGroup{{Group: "huge-group", Services: svcs}}}
	embed := buildServiceStatusEmbed(resp)
	if len(embed.Fields) != 1 {
		t.Fatalf("Fields = %d, want 1", len(embed.Fields))
	}
	value := embed.Fields[0].Value
	if len(value) > discordMaxFieldValueLen {
		t.Errorf("field value len = %d, want <= %d", len(value), discordMaxFieldValueLen)
	}
	if !strings.Contains(value, "more") {
		t.Errorf("value = %q, want a truncation note since 60 services can't fit", value)
	}
}

func TestServiceLineIcons(t *testing.T) {
	cases := map[string]string{"up": "✅", "degraded": "⚠️", "down": "🔴", "unknown-state": "❔"}
	for state, icon := range cases {
		line := serviceLine(serviceStatusEntry{Name: "x", State: state})
		if !strings.HasPrefix(line, icon) {
			t.Errorf("serviceLine(state=%q) = %q, want prefix %q", state, line, icon)
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

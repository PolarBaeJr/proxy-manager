package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestBuildStatusPagesCountAndOrder(t *testing.T) {
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
	pages := buildStatusPages(resp)
	if len(pages) != 3 {
		t.Fatalf("len(pages) = %d, want 3 (overview + 2 groups)", len(pages))
	}
	if len(pages[0].Fields) != 2 {
		t.Errorf("overview Fields = %d, want one per group (2)", len(pages[0].Fields))
	}
	if pages[0].Fields[0].Name != "✅ badminton" {
		t.Errorf("overview Fields[0].Name = %q, want %q", pages[0].Fields[0].Name, "✅ badminton")
	}
	if pages[0].Fields[1].Name != "⚠️ market-tracker" {
		t.Errorf("overview Fields[1].Name = %q, want %q", pages[0].Fields[1].Name, "⚠️ market-tracker")
	}

	if !strings.HasPrefix(pages[1].Title, "📁 badminton") {
		t.Errorf("pages[1].Title = %q, want it to start with the group name", pages[1].Title)
	}
	if len(pages[1].Fields) != 2 {
		t.Fatalf("pages[1].Fields = %d, want one per service (2)", len(pages[1].Fields))
	}
	if pages[1].Fields[0].Name != "✅ player" {
		t.Errorf("pages[1].Fields[0].Name = %q, want %q", pages[1].Fields[0].Name, "✅ player")
	}

	for i, p := range pages {
		want := fmt.Sprintf("Page %d/%d", i+1, len(pages))
		if p.Footer == nil || p.Footer.Text != want {
			t.Errorf("pages[%d].Footer = %+v, want text %q", i, p.Footer, want)
		}
	}
}

func TestBuildStatusPagesDashForNullOrUnroutedRate(t *testing.T) {
	resp := serviceStatusResp{
		Groups: []serviceStatusGroup{
			{Group: "g", Services: []serviceStatusEntry{
				{Name: "unrouted", Routed: false, State: "up", Requests5m: intPtr(0)}, // even if the backend somehow sent a number, Routed:false wins
				{Name: "null-rate", Routed: true, State: "up", Requests5m: nil},
				{Name: "routed", Routed: true, State: "up", Requests5m: intPtr(0)},
			}},
		},
	}
	pages := buildStatusPages(resp)
	fields := pages[1].Fields
	if len(fields) != 3 {
		t.Fatalf("fields = %d, want 3", len(fields))
	}
	if !strings.Contains(fields[0].Value, "— req/5m") {
		t.Errorf("unrouted value = %q, want a dash, not 0", fields[0].Value)
	}
	if !strings.Contains(fields[1].Value, "— req/5m") {
		t.Errorf("null-rate value = %q, want a dash, not 0", fields[1].Value)
	}
	if !strings.Contains(fields[2].Value, "0 req/5m") {
		t.Errorf("routed/zero value = %q, want an explicit 0 (confirmed idle)", fields[2].Value)
	}
}

func TestBuildStatusPagesEmpty(t *testing.T) {
	pages := buildStatusPages(serviceStatusResp{})
	if len(pages) != 1 {
		t.Fatalf("len(pages) = %d, want 1 for an empty response", len(pages))
	}
	if !strings.Contains(strings.ToLower(pages[0].Description), "unavailable") {
		t.Errorf("Description = %q, want it to say status is unavailable", pages[0].Description)
	}
}

// TestBuildStatusPagesLargeFleetStaysWithinDiscordCaps synthesizes a fleet
// far bigger than the live stack (30 groups x 10 services = 300 services) to
// prove every page — overview and per-group — stays within Discord's
// 25-field / 1024-char-per-field / 6000-total-char embed limits.
func TestBuildStatusPagesLargeFleetStaysWithinDiscordCaps(t *testing.T) {
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
	pages := buildStatusPages(resp)
	if len(pages) != 31 {
		t.Fatalf("len(pages) = %d, want 31 (overview + 30 groups)", len(pages))
	}
	for pi, embed := range pages {
		if len(embed.Fields) > discordMaxFields {
			t.Errorf("pages[%d] Fields = %d, want <= %d", pi, len(embed.Fields), discordMaxFields)
		}
		total := len(embed.Title) + len(embed.Description)
		for i, f := range embed.Fields {
			if len(f.Value) > discordMaxFieldValueLen {
				t.Errorf("pages[%d] Fields[%d] (%s) value len = %d, want <= %d", pi, i, f.Name, len(f.Value), discordMaxFieldValueLen)
			}
			total += len(f.Name) + len(f.Value)
		}
		if total > discordMaxTotalLen {
			t.Errorf("pages[%d] total embed length = %d, want <= %d", pi, total, discordMaxTotalLen)
		}
	}
}

// TestBuildStatusPagesManyServicesInOneGroupTruncatesFields covers a single
// group with too many services to fit in one page's 25 fields.
func TestBuildStatusPagesManyServicesInOneGroupTruncatesFields(t *testing.T) {
	var svcs []serviceStatusEntry
	for i := 0; i < 60; i++ {
		svcs = append(svcs, serviceStatusEntry{
			Name: fmt.Sprintf("service-with-a-somewhat-long-descriptive-name-%d", i), Routed: true,
			HealthyReplicas: 1, TotalReplicas: 1, State: "up", Requests5m: intPtr(1), CPUPercent: 1.0,
		})
	}
	resp := serviceStatusResp{Groups: []serviceStatusGroup{{Group: "huge-group", Services: svcs}}}
	pages := buildStatusPages(resp)
	fields := pages[1].Fields
	if len(fields) > discordMaxFields {
		t.Errorf("Fields = %d, want <= %d", len(fields), discordMaxFields)
	}
	last := fields[len(fields)-1]
	if !strings.Contains(last.Value, "more") {
		t.Errorf("last field = %+v, want a truncation note since 60 services can't fit", last)
	}
}

func TestBuildStatusPagesShowsMachineLabel(t *testing.T) {
	resp := serviceStatusResp{
		Groups: []serviceStatusGroup{
			{Group: "core", Services: []serviceStatusEntry{{Name: "proxy", State: "up"}}}, // Machine unset — this host's own group
			{Group: "badminton-mac-player", Machine: "dashboard-mac.polardev.org", Services: []serviceStatusEntry{
				{Name: "player", State: "up"},
			}},
		},
	}
	pages := buildStatusPages(resp)
	if pages[0].Fields[0].Name != "✅ core" {
		t.Errorf("overview Fields[0].Name = %q, want no machine suffix for an unset Machine", pages[0].Fields[0].Name)
	}
	if pages[0].Fields[1].Name != "✅ badminton-mac-player · dashboard-mac" {
		t.Errorf("overview Fields[1].Name = %q, want a trimmed machine suffix", pages[0].Fields[1].Name)
	}
	if pages[1].Title != "📁 core: ✅ Up" {
		t.Errorf("pages[1].Title = %q, want no machine suffix", pages[1].Title)
	}
	if pages[2].Title != "📁 badminton-mac-player · dashboard-mac: ✅ Up" {
		t.Errorf("pages[2].Title = %q, want a trimmed machine suffix", pages[2].Title)
	}
}

func TestStatusPageComponentsBoundaries(t *testing.T) {
	cases := []struct {
		name             string
		page, total      int
		wantRow          bool
		wantBackDisabled bool
		wantNextDisabled bool
	}{
		{"single page", 0, 1, false, false, false},
		{"first of many", 0, 3, true, true, false},
		{"middle", 1, 3, true, false, false},
		{"last", 2, 3, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			comps := statusPageComponents(c.page, c.total)
			if !c.wantRow {
				if len(comps) != 0 {
					t.Fatalf("components = %+v, want none", comps)
				}
				return
			}
			if len(comps) != 1 {
				t.Fatalf("len(components) = %d, want 1 action row", len(comps))
			}
			row, ok := comps[0].(discordgo.ActionsRow)
			if !ok {
				t.Fatalf("components[0] = %T, want discordgo.ActionsRow", comps[0])
			}
			if len(row.Components) != 2 {
				t.Fatalf("len(row.Components) = %d, want 2 buttons", len(row.Components))
			}
			back := row.Components[0].(discordgo.Button)
			next := row.Components[1].(discordgo.Button)
			if back.Disabled != c.wantBackDisabled {
				t.Errorf("back.Disabled = %v, want %v", back.Disabled, c.wantBackDisabled)
			}
			if next.Disabled != c.wantNextDisabled {
				t.Errorf("next.Disabled = %v, want %v", next.Disabled, c.wantNextDisabled)
			}
			if back.CustomID != statusPageCustomIDBack || next.CustomID != statusPageCustomIDNext {
				t.Errorf("custom ids = %q, %q, want %q, %q", back.CustomID, next.CustomID, statusPageCustomIDBack, statusPageCustomIDNext)
			}
		})
	}
}

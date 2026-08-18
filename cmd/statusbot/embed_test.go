package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBuildStatusEmbedUp(t *testing.T) {
	hs := healthStatus{Status: "up", CheckedAt: "2026-01-01T00:00:00Z"}
	embed := buildStatusEmbed(hs, nil)
	if !strings.Contains(embed.Title, "✅") || !strings.Contains(embed.Title, "Up") {
		t.Errorf("Title = %q, want it to mention ✅ and Up", embed.Title)
	}
	if embed.Color != colorUp {
		t.Errorf("Color = %#x, want %#x", embed.Color, colorUp)
	}
	if embed.Description == "" || embed.Description != statusReply(hs, nil) {
		t.Errorf("Description = %q, want it to match statusReply", embed.Description)
	}
}

func TestBuildStatusEmbedDegraded(t *testing.T) {
	hs := healthStatus{Status: "degraded", CheckedAt: "2026-01-01T00:00:00Z", Targets: []healthTarget{
		{Name: "proxy", Health: "down"},
		{Name: "edge", Health: "up"},
	}}
	embed := buildStatusEmbed(hs, nil)
	if !strings.Contains(embed.Title, "⚠️") || !strings.Contains(embed.Title, "Degraded") {
		t.Errorf("Title = %q, want it to mention ⚠️ and Degraded", embed.Title)
	}
	if embed.Color != colorDegraded {
		t.Errorf("Color = %#x, want %#x", embed.Color, colorDegraded)
	}
	if embed.Description == "" || embed.Description != statusReply(hs, nil) {
		t.Errorf("Description = %q, want it to match statusReply", embed.Description)
	}
	if len(embed.Fields) != 1 {
		t.Fatalf("Fields = %+v, want exactly 1", embed.Fields)
	}
	if embed.Fields[0].Name != "proxy" {
		t.Errorf("Fields[0].Name = %q, want %q", embed.Fields[0].Name, "proxy")
	}
}

func TestBuildStatusEmbedUnreachable(t *testing.T) {
	embed := buildStatusEmbed(healthStatus{}, context.DeadlineExceeded)
	if !strings.Contains(embed.Title, "🔴") || !strings.Contains(embed.Title, "Unreachable") {
		t.Errorf("Title = %q, want it to mention 🔴 and Unreachable", embed.Title)
	}
	if embed.Color != colorUnreachable {
		t.Errorf("Color = %#x, want %#x", embed.Color, colorUnreachable)
	}
	if embed.Description == "" || embed.Description != statusReply(healthStatus{}, context.DeadlineExceeded) {
		t.Errorf("Description = %q, want it to match statusReply", embed.Description)
	}
}

func TestBuildStatusEmbedTimestampFallback(t *testing.T) {
	embed := buildStatusEmbed(healthStatus{Status: "up", CheckedAt: ""}, nil)
	if embed.Timestamp == "" {
		t.Fatalf("Timestamp = %q, want non-empty", embed.Timestamp)
	}
	if _, err := time.Parse(time.RFC3339, embed.Timestamp); err != nil {
		t.Errorf("Timestamp = %q, want valid RFC3339: %v", embed.Timestamp, err)
	}
}

func TestBuildStatusEmbedTimestampFromCheckedAt(t *testing.T) {
	embed := buildStatusEmbed(healthStatus{Status: "up", CheckedAt: "2026-01-01T00:00:00Z"}, nil)
	if embed.Timestamp != "2026-01-01T00:00:00Z" {
		t.Errorf("Timestamp = %q, want %q", embed.Timestamp, "2026-01-01T00:00:00Z")
	}
}

func TestStartupEmbedNeutral(t *testing.T) {
	embed := startupEmbed()
	if embed.Color != colorNeutral {
		t.Errorf("Color = %#x, want %#x", embed.Color, colorNeutral)
	}
	desc := strings.ToLower(embed.Description)
	if strings.Contains(desc, "unreachable") || strings.Contains(desc, "🔴") {
		t.Errorf("Description = %q, want no red-alert wording", embed.Description)
	}
}

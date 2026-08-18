package main

import (
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	colorUp          = 0x2ECC71
	colorDegraded    = 0xF1C40F
	colorUnreachable = 0xE74C3C
	colorNeutral     = 0x95A5A6
)

// buildStatusEmbed renders the most recent poll result (hs, err) as a
// Discord embed. It reuses statusReply's text as the embed body so status
// wording has one source of truth; it only adds Discord-specific styling
// (title/color/fields/timestamp) on top. Shared by the proactive
// live-status message and the on-demand !status reply.
func buildStatusEmbed(hs healthStatus, err error) *discordgo.MessageEmbed {
	cur := classify(hs, err)
	embed := &discordgo.MessageEmbed{
		Description: statusReply(hs, err),
	}
	switch cur {
	case "up":
		embed.Title = "✅ Status: Up"
		embed.Color = colorUp
	case "degraded":
		embed.Title = "⚠️ Status: Degraded"
		embed.Color = colorDegraded
		for _, t := range hs.Targets {
			if t.Health != "up" {
				embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
					Name: t.Name, Value: t.Health, Inline: true,
				})
			}
		}
	default: // "unreachable"
		embed.Title = "🔴 Status: Unreachable"
		embed.Color = colorUnreachable
	}
	if ts, perr := time.Parse(time.RFC3339, hs.CheckedAt); perr == nil {
		embed.Timestamp = ts.Format(time.RFC3339)
	} else {
		embed.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	return embed
}

// startupEmbed is the confirmation posted by /set-alert-channel when no
// health check has completed yet (lastStatus == "" — the very first
// interaction after a fresh deploy, racing the baseline poll() in main()).
// Without this, buildStatusEmbed would render a misleading red
// "Unreachable" as the bot's first-ever post in the new channel, because
// classify(healthStatus{}, nil) == "unreachable" is how a not-yet-checked
// state and a genuine unreachable state are indistinguishable today (see
// health.go's classify — pre-existing, out of scope to change here).
func startupEmbed() *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       "statusbot",
		Description: "✅ alerts will be posted here — no health check completed yet.",
		Color:       colorNeutral,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
}

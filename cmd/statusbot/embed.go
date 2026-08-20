package main

import (
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	colorUp          = 0x2ECC71
	colorDegraded    = 0xF1C40F
	colorUnreachable = 0xE74C3C
)

// buildStatusEmbed renders the most recent poll result (hs, err) as a
// Discord embed. It reuses statusReply's text as the embed body so status
// wording has one source of truth; it only adds Discord-specific styling
// (title/color/fields/timestamp) on top. Used by the on-demand !status
// reply.
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

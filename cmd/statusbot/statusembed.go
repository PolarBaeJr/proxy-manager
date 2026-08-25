package main

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Discord embed hard limits (25 fields, 1024 chars per field value, 6000
// chars total across the whole embed — see
// https://discord.com/developers/docs/resources/channel#embed-limits).
// statuspager.go's per-group pages stay well under these for today's fleet
// size, but still truncate deterministically (a trailing "+N more" field)
// as a defensive backstop.
const (
	discordMaxFields        = 25
	discordMaxFieldValueLen = 1024
	discordMaxTotalLen      = 6000
	// Headroom below discordMaxTotalLen for the title, description, and a
	// possible trailing "+N more" summary field, so the running total
	// tracked in statuspager.go never gets close to the real cap.
	statusEmbedTotalBudget = discordMaxTotalLen - 400
)

func stateIcon(state string) string {
	switch state {
	case "up":
		return "✅"
	case "degraded":
		return "⚠️"
	case "down":
		return "🔴"
	default:
		return "❔"
	}
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for nn := n / unit; nn >= unit; nn /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// statusUnavailableEmbed renders a clear "no data" embed instead of leaving
// a stale service-status message in place — used when the fetch failed
// (including no dashboard API token being available) or the response
// decoded to zero groups.
func statusUnavailableEmbed(err error) *discordgo.MessageEmbed {
	desc := "⚠️ service status unavailable"
	if err != nil {
		desc += ": " + err.Error()
	}
	return &discordgo.MessageEmbed{
		Title:       "📊 Service Status",
		Description: desc,
		Color:       colorUnreachable,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
}

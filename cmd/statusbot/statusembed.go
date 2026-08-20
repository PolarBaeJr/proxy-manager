package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Discord embed hard limits (25 fields, 1024 chars per field value, 6000
// chars total across the whole embed — see
// https://discord.com/developers/docs/resources/channel#embed-limits). A
// one-field-per-service design would blow these on a modest fleet (the live
// stack already has ~15 services across several groups), so
// buildServiceStatusEmbed renders one field PER GROUP with each service as
// a compact line inside that field's value, and truncates deterministically
// (a trailing "+N more" field/line) if a fleet still doesn't fit.
const (
	discordMaxFields        = 25
	discordMaxFieldValueLen = 1024
	discordMaxTotalLen      = 6000
	// Headroom below discordMaxTotalLen for the title, description, and a
	// possible trailing "+N more group(s)" summary field, so the running
	// total tracked below never gets close to the real cap.
	statusEmbedTotalBudget = discordMaxTotalLen - 400
)

// buildServiceStatusEmbed renders /api/service-status's grouped response as
// a compact Discord embed.
func buildServiceStatusEmbed(resp serviceStatusResp) *discordgo.MessageEmbed {
	if len(resp.Groups) == 0 {
		return statusUnavailableEmbed(nil)
	}

	up, degraded, down := 0, 0, 0
	for _, g := range resp.Groups {
		for _, s := range g.Services {
			switch s.State {
			case "degraded":
				degraded++
			case "down":
				down++
			default:
				up++
			}
		}
	}
	overall, color := "✅ Up", colorUp
	switch {
	case down > 0:
		overall, color = "🔴 Down", colorUnreachable
	case degraded > 0:
		overall, color = "⚠️ Degraded", colorDegraded
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📊 Service Status: " + overall,
		Description: fmt.Sprintf("%d up · %d degraded · %d down across %d group(s)", up, degraded, down, len(resp.Groups)),
		Color:       color,
	}
	if resp.SampledAt.IsZero() {
		embed.Timestamp = time.Now().UTC().Format(time.RFC3339)
	} else {
		embed.Timestamp = resp.SampledAt.UTC().Format(time.RFC3339)
	}

	total := len(embed.Title) + len(embed.Description)
	for i, g := range resp.Groups {
		remaining := len(resp.Groups) - i
		value := groupFieldValue(g.Services)
		// Reserve a trailing "+N more" field if this group would push us
		// past the field-count or char budget and there's more to show —
		// checked before appending, not after, so the budget can never be
		// exceeded by one group's worth of overshoot.
		wouldExceedFields := len(embed.Fields) >= discordMaxFields-1 && remaining > 1
		wouldExceedChars := total+len(g.Group)+len(value) > statusEmbedTotalBudget && remaining > 1
		if wouldExceedFields || wouldExceedChars {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:  "…",
				Value: fmt.Sprintf("+%d more group(s) not shown", remaining),
			})
			break
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  g.Group,
			Value: value,
		})
		total += len(g.Group) + len(value)
	}
	return embed
}

// groupFieldValue renders one group's services as compact lines, truncated
// to fit a single Discord field value (1024 chars) with a deterministic
// "+N more" trailer if the group itself has too many services to list.
func groupFieldValue(services []serviceStatusEntry) string {
	const budget = discordMaxFieldValueLen - 40 // headroom for a trailing "+N more" line
	out := ""
	shown := 0
	for _, s := range services {
		line := serviceLine(s)
		next := line
		if out != "" {
			next = out + "\n" + line
		}
		if len(next) > budget {
			break
		}
		out = next
		shown++
	}
	if shown < len(services) {
		suffix := fmt.Sprintf("+%d more", len(services)-shown)
		if out != "" {
			out += "\n" + suffix
		} else {
			out = suffix
		}
	}
	if out == "" {
		out = "—"
	}
	return out
}

// serviceLine renders one service's status/rate/resource usage as a single
// compact line. rate is "—" (never 0) when the service isn't routed or
// requests_5m is null — 0 would misleadingly read as "confirmed idle".
func serviceLine(s serviceStatusEntry) string {
	rate := "—"
	if s.Routed && s.Requests5m != nil {
		rate = strconv.Itoa(*s.Requests5m)
		if s.RateTruncated {
			rate += "+"
		}
	}
	return fmt.Sprintf("%s **%s** %d/%d · %s req/5m · %.1f%% cpu · %s/%s mem",
		stateIcon(s.State), s.Name, s.HealthyReplicas, s.TotalReplicas, rate, s.CPUPercent,
		humanBytes(s.MemUsedBytes), humanBytes(s.MemLimitBytes))
}

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
// (including a missing DASHBOARD_API_TOKEN) or the response decoded to zero
// groups.
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

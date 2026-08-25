package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// statusPageCustomIDBack/Next are the persistent status message's Back/Next
// button custom_ids — matched by the InteractionMessageComponent handler in
// main.go to tell status-page navigation apart from any other component
// interaction the bot might grow later.
const (
	statusPageCustomIDBack = "statuspage:back"
	statusPageCustomIDNext = "statuspage:next"
)

// buildStatusPages renders /api/service-status's grouped response as a
// sequence of embeds: page 0 is an overview (aggregate counts plus one
// compact line per group), and pages 1..len(Groups) each detail a single
// group's services in full — one embed field per service. Splitting one
// group per page (rather than the old single all-groups embed) means each
// page has the full ~6000-char/25-field budget to itself, so a group's
// services no longer need the old compact single-line-per-service format.
func buildStatusPages(resp serviceStatusResp) []*discordgo.MessageEmbed {
	if len(resp.Groups) == 0 {
		return []*discordgo.MessageEmbed{statusUnavailableEmbed(nil)}
	}
	pages := make([]*discordgo.MessageEmbed, 0, len(resp.Groups)+1)
	pages = append(pages, overviewPage(resp))
	for _, g := range resp.Groups {
		pages = append(pages, groupPage(g))
	}
	for i, p := range pages {
		p.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d/%d", i+1, len(pages))}
	}
	return pages
}

// tallyStates counts services by state — shared by overviewPage (across all
// groups) and groupPage (within one group).
func tallyStates(services []serviceStatusEntry) (up, degraded, down int) {
	for _, s := range services {
		switch s.State {
		case "degraded":
			degraded++
		case "down":
			down++
		default:
			up++
		}
	}
	return up, degraded, down
}

func overallStatus(down, degraded int) (label string, color int) {
	switch {
	case down > 0:
		return "🔴 Down", colorUnreachable
	case degraded > 0:
		return "⚠️ Degraded", colorDegraded
	default:
		return "✅ Up", colorUp
	}
}

func overviewPage(resp serviceStatusResp) *discordgo.MessageEmbed {
	var allServices []serviceStatusEntry
	for _, g := range resp.Groups {
		allServices = append(allServices, g.Services...)
	}
	up, degraded, down := tallyStates(allServices)
	overall, color := overallStatus(down, degraded)

	embed := &discordgo.MessageEmbed{
		Title: "📊 Service Status: " + overall,
		Description: fmt.Sprintf("%d up · %d degraded · %d down across %d group(s)\nUse ◀ Back / Next ▶ to see each group's services in detail.",
			up, degraded, down, len(resp.Groups)),
		Color: color,
	}
	if resp.SampledAt.IsZero() {
		embed.Timestamp = time.Now().UTC().Format(time.RFC3339)
	} else {
		embed.Timestamp = resp.SampledAt.UTC().Format(time.RFC3339)
	}
	// One field per host in resp.Hosts, rendered before the per-group fields
	// so a down peer's "unreachable" summary sits right at the top of the
	// overview page, side by side with the other host(s). Unlike the group
	// loop below, this one is NOT capped against discordMaxFields — fine at
	// today's 2-node mesh, but a roster past ~25 hosts would overflow
	// Discord's per-embed field limit and get the whole message rejected.
	for _, h := range resp.Hosts {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("%s %s", hostIcon(h), trimMachineLabel(h.Machine)),
			Value:  hostSummary(h),
			Inline: true,
		})
	}

	// Even a compact one-line-per-group field can't exceed discordMaxFields
	// groups on the overview page — a big enough fleet (e.g. today's 30+
	// group synthetic test, or a mesh with several peers merged in) needs
	// the same deterministic "+N more" truncation groupPage already applies
	// per-service. maxFields also reserves one slot per host field added
	// above, so N host fields can't silently push real groups into "+N
	// more" truncation.
	maxFields := discordMaxFields - 1 - len(resp.Hosts) // headroom for a trailing "+N more" field
	if maxFields < 0 {
		maxFields = 0
	}
	shown := 0
	for _, g := range resp.Groups {
		if shown >= maxFields {
			break
		}
		gUp, gDegraded, gDown := tallyStates(g.Services)
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("%s %s%s", stateIcon(overviewGroupState(gDown, gDegraded)), g.Group, machineSuffix(g.Machine)),
			Value:  fmt.Sprintf("%d up · %d degraded · %d down", gUp, gDegraded, gDown),
			Inline: true,
		})
		shown++
	}
	if shown < len(resp.Groups) {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  "…",
			Value: fmt.Sprintf("+%d more group(s) not shown", len(resp.Groups)-shown),
		})
	}
	return embed
}

// trimMachineLabel strips the shared ".polardev.org" suffix off a Machine
// identity so it stays short — shared by machineSuffix (group labels) and
// the host-roster field names in overviewPage.
func trimMachineLabel(machine string) string {
	return strings.TrimSuffix(machine, ".polardev.org")
}

// machineSuffix renders a group's origin host as a short " · label" tag, or
// "" when Machine is unset (this host's own groups, or a mesh with no peers
// configured at all — the common case, and the only case before any peer is
// wired up).
func machineSuffix(machine string) string {
	if machine == "" {
		return ""
	}
	return " · " + trimMachineLabel(machine)
}

// hostIcon maps one host-roster entry to the same icon set stateIcon uses
// elsewhere. !Reachable must be checked before Status — an unreachable
// host's Status is always "" (never populated on the failure path), which
// would otherwise fall through to the "up" icon.
func hostIcon(h hostHealth) string {
	if !h.Reachable {
		return stateIcon("down")
	}
	if h.Status == "degraded" {
		return stateIcon("degraded")
	}
	return stateIcon("up")
}

// hostSummary renders one host-roster field's value.
func hostSummary(h hostHealth) string {
	if !h.Reachable {
		return "unreachable"
	}
	if h.Status == "degraded" {
		return "⚠️ degraded — " + degradedSummary(h.Targets)
	}
	return "up"
}

// overviewGroupState maps a group's tally back to the "up"/"degraded"/"down"
// vocabulary stateIcon expects, so the overview's per-group icons match the
// same icon set used everywhere else instead of inventing a second one.
func overviewGroupState(down, degraded int) string {
	switch {
	case down > 0:
		return "down"
	case degraded > 0:
		return "degraded"
	default:
		return "up"
	}
}

func groupPage(g serviceStatusGroup) *discordgo.MessageEmbed {
	up, degraded, down := tallyStates(g.Services)
	overall, color := overallStatus(down, degraded)

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("📁 %s%s: %s", g.Group, machineSuffix(g.Machine), overall),
		Description: fmt.Sprintf("%d up · %d degraded · %d down", up, degraded, down),
		Color:       color,
	}
	const maxFields = discordMaxFields - 1 // headroom for a trailing "+N more" field
	total := len(embed.Title) + len(embed.Description)
	shown := 0
	for _, s := range g.Services {
		name := fmt.Sprintf("%s %s", stateIcon(s.State), s.Name)
		value := serviceDetail(s)
		if shown >= maxFields || total+len(name)+len(value) > statusEmbedTotalBudget {
			break
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: name, Value: value, Inline: true})
		total += len(name) + len(value)
		shown++
	}
	if shown < len(g.Services) {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  "…",
			Value: fmt.Sprintf("+%d more service(s) not shown", len(g.Services)-shown),
		})
	}
	return embed
}

// serviceDetail renders one service's replica/rate/resource detail as a
// multi-line field value — a group page has room for this per service since
// it's no longer sharing a field budget with every other group.
func serviceDetail(s serviceStatusEntry) string {
	rate := "—"
	if s.Routed && s.Requests5m != nil {
		rate = strconv.Itoa(*s.Requests5m)
		if s.RateTruncated {
			rate += "+"
		}
	}
	return fmt.Sprintf("%d/%d replicas · %s req/5m\n%.1f%% cpu · %s/%s mem",
		s.HealthyReplicas, s.TotalReplicas, rate, s.CPUPercent,
		humanBytes(s.MemUsedBytes), humanBytes(s.MemLimitBytes))
}

// statusPageComponents returns the Back/Next button row for the given
// 0-indexed page out of total pages, disabling whichever end has nowhere to
// go. total <= 1 (fetch failed, or a fleet with zero groups) returns an
// empty (not nil) slice — ChannelMessageEditComplex needs a non-nil pointer
// to an empty slice to actually clear a previous message's buttons; a nil
// slice would marshal to JSON null and Discord would leave the old row in
// place.
func statusPageComponents(page, total int) []discordgo.MessageComponent {
	if total <= 1 {
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "◀ Back",
					Style:    discordgo.SecondaryButton,
					CustomID: statusPageCustomIDBack,
					Disabled: page <= 0,
				},
				discordgo.Button{
					Label:    "Next ▶",
					Style:    discordgo.SecondaryButton,
					CustomID: statusPageCustomIDNext,
					Disabled: page >= total-1,
				},
			},
		},
	}
}

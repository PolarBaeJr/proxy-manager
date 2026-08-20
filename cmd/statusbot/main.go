// statusbot: a Discord bot, deployed as its own container separate from
// dashboard/proxy/edge, that polls the dashboard's authenticated
// GET /api/service-status endpoint (see servicestatus.go/statusembed.go) on
// a fixed ticker and keeps a single persistent "service status" embed — one
// field per service group, with an aggregate up/degraded/down count in its
// header — edited in place on every tick rather than a new message every
// time, falling back to a new message if the edit fails (e.g. the message
// was deleted). The "/set-alert-channel" slash command (Manage Server
// permission required) lets a user move that message to whatever channel
// they invoke it in, persisted to disk so it survives a container restart —
// doubling as a capability probe (confirms the bot can post there) and the
// seed for the new channel's status message.
//
// Separately, it also polls the dashboard's public GET /api/health endpoint
// on its own slower ticker purely to keep the on-demand "!status" reply
// fresh — that poll no longer posts or edits any message on its own.
//
// Deliberately its own process/container rather than code inside the
// dashboard: if the dashboard hangs or crashes, a status check running
// inside it can't report that. It still runs on the same Pi, so it won't
// survive a full host outage — only isolates it from the dashboard/proxy
// processes specifically.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

func main() {
	healthURL := flag.String("health-url", "http://dashboard:8093/api/health", "dashboard /api/health URL to poll")
	pollInterval := flag.Duration("poll-interval", 60*time.Second, "how often to poll health")
	statusURL := flag.String("status-url", "http://dashboard:8093/api/service-status", "dashboard /api/service-status URL to poll")
	statusInterval := flag.Duration("status-interval", 15*time.Second, "how often to poll per-service status")
	tokenFile := flag.String("token-file", "/tokens/statusbot.token", "path to an auto-provisioned dashboard API token; used when DASHBOARD_API_TOKEN is unset")
	flag.Parse()

	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is required")
	}
	log.Printf("dashboard API token: DASHBOARD_API_TOKEN env overrides; otherwise read from %s each poll (written automatically by the dashboard on first boot)", *tokenFile)

	chStore, chMsgs := newChannelStoreFromEnv(os.Getenv)
	for _, m := range chMsgs {
		log.Println(m)
	}
	alertChannelID, seedMsgs := initialAlertChannel(chStore, os.Getenv)
	for _, m := range seedMsgs {
		log.Println(m)
	}
	statusMessageID := chStore.GetStatusMessageID()

	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("create discord session: %v", err)
	}
	// GuildMessages + MessageContent so the !status handler can read message
	// text — MessageContent must also be enabled for the bot application in
	// the Discord Developer Portal, or Discord silently sends empty Content.
	sess.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	client := &http.Client{Timeout: 5 * time.Second}
	var mu sync.Mutex
	var lastHealth healthStatus
	var lastErr error
	var lastStatusErr string // last service-status fetch error's message, "" = last poll succeeded; only used to dedupe repeated log lines below

	poll := func() {
		hs, err := fetchHealth(context.Background(), *healthURL, client)
		cur := classify(hs, err)
		mu.Lock()
		lastHealth, lastErr = hs, err
		mu.Unlock()
		log.Printf("health check: status=%s", cur)
	}

	pollStatus := func() {
		resp, fetchErr := fetchServiceStatus(context.Background(), *statusURL, resolveDashboardToken(*tokenFile), client)
		errMsg := ""
		if fetchErr != nil {
			errMsg = fetchErr.Error()
		}
		mu.Lock()
		shouldLog := errMsg != lastStatusErr // dedupe: only log when the error (or its absence) changes, not every tick
		lastStatusErr = errMsg
		target := alertChannelID
		msgID := statusMessageID
		mu.Unlock()
		if fetchErr != nil && shouldLog {
			log.Printf("service-status poll failed: %v", fetchErr)
		}
		if target == "" {
			return
		}
		var embed *discordgo.MessageEmbed
		if fetchErr != nil {
			embed = statusUnavailableEmbed(fetchErr)
		} else {
			embed = buildServiceStatusEmbed(resp)
		}
		newMsgID := msgID
		edited := false
		if msgID != "" {
			if _, editErr := sess.ChannelMessageEditEmbed(target, msgID, embed); editErr != nil {
				log.Printf("failed to edit service-status message %s in %s, sending a new one: %v", msgID, target, editErr)
			} else {
				edited = true
			}
		}
		if !edited {
			sent, sendErr := sess.ChannelMessageSendEmbed(target, embed)
			if sendErr != nil {
				log.Printf("failed to send service-status message: %v", sendErr)
				return
			}
			newMsgID = sent.ID
		}
		if newMsgID == msgID {
			return
		}
		mu.Lock()
		stale := alertChannelID != target // channel changed while we were doing I/O — don't clobber the new channel's message id
		if !stale {
			statusMessageID = newMsgID
		}
		mu.Unlock()
		if stale {
			return
		}
		if err := chStore.SetStatusMessageID(newMsgID); err != nil {
			log.Printf("failed to persist service-status message id: %v", err)
		}
	}

	sess.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}
		if strings.TrimSpace(strings.ToLower(m.Content)) != "!status" {
			return
		}
		mu.Lock()
		embed := buildStatusEmbed(lastHealth, lastErr)
		mu.Unlock()
		if _, err := s.ChannelMessageSendEmbed(m.ChannelID, embed); err != nil {
			log.Printf("failed to reply to !status: %v", err)
		}
	})

	// sess.Open() reads exactly one post-identify message and treats it as
	// READY without looping if it isn't (e.g. an Op9 Invalid Session forces a
	// re-identify first) — so State.User can still be nil when Open() returns
	// successfully. The real READY always arrives later via the session's
	// read-loop goroutine and is dispatched here (asynchronously — SyncEvents
	// is left at its default false, see discordgo's Session.handle), which is
	// why command registration is done here instead of right after Open().
	// This handler must be registered before sess.Open() is called, same as
	// the other handlers below, so it can't miss the first Ready dispatch.
	var registerCmdOnce sync.Once
	sess.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		if r.User == nil {
			log.Printf("ready event missing user info; will retry on next ready (e.g. resume)")
			return
		}
		registerCmdOnce.Do(func() {
			alertChannelCmdPerms := int64(discordgo.PermissionManageGuild)
			guildID := os.Getenv("DISCORD_GUILD_ID")
			if _, err := s.ApplicationCommandCreate(r.User.ID, guildID, &discordgo.ApplicationCommand{
				Name:                     "set-alert-channel",
				Description:              "Set this channel as the destination for statusbot's persistent service-status message.",
				DefaultMemberPermissions: &alertChannelCmdPerms,
			}); err != nil {
				log.Printf("failed to register /set-alert-channel command (guild=%q): %v", guildID, err)
			}
		})
		// Sent here rather than right after sess.Open() returns — Open()
		// returning without error only means our identify was accepted, not
		// that Discord's backend has finished standing up the session (same
		// race as command registration above). A presence update sent before
		// that finishes is liable to be silently dropped. Re-sent on every
		// Ready (including resumes), which is harmless and keeps the status
		// fresh if Discord ever reset it server-side.
		if err := s.UpdateStatusComplex(discordgo.UpdateStatusData{Status: "online"}); err != nil {
			log.Printf("failed to set online presence: %v", err)
		}
	})

	sess.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand ||
			i.ApplicationCommandData().Name != "set-alert-channel" {
			return
		}
		respond := func(content string) {
			if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: content,
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			}); err != nil {
				log.Printf("failed to respond to /set-alert-channel: %v", err)
			}
		}
		if i.Member == nil {
			respond("this command only works in a server channel, not a DM.")
			return
		}
		// DefaultMemberPermissions below only sets the *initial* Discord UI
		// restriction — a guild admin can loosen it afterward in
		// Server Settings > Integrations without this code knowing, so the
		// permission is re-checked here rather than trusted from registration.
		if i.Member.Permissions&discordgo.PermissionManageGuild == 0 {
			respond("🚫 you need the Manage Server permission to change the alert channel.")
			return
		}
		// Discord requires an interaction response within 3s; a background
		// poll can afford the client's full 5s timeout but a synchronous
		// probe here can't, so this uses a tighter deadline and treats a
		// timeout the same as any other fetch failure (statusUnavailableEmbed).
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, fetchErr := fetchServiceStatus(probeCtx, *statusURL, resolveDashboardToken(*tokenFile), client)
		probeCancel()
		var embed *discordgo.MessageEmbed
		if fetchErr != nil {
			embed = statusUnavailableEmbed(fetchErr)
		} else {
			embed = buildServiceStatusEmbed(resp)
		}
		// Doubles as a capability probe (confirms the bot can post here) AND
		// seeds the status message in one action — unifies "post the
		// confirmation" with "create the initial status message for this
		// channel" so there's no second, redundant post.
		sent, sendErr := s.ChannelMessageSendEmbed(i.ChannelID, embed)
		if sendErr != nil {
			log.Printf("set-alert-channel: cannot post in %s: %v", i.ChannelID, sendErr)
			respond("⚠️ I can't send messages in this channel, so I can't set it as the alert channel.")
			return
		}
		mu.Lock()
		alertChannelID = i.ChannelID
		statusMessageID = sent.ID
		mu.Unlock()
		ack := "Done."
		if err := chStore.Set(i.ChannelID); err != nil {
			log.Printf("failed to persist alert channel: %v", err)
			ack = "Done — but not saved to disk, so it will reset on the next restart."
		} else if err := chStore.SetStatusMessageID(sent.ID); err != nil {
			log.Printf("failed to persist service-status message id: %v", err)
			ack = "Done — but the service-status message id wasn't saved, so an edit later may post a duplicate."
		}
		respond(ack)
	})

	if err := sess.Open(); err != nil {
		log.Fatalf("open discord session: %v", err)
	}
	defer sess.Close()

	// One-time cleanup: delete the old simple health-alert embed left over
	// from before the two persistent messages were consolidated into just
	// the detailed one, so it doesn't sit in the channel forever showing a
	// stale status. Best-effort — a delete failure (already gone, missing
	// permission) is logged and ignored, not fatal.
	if legacyID := chStore.LegacyMessageID(); legacyID != "" && alertChannelID != "" {
		if err := sess.ChannelMessageDelete(alertChannelID, legacyID); err != nil {
			log.Printf("failed to delete legacy status message %s in %s (safe to ignore if it's already gone): %v", legacyID, alertChannelID, err)
		} else {
			log.Printf("deleted legacy status message %s in %s", legacyID, alertChannelID)
		}
	}

	mu.Lock()
	initialTarget := alertChannelID
	mu.Unlock()
	log.Printf("statusbot connected — polling %s every %s, and %s every %s, posting status to channel %q",
		*healthURL, *pollInterval, *statusURL, *statusInterval, initialTarget)

	poll()       // establish a baseline immediately so !status works right away
	pollStatus() // ditto, for the service-status embed
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()
	statusTicker := time.NewTicker(*statusInterval)
	defer statusTicker.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
			poll()
		case <-statusTicker.C:
			pollStatus()
		}
	}
}

// resolveDashboardToken prefers an explicit env override; otherwise it reads
// the token the dashboard auto-provisions on first boot. Re-read on every
// call (not cached) so a token minted by the dashboard after statusbot has
// already started is picked up on the next poll tick without a restart, and
// so deleting the file to force a manual rotation takes effect on the very
// next tick.
func resolveDashboardToken(tokenFile string) string {
	if t := os.Getenv("DASHBOARD_API_TOKEN"); t != "" {
		return t
	}
	if tokenFile == "" {
		return ""
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

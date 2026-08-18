// statusbot: a Discord bot, deployed as its own container separate from
// dashboard/proxy/edge, that polls the dashboard's public /api/health
// endpoint and posts to a Discord channel on up/degraded/unreachable
// transitions. Also answers "!status" on demand from the last poll result,
// and the "/set-alert-channel" slash command (Manage Server permission
// required) lets a user move the alert destination to whatever channel they
// invoke it in, persisted to disk so it survives a container restart.
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
	flag.Parse()

	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is required")
	}

	chStore, chMsgs := newChannelStoreFromEnv(os.Getenv)
	for _, m := range chMsgs {
		log.Println(m)
	}
	alertChannelID, seedMsgs := initialAlertChannel(chStore, os.Getenv)
	for _, m := range seedMsgs {
		log.Println(m)
	}

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
	var lastStatus string
	var lastHealth healthStatus
	var lastErr error

	poll := func() {
		hs, err := fetchHealth(context.Background(), *healthURL, client)
		mu.Lock()
		cur := classify(hs, err)
		msg := transitionMessage(lastStatus, cur, hs, err)
		lastStatus, lastHealth, lastErr = cur, hs, err
		target := alertChannelID
		mu.Unlock()
		log.Printf("health check: status=%s", cur)
		if msg == "" {
			return
		}
		if target == "" {
			log.Printf("alert suppressed (no alert channel configured): %s", msg)
			return
		}
		if _, sendErr := sess.ChannelMessageSend(target, msg); sendErr != nil {
			log.Printf("failed to send alert: %v", sendErr)
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
		reply := statusReply(lastHealth, lastErr)
		mu.Unlock()
		if _, err := s.ChannelMessageSend(m.ChannelID, reply); err != nil {
			log.Printf("failed to reply to !status: %v", err)
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
		// Doubles as a capability probe: InteractionRespond succeeding doesn't
		// prove the bot can post proactive alerts here later (it goes through
		// the interaction's own webhook token, not standing channel permissions)
		// — a real ChannelMessageSend does. Don't persist a channel the bot
		// can't actually alert into.
		if _, err := s.ChannelMessageSend(i.ChannelID, "✅ statusbot alerts will now be posted here."); err != nil {
			log.Printf("set-alert-channel: cannot post in %s: %v", i.ChannelID, err)
			respond("⚠️ I can't send messages in this channel, so I can't set it as the alert channel.")
			return
		}
		mu.Lock()
		alertChannelID = i.ChannelID
		mu.Unlock()
		ack := "Done."
		if err := chStore.Set(i.ChannelID); err != nil {
			log.Printf("failed to persist alert channel: %v", err)
			ack = "Done — but not saved to disk, so it will reset on the next restart."
		}
		respond(ack)
	})

	if err := sess.Open(); err != nil {
		log.Fatalf("open discord session: %v", err)
	}
	defer sess.Close()

	alertChannelCmdPerms := int64(discordgo.PermissionManageGuild)
	guildID := os.Getenv("DISCORD_GUILD_ID")
	if _, err := sess.ApplicationCommandCreate(sess.State.User.ID, guildID, &discordgo.ApplicationCommand{
		Name:                     "set-alert-channel",
		Description:              "Set this channel as the destination for statusbot's up/degraded/unreachable alerts.",
		DefaultMemberPermissions: &alertChannelCmdPerms,
	}); err != nil {
		log.Printf("failed to register /set-alert-channel command (guild=%q): %v", guildID, err)
	}

	mu.Lock()
	initialTarget := alertChannelID
	mu.Unlock()
	log.Printf("statusbot connected — polling %s every %s, alerts to channel %q", *healthURL, *pollInterval, initialTarget)

	poll() // establish a baseline immediately so !status works right away
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
			poll()
		}
	}
}

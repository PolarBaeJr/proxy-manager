// statusbot: a Discord bot, deployed as its own container separate from
// dashboard/proxy/edge, that polls the dashboard's public /api/health
// endpoint and posts to a Discord channel on up/degraded/unreachable
// transitions. Also answers "!status" on demand from the last poll result.
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
	channelID := os.Getenv("DISCORD_CHANNEL_ID")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is required")
	}
	if channelID == "" {
		log.Fatal("DISCORD_CHANNEL_ID is required")
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
		mu.Unlock()
		log.Printf("health check: status=%s", cur)
		if msg != "" {
			if _, sendErr := sess.ChannelMessageSend(channelID, msg); sendErr != nil {
				log.Printf("failed to send alert: %v", sendErr)
			}
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

	if err := sess.Open(); err != nil {
		log.Fatalf("open discord session: %v", err)
	}
	defer sess.Close()
	log.Printf("statusbot connected — polling %s every %s, alerts to channel %s", *healthURL, *pollInterval, channelID)

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

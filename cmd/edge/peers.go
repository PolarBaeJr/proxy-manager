package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// PeerSync gossips per-IP rate-limit consumption between edge instances.
// Every `interval`, each edge:
//   1. Snapshots PushDeltas() from its rate limiter.
//   2. POSTs the map to every peer's /gossip endpoint over the internal
//      metrics port, authenticated with a shared bearer secret.
//   3. Peers ApplyRemote() the deltas, draining tokens locally.
//
// Effect: 100 req/s cap applies across ALL edges collectively, not per edge.
// Consistency is eventual; the window between gossip ticks is the tolerance.

type PeerSync struct {
	rl       *rateLimiter
	peers    []string // full URLs, e.g. http://edge-eu.tailnet:8094
	secret   string
	interval time.Duration
	client   *http.Client
}

func newPeerSync(rl *rateLimiter, peers []string, secret string, interval time.Duration) *PeerSync {
	return &PeerSync{
		rl:       rl,
		peers:    peers,
		secret:   secret,
		interval: interval,
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        len(peers) * 2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *PeerSync) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *PeerSync) tick(ctx context.Context) {
	deltas := p.rl.PushDeltas()
	if len(deltas) == 0 {
		return
	}
	body, err := json.Marshal(deltas)
	if err != nil {
		return
	}
	for _, peer := range p.peers {
		go p.send(ctx, peer, body)
	}
}

func (p *PeerSync) send(ctx context.Context, peer string, body []byte) {
	url := strings.TrimRight(peer, "/") + "/gossip"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		// Peer unreachable — expected during restarts / network blips. Silent.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// Auth failure or malformed request: worth surfacing so misconfig is visible.
		log.Printf("edge peer gossip: %s → %s", url, resp.Status)
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}

// gossipHandler returns the HTTP handler for /gossip on the internal metrics
// port. Bearer-authenticated with the shared secret; body is a JSON map of
// {ip: delta_since_last_push}. Empty secret disables the endpoint entirely
// (returns 404) so an unconfigured edge can't accept remote drains.
func gossipHandler(rl *rateLimiter, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var deltas map[string]uint64
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&deltas); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		rl.ApplyRemote(deltas)
		w.WriteHeader(http.StatusNoContent)
	})
}

package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PeerRegistry discovers and authenticates with sibling proxy instances
// running on other hosts (e.g. Pi + Mac mini over Tailscale). Modeled on
// cmd/edge/peers.go's PeerSync/gossipHandler pair, but for this phase it
// only proves connectivity + auth — no route data crosses the wire yet
// (see docs/PEER_MESH_PLAN.md, Phase 3).
//
// Every `interval`, each proxy POSTs a placeholder handshake to every
// configured peer's /peer/handshake endpoint on the internal metrics port,
// authenticated with a shared bearer secret, and records success/failure +
// last-contact time per peer. A peer being unreachable is expected during
// restarts/network blips, not an error condition.

// peerStatus is the last-known bookkeeping for one configured peer.
type peerStatus struct {
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success"`
	OK          bool      `json:"ok"`
}

type PeerRegistry struct {
	peers    []string // full URLs, e.g. http://100.83.62.68:8094
	secret   string
	identity string
	interval time.Duration
	client   *http.Client

	mu     sync.Mutex
	status map[string]peerStatus
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func newPeerRegistry(peers []string, secret, identity string, interval time.Duration) *PeerRegistry {
	return &PeerRegistry{
		peers:    peers,
		secret:   secret,
		identity: identity,
		interval: interval,
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        len(peers) * 2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		status: map[string]peerStatus{},
	}
}

func (p *PeerRegistry) Run(ctx context.Context) {
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

func (p *PeerRegistry) tick(ctx context.Context) {
	for _, peer := range p.peers {
		go p.send(ctx, peer)
	}
}

// send POSTs a placeholder handshake payload to peer's /peer/handshake and
// records the outcome. No route data yet — that's Phase 3.
func (p *PeerRegistry) send(ctx context.Context, peer string) {
	url := strings.TrimRight(peer, "/") + "/peer/handshake"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		// Peer unreachable — expected during restarts / network blips. Silent.
		p.recordResult(peer, false)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Auth failure or malformed request: worth surfacing so misconfig is visible.
		log.Printf("proxy peer handshake: %s → %s", url, resp.Status)
	}
	p.recordResult(peer, resp.StatusCode == http.StatusOK)
}

func (p *PeerRegistry) recordResult(peer string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status[peer]
	st.LastAttempt = time.Now()
	if ok {
		st.LastSuccess = time.Now()
	}
	st.OK = ok
	p.status[peer] = st
}

// Status returns a copy of the current per-peer bookkeeping.
func (p *PeerRegistry) Status() map[string]peerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]peerStatus, len(p.status))
	for k, v := range p.status {
		out[k] = v
	}
	return out
}

// peerHandshakeHandler returns the HTTP handler for POST /peer/handshake on
// the internal metrics port. Bearer-authenticated with the shared secret,
// constant-time compare — same approach as cmd/edge/peers.go's
// gossipHandler. Empty secret disables the endpoint entirely (404) so an
// unconfigured proxy can't accept handshakes. A valid request gets a
// minimal ok:true body — no route data yet (Phase 3).
func peerHandshakeHandler(secret, identity string) http.Handler {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"peer": identity, "ok": true})
	})
}

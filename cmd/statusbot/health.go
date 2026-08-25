package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// healthStatus mirrors the dashboard's public GET /api/health response —
// see cmd/dashboard/api.go. That endpoint already aggregates proxy/edge/
// dashboard health via the monitor binary and is deliberately unauthenticated
// and sanitized (no hostnames or route detail), so statusbot needs no
// dashboard credentials at all.
type healthStatus struct {
	Status    string         `json:"status"` // "up" or "degraded"
	Targets   []healthTarget `json:"targets"`
	CheckedAt string         `json:"checked_at"`
}

type healthTarget struct {
	Name   string `json:"name"`
	Health string `json:"health"`
}

// fetchHealth polls the dashboard's /api/health endpoint. /api/health returns
// 200 for "up" and 503 for "degraded", but writes a JSON body either way — so
// only a transport failure, a non-200/503 status, or an undecodable body is
// treated as an error (folded into the synthetic "unreachable" status by the
// caller). A poll error against the dashboard's own health endpoint, reached
// over the internal edge network, most likely means the dashboard container
// itself is down or hung — exactly the failure mode this bot exists to catch
// even though it runs on the same Pi.
func fetchHealth(ctx context.Context, url string, client *http.Client) (healthStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return healthStatus{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return healthStatus{}, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return healthStatus{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var hs healthStatus
	if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
		return healthStatus{}, fmt.Errorf("decode response: %w", err)
	}
	return hs, nil
}

// classify folds a fetch error into a third synthetic status alongside the
// dashboard's own "up"/"degraded", so transitionMessage/statusReply never
// need to special-case err separately from hs.Status.
func classify(hs healthStatus, err error) string {
	if err != nil || hs.Status == "" {
		return "unreachable"
	}
	return hs.Status
}

// statusReply renders the on-demand !status reply from the most recent poll
// result — no live poll, so it answers instantly even if the target is down.
func statusReply(hs healthStatus, err error) string {
	switch classify(hs, err) {
	case "up":
		return "✅ up — all targets healthy (checked " + hs.CheckedAt + ")"
	case "unreachable":
		msg := "🔴 unreachable"
		if err != nil {
			msg += ": " + err.Error()
		}
		return msg
	default:
		return "⚠️ degraded — " + degradedSummary(hs.Targets) + " (checked " + hs.CheckedAt + ")"
	}
}

func degradedSummary(targets []healthTarget) string {
	var down []string
	for _, t := range targets {
		if t.Health != "up" {
			down = append(down, t.Name+" ("+t.Health+")")
		}
	}
	if len(down) == 0 {
		return "no per-target detail available"
	}
	return strings.Join(down, ", ")
}

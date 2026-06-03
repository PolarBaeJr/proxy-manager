// monitor: scrapes /metrics from edge + proxy, keeps a rolling time series,
// exposes aggregated stats over /api/* so the dashboard can render them.
//
// Health classification per target:
//   - up    : last scrape OK + uptime increasing
//   - flaky : intermittent scrape failures in the last minute
//   - down  : last N scrapes all failed
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":8095", "monitor listen address")
	targetsFlag := flag.String("targets", "edge=http://edge:8094/metrics,proxy=http://proxy:8094/metrics", "comma-separated name=url pairs to scrape")
	interval := flag.Duration("interval", 5*time.Second, "scrape interval")
	window := flag.Duration("window", 1*time.Hour, "how much history to retain in memory")
	flag.Parse()

	targets := parseTargets(*targetsFlag)
	if len(targets) == 0 {
		log.Fatal("no -targets provided")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(*window, *interval)
	scraper := NewScraper(targets, *interval, store)
	go scraper.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// Current snapshot of all targets + per-target health classification.
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, store.Snapshot())
	})

	// Last N time-series points for a target, useful for sparklines.
	// Query: /api/series?target=proxy&field=total&points=60
	mux.HandleFunc("/api/series", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, store.Series(r.URL.Query().Get("target"), r.URL.Query().Get("field")))
	})

	// Aggregated view across all targets. Convenient for the dashboard.
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, store.Overview())
	})

	log.Printf("monitor on %s — scraping %d target(s) every %s", *addr, len(targets), *interval)
	if err := http.ListenAndServe(*addr, mux); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func parseTargets(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			log.Printf("ignoring malformed target %q (need name=url)", pair)
			continue
		}
		out[pair[:eq]] = pair[eq+1:]
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

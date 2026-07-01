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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
)

func main() {
	addr := flag.String("addr", ":8095", "monitor listen address")
	targetsFlag := flag.String("targets", "edge=http://edge:8094/metrics,proxy=http://proxy:8094/metrics", "comma-separated name=url pairs to scrape")
	interval := flag.Duration("interval", 5*time.Second, "scrape interval")
	window := flag.Duration("window", 1*time.Hour, "how much history to retain in memory")
	probeTargets := flag.String("tls-probe-targets", "", "comma-separated TLS probe targets (sni[@host:port]); empty disables cert probing")
	probeInterval := flag.Duration("tls-probe-interval", 15*time.Minute, "TLS probe interval")
	probeDial := flag.String("tls-probe-default-dial", "host.docker.internal:443", "default dial target for probe entries without @host:port")
	statePath := flag.String("state", "/data/monitor-state.json", "store persistence file")
	stateInterval := flag.Duration("state-interval", 60*time.Second, "how often to snapshot the store to -state")
	flag.Parse()

	targets := parseTargets(*targetsFlag)
	if len(targets) == 0 {
		log.Fatal("no -targets provided")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(*window, *interval)
	// Restore BEFORE the scraper starts so the first Record() already has the
	// prior series to compute deltas against.
	if st, ok := loadStoreState(*statePath); ok {
		store.restoreState(st)
		log.Printf("restored monitor state from %s (%d target(s), saved %s)",
			*statePath, len(st.Targets), st.SavedAt.Format(time.RFC3339))
	}
	go persistLoop(ctx, *statePath, *stateInterval, store)
	saveOnShutdown(*statePath, store)
	scraper := NewScraper(targets, *interval, store)
	go scraper.Run(ctx)

	// Optional cert prober — runs only if -tls-probe-targets is non-empty.
	var prober *CertProber
	if *probeTargets != "" {
		prober = NewCertProber(parseProbeTargets(*probeTargets, *probeDial), *probeInterval)
		go prober.Run(ctx)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// Current snapshot of all targets + per-target health classification.
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, store.Snapshot())
	})

	// Last N time-series points for a target, useful for sparklines.
	// Query: /api/series?target=proxy&field=total&points=60
	mux.HandleFunc("/api/series", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, store.Series(r.URL.Query().Get("target"), r.URL.Query().Get("field")))
	})

	// Aggregated view across all targets. Convenient for the dashboard.
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, store.Overview())
	})

	// Per-target detail. Path: /api/target/{name}[/hosts|/errors|/series]
	mux.HandleFunc("/api/target/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/target/")
		if rest == "" {
			http.NotFound(w, r)
			return
		}
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		sub := ""
		if len(parts) == 2 {
			sub = parts[1]
		}
		switch sub {
		case "":
			t := store.Target(name)
			if t == nil {
				http.NotFound(w, r)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, t)
		case "hosts":
			h := store.TargetHosts(name)
			if h == nil {
				h = []map[string]any{}
			}
			httpx.WriteJSON(w, http.StatusOK, h)
		case "errors":
			e := store.TargetErrors(name)
			if e == nil {
				e = map[string]any{}
			}
			httpx.WriteJSON(w, http.StatusOK, e)
		case "series":
			field := r.URL.Query().Get("field")
			httpx.WriteJSON(w, http.StatusOK, store.Series(name, field))
		default:
			http.NotFound(w, r)
		}
	})

	// TLS cert info per probed hostname.
	mux.HandleFunc("/api/certs", func(w http.ResponseWriter, _ *http.Request) {
		if prober == nil {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": false, "certs": []any{}})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"enabled":      true,
			"worst_status": prober.Worst(),
			"certs":        prober.Snapshot(),
		})
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

// ---- store persistence: periodic JSON snapshots, best-effort ----
// Same pattern as the proxy's persist.go — the ~30 duplicated lines per
// binary are deliberate, the binaries don't share packages.

func loadStoreState(path string) (storeState, bool) {
	var st storeState
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("monitor state: read %s: %v", path, err)
		}
		return storeState{}, false
	}
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("monitor state: parse %s: %v (starting fresh)", path, err)
		return storeState{}, false
	}
	if st.Version != storeStateVersion {
		log.Printf("monitor state: %s has version %d, want %d (starting fresh)", path, st.Version, storeStateVersion)
		return storeState{}, false
	}
	return st, true
}

func saveStoreState(path string, s *Store) error {
	st := s.exportState()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write to a sibling temp file, then rename — a same-directory rename is
	// atomic, so readers never see a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func persistLoop(ctx context.Context, path string, interval time.Duration, s *Store) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := saveStoreState(path, s); err != nil {
				log.Printf("monitor state: save: %v", err)
			}
		}
	}
}

// saveOnShutdown flushes one final snapshot on SIGTERM/interrupt, then exits.
func saveOnShutdown(path string, s *Store) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-ch
		if err := saveStoreState(path, s); err != nil {
			log.Printf("monitor state: shutdown save: %v", err)
		}
		os.Exit(0)
	}()
}


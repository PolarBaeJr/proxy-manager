package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type Scraper struct {
	targets  map[string]string
	interval time.Duration
	store    *Store
	http     *http.Client
}

func NewScraper(targets map[string]string, interval time.Duration, store *Store) *Scraper {
	return &Scraper{
		targets:  targets,
		interval: interval,
		store:    store,
		http:     &http.Client{Timeout: 3 * time.Second},
	}
}

func (s *Scraper) Run(ctx context.Context) {
	s.scrapeAll(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scrapeAll(ctx)
		}
	}
}

func (s *Scraper) scrapeAll(ctx context.Context) {
	for name, url := range s.targets {
		data, err := s.scrape(ctx, url)
		s.store.Record(name, url, data, err)
	}
}

func (s *Scraper) scrape(ctx context.Context, url string) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, errStatus(resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	log.Printf("scrape ok: %s", url)
	return out, nil
}

type httpStatusErr int

func (e httpStatusErr) Error() string { return "http " + itoa(int(e)) }
func errStatus(n int) error           { return httpStatusErr(n) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = '0' + byte(n%10)
		n /= 10
	}
	return string(b[i:])
}

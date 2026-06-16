package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-image base ref → list of user-marked "this build is good, keep it
// findable for rollback" entries. Pure metadata; nothing here moves running
// containers around — that's the user's `docker compose up -d <svc>` after
// they edit the tag env var.
//
// Image base = registry path without the :tag suffix
// (e.g. "ghcr.io/polarbaejr/proxy-manager-dashboard").

type StableMark struct {
	Tag      string    `json:"tag"`
	Label    string    `json:"label,omitempty"`
	MarkedAt time.Time `json:"marked_at"`
	MarkedBy string    `json:"marked_by,omitempty"`
}

type releasesData struct {
	Marks map[string][]StableMark `json:"marks"`
}

type ReleasesStore struct {
	mu   sync.RWMutex
	path string
	data releasesData
}

func loadReleasesStore(path string) (*ReleasesStore, error) {
	rs := &ReleasesStore{path: path, data: releasesData{Marks: map[string][]StableMark{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return rs, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &rs.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if rs.data.Marks == nil {
		rs.data.Marks = map[string][]StableMark{}
	}
	return rs, nil
}

func (s *ReleasesStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

func (s *ReleasesStore) List(imageBase string) []StableMark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]StableMark(nil), s.data.Marks[imageBase]...)
	sort.Slice(out, func(i, j int) bool { return out[i].MarkedAt.After(out[j].MarkedAt) })
	return out
}

func (s *ReleasesStore) Mark(imageBase, tag, label, by string) error {
	if tag == "" {
		return fmt.Errorf("tag required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// De-dupe by tag — re-marking just updates label + bumps timestamp.
	existing := s.data.Marks[imageBase]
	filtered := existing[:0]
	for _, m := range existing {
		if m.Tag != tag {
			filtered = append(filtered, m)
		}
	}
	filtered = append(filtered, StableMark{Tag: tag, Label: label, MarkedAt: time.Now().UTC(), MarkedBy: by})
	s.data.Marks[imageBase] = filtered
	return s.save()
}

func (s *ReleasesStore) Unmark(imageBase, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.data.Marks[imageBase]
	if !ok {
		return nil
	}
	filtered := existing[:0]
	for _, m := range existing {
		if m.Tag != tag {
			filtered = append(filtered, m)
		}
	}
	s.data.Marks[imageBase] = filtered
	return s.save()
}

// splitImageRef parses a docker image reference into base + tag.
// "ghcr.io/foo/bar:sha-abc" → ("ghcr.io/foo/bar", "sha-abc")
// "ghcr.io/foo/bar"        → ("ghcr.io/foo/bar", "latest")
// Ignores @sha256:... digest suffixes (treated as opaque tag).
func splitImageRef(ref string) (base, tag string) {
	if i := strings.LastIndex(ref, "@"); i != -1 {
		// digest pin — base is everything before @, tag is empty.
		return ref[:i], ""
	}
	if i := strings.LastIndex(ref, ":"); i != -1 {
		// : could be part of a host:port. Only treat as tag if there's no /
		// after it (i.e. the colon is in the last path segment).
		if !strings.Contains(ref[i:], "/") {
			return ref[:i], ref[i+1:]
		}
	}
	return ref, "latest"
}

// ----- GHCR tag listing (anonymous via docker registry v2 API) -----
//
// The GitHub Packages REST API requires `read:packages` even for public
// packages, but the underlying docker registry at ghcr.io serves anonymous
// pull tokens to anyone, and the v2 /tags/list endpoint just works. Two
// hops: token endpoint → tags list.

// listGHCRTags returns the bare list of tags ghcr.io knows about for the
// given image base ref. Returns (nil, nil) for non-GHCR refs or 404s.
func listGHCRTags(ctx context.Context, imageBase string) ([]string, error) {
	const prefix = "ghcr.io/"
	if !strings.HasPrefix(imageBase, prefix) {
		return nil, nil
	}
	repo := strings.TrimPrefix(imageBase, prefix)
	if !strings.Contains(repo, "/") {
		return nil, nil
	}
	client := &http.Client{Timeout: 10 * time.Second}

	// 1. Anonymous pull token.
	tokURL := fmt.Sprintf("https://ghcr.io/token?service=ghcr.io&scope=repository:%s:pull", repo)
	tReq, _ := http.NewRequestWithContext(ctx, "GET", tokURL, nil)
	tResp, err := client.Do(tReq)
	if err != nil {
		return nil, err
	}
	defer tResp.Body.Close()
	if tResp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ghcr token: %d", tResp.StatusCode)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tResp.Body).Decode(&tok); err != nil {
		return nil, err
	}

	// 2. Tags list.
	url := fmt.Sprintf("https://ghcr.io/v2/%s/tags/list", repo)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ghcr tags: %d", resp.StatusCode)
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Tags, nil
}

// inspectImage returns the Config.Image field for a container by name.
// Used to read the currently-running tag for the infra services.
func (c *dockerClient) inspectImage(ctx context.Context, name string) (string, error) {
	body, err := c.get(ctx, "/containers/"+name+"/json")
	if err != nil {
		return "", err
	}
	defer body.Close()
	var resp struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return "", err
	}
	return resp.Config.Image, nil
}

// infraServices is the static list of compose-managed services this
// dashboard knows how to track release history for.
var infraServices = []string{"dashboard", "proxy", "edge", "monitor"}

type releaseInfoEntry struct {
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	IsCurrent bool      `json:"is_current,omitempty"`
	IsStable  bool      `json:"is_stable,omitempty"`
	Label     string    `json:"label,omitempty"`
	MarkedAt  time.Time `json:"marked_at,omitempty"`
}

type releaseInfoResp struct {
	Service    string             `json:"service"`
	ImageBase  string             `json:"image_base"`
	CurrentRef string             `json:"current_ref"`
	CurrentTag string             `json:"current_tag"`
	EnvVar     string             `json:"env_var"`
	Entries    []releaseInfoEntry `json:"entries"`
}

// buildReleaseInfo combines: currently-running tag (from docker inspect),
// user-marked stable entries (from local store), and all GHCR tags (from
// the public packages API). The dashboard renders these as a unified list.
func buildReleaseInfo(ctx context.Context, dc *dockerClient, rs *ReleasesStore, service string) (*releaseInfoResp, error) {
	curRef, err := dc.inspectImage(ctx, service)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", service, err)
	}
	base, tag := splitImageRef(curRef)
	out := &releaseInfoResp{
		Service:    service,
		ImageBase:  base,
		CurrentRef: curRef,
		CurrentTag: tag,
		EnvVar:     strings.ToUpper(service) + "_TAG",
	}

	// Index marks by tag for fast lookup.
	markByTag := map[string]StableMark{}
	for _, m := range rs.List(base) {
		markByTag[m.Tag] = m
	}

	// Pull all GHCR tags for this base (best-effort; ignored on failure).
	seen := map[string]bool{}
	tags, _ := listGHCRTags(ctx, base)
	for _, t := range tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		e := releaseInfoEntry{Tag: t}
		if t == tag {
			e.IsCurrent = true
		}
		if m, ok := markByTag[t]; ok {
			e.IsStable = true
			e.Label = m.Label
			e.MarkedAt = m.MarkedAt
		}
		out.Entries = append(out.Entries, e)
	}

	// Surface any stable marks for tags that GHCR doesn't list (e.g. user
	// marked an older tag that was later pruned). They still show up so the
	// user can see they did mark something, even if it's gone.
	for tagName, m := range markByTag {
		if seen[tagName] {
			continue
		}
		out.Entries = append(out.Entries, releaseInfoEntry{
			Tag: tagName, IsStable: true, Label: m.Label, MarkedAt: m.MarkedAt,
		})
	}

	// Ensure the currently-running tag is always present, even if GHCR
	// hasn't returned it (e.g. a one-off local build still in service).
	if !seen[tag] && tag != "" {
		out.Entries = append([]releaseInfoEntry{{Tag: tag, IsCurrent: true}}, out.Entries...)
	}

	sort.SliceStable(out.Entries, func(i, j int) bool {
		// Current first, then stable, then by tag name desc (puts run-N
		// in reverse-numeric-ish order, sha-* and latest fall through).
		if out.Entries[i].IsCurrent != out.Entries[j].IsCurrent {
			return out.Entries[i].IsCurrent
		}
		if out.Entries[i].IsStable != out.Entries[j].IsStable {
			return out.Entries[i].IsStable
		}
		return out.Entries[i].Tag > out.Entries[j].Tag
	})
	return out, nil
}

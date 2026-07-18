package main

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
)

// imageChecker periodically polls the Docker registry (via the local daemon)
// for fresh digests of the images currently in use, and compares against the
// locally-pulled digest. Services running an image with a newer remote digest
// are flagged "update available" on the dashboard.
//
// Uses the Docker engine's /distribution/{name}/json endpoint — that does the
// registry conversation for us, including auth. Works for any registry the
// engine itself can pull from.

const imageCheckInterval = 10 * time.Minute

type imageStatus struct {
	Image           string    `json:"image"`
	LocalDigest     string    `json:"local_digest,omitempty"`
	RegistryDigest  string    `json:"registry_digest,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	LastChecked     time.Time `json:"last_checked"`
	Err             string    `json:"error,omitempty"`
}

type imageChecker struct {
	mu       sync.RWMutex
	statuses map[string]*imageStatus
	dc       *dockerClient
}

func newImageChecker(dc *dockerClient) *imageChecker {
	return &imageChecker{statuses: map[string]*imageStatus{}, dc: dc}
}

// Get returns the cached status for an image (or nil if never checked).
func (ic *imageChecker) Get(image string) *imageStatus {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	return ic.statuses[image]
}

// Check runs one update for one image.
func (ic *imageChecker) Check(ctx context.Context, image string) {
	status := &imageStatus{Image: image, LastChecked: time.Now()}
	defer func() {
		ic.mu.Lock()
		ic.statuses[image] = status
		ic.mu.Unlock()
	}()

	// Local digest from /images/{name}/json → RepoDigests
	if body, err := ic.dc.get(ctx, "/images/"+url.PathEscape(image)+"/json"); err == nil {
		var resp struct {
			RepoDigests []string `json:"RepoDigests"`
		}
		_ = json.NewDecoder(body).Decode(&resp)
		body.Close()
		for _, d := range resp.RepoDigests {
			if i := strings.Index(d, "@"); i != -1 {
				status.LocalDigest = d[i+1:]
				break
			}
		}
	} else {
		status.Err = "no local image"
		return
	}

	// Registry digest from /distribution/{name}/json. This is what newer Docker
	// engines expose; older ones may 404, in which case we skip silently.
	body, err := ic.dc.get(ctx, "/distribution/"+url.PathEscape(image)+"/json")
	if err != nil {
		status.Err = err.Error()
		return
	}
	defer body.Close()
	var dist struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	if err := json.NewDecoder(body).Decode(&dist); err != nil {
		status.Err = "decode: " + err.Error()
		return
	}
	status.RegistryDigest = dist.Descriptor.Digest
	if status.LocalDigest != "" && status.RegistryDigest != "" {
		status.UpdateAvailable = status.LocalDigest != status.RegistryDigest
	}
}

// Loop runs Check on every image returned by allImages, on a tick. Idempotent;
// safe to call once at startup. afterCycle (optional) runs synchronously after
// each scan — same goroutine, so cycles never overlap.
func (ic *imageChecker) Loop(ctx context.Context, allImages func() []string, afterCycle func(context.Context)) {
	do := func() {
		seen := map[string]bool{}
		for _, img := range allImages() {
			if img == "" || seen[img] {
				continue
			}
			seen[img] = true
			ic.Check(ctx, img)
		}
		if afterCycle != nil {
			afterCycle(ctx)
		}
	}
	do()
	tick := time.NewTicker(imageCheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			log.Printf("image-check: scanning %d image(s)", len(ic.statuses))
			do()
		}
	}
}

// Image history + phase-out. Tracks every image ref a managed service has
// run (so old versions stay findable after a Replace/Promote), and powers the
// dashboard's Images panel: view local images per service, mark versions
// stable (protected), delete individual images, and bulk-prune old ones to
// reclaim disk. Deletes LOCAL images only — never touches a registry.
//
// SAFETY: history is metadata only. Protection sets (what must never be
// deleted) are always recomputed live from the Docker daemon + current
// services + stable marks at delete/prune time — see protectedSets.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// imageHistoryCap bounds per-service history. Evicting the oldest is safe:
// stable marks live in the ReleasesStore, and any image still running gets
// re-recorded on the next tick.
const imageHistoryCap = 50

type ImageRecord struct {
	Repo      string    `json:"repo"`
	Tag       string    `json:"tag,omitempty"`
	Ref       string    `json:"ref"`
	ImageID   string    `json:"image_id,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type ImageHistoryStore struct {
	path string
	mu   sync.RWMutex
	data map[string][]ImageRecord // service name → records
}

func loadImageHistoryStore(path string) (*ImageHistoryStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &ImageHistoryStore{path: path, data: map[string][]ImageRecord{}}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.data) // corrupt file → start fresh
		if s.data == nil {
			s.data = map[string][]ImageRecord{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *ImageHistoryStore) save() error {
	b, _ := json.MarshalIndent(s.data, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *ImageHistoryStore) List(service string) []ImageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ImageRecord, len(s.data[service]))
	copy(out, s.data[service])
	return out
}

func (s *ImageHistoryStore) ServiceNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Record upserts the currently-running image (and canary, when staged) of
// every managed service into the history. Keyed by Ref, falling back to
// ImageID when the ref is empty. FirstSeen is preserved; LastSeen refreshes.
func (s *ImageHistoryStore) Record(svcs []Service, onb []OnboardedService) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	upsert := func(service, ref, imageID string) {
		if service == "" || (ref == "" && imageID == "") {
			return
		}
		recs := s.data[service]
		for i := range recs {
			hit := (ref != "" && recs[i].Ref == ref) ||
				(ref == "" && imageID != "" && recs[i].ImageID == imageID)
			if hit {
				recs[i].LastSeen = now
				if imageID != "" {
					recs[i].ImageID = imageID
				}
				changed = true
				return
			}
		}
		repo, tag := "", ""
		if ref != "" {
			repo, tag = splitImageRef(ref)
		}
		recs = append(recs, ImageRecord{
			Repo: repo, Tag: tag, Ref: ref, ImageID: imageID,
			FirstSeen: now, LastSeen: now,
		})
		if len(recs) > imageHistoryCap {
			sort.Slice(recs, func(i, j int) bool { return recs[i].LastSeen.After(recs[j].LastSeen) })
			recs = recs[:imageHistoryCap]
		}
		s.data[service] = recs
		changed = true
	}
	for _, svc := range svcs {
		upsert(svc.Name, svc.Image, svc.ImageID)
		if svc.CanaryImage != "" {
			upsert(svc.Name, svc.CanaryImage, "")
		}
	}
	for _, o := range onb {
		upsert(o.Name, o.Image, "")
		if o.CanaryImage != "" {
			upsert(o.Name, o.CanaryImage, "")
		}
	}
	if changed {
		_ = s.save()
	}
}

// ---- Prune decision (pure — unit-tested, no IO) ----

type imgMeta struct {
	Ref       string
	ID        string
	Tagged    bool
	SizeBytes int64
	LastSeen  time.Time
	Created   int64
}

// imagesToPrune returns the delete tokens for a "keep stable + running +
// last N" prune: images are ordered newest-first by LastSeen (falling back
// to Created when LastSeen is unknown), the first keepN are kept, and of the
// rest anything protected is skipped. Tagged images emit their Ref (untag
// semantics); dangling ones emit their ID.
func imagesToPrune(onDisk []imgMeta, protectedRefs, protectedIDs map[string]bool, keepN int) []string {
	if keepN < 0 {
		keepN = 0
	}
	metas := append([]imgMeta(nil), onDisk...)
	seenAt := func(m imgMeta) time.Time {
		if !m.LastSeen.IsZero() {
			return m.LastSeen
		}
		return time.Unix(m.Created, 0)
	}
	sort.SliceStable(metas, func(i, j int) bool { return seenAt(metas[i]).After(seenAt(metas[j])) })
	var out []string
	for i, m := range metas {
		if i < keepN {
			continue
		}
		if protectedRefs[m.Ref] || protectedIDs[m.ID] {
			continue
		}
		token := m.ID
		if m.Tagged {
			token = m.Ref
		}
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

// ---- Protection + join helpers (IO) ----

// mergedServices appends onboarded-only services to the labeled list (same
// merge the /api/services GET does) so protection and grouping cover both.
func mergedServices(svcs []Service, onb []OnboardedService) []Service {
	out := append([]Service(nil), svcs...)
	idx := map[string]bool{}
	for i := range out {
		idx[out[i].Name] = true
	}
	for _, o := range onb {
		if idx[o.Name] {
			continue
		}
		out = append(out, Service{Name: o.Name, Image: o.Image, CanaryImage: o.CanaryImage, Onboarded: true})
	}
	return out
}

// protectedSets computes, LIVE from the daemon, everything that must never
// be deleted: the image ref + ID of every container (running AND stopped),
// every current service image (live + canary), and every stable-marked
// base:tag ref. Called fresh at delete/prune time — never cached, never
// derived from history.
func protectedSets(ctx context.Context, dc *dockerClient, rs *ReleasesStore, svcs []Service) (protectedRefs, protectedIDs map[string]bool, err error) {
	protectedRefs = map[string]bool{}
	protectedIDs = map[string]bool{}
	containers, err := dc.listAll(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	for _, ct := range containers {
		if ct.Image != "" {
			protectedRefs[ct.Image] = true
		}
		if ct.ImageID != "" {
			protectedIDs[ct.ImageID] = true
		}
	}
	for _, s := range svcs {
		if s.Image != "" {
			protectedRefs[s.Image] = true
		}
		if s.ImageID != "" {
			protectedIDs[s.ImageID] = true
		}
		if s.CanaryImage != "" {
			protectedRefs[s.CanaryImage] = true
		}
	}
	for base, marks := range rs.All() {
		for _, m := range marks {
			if m.Tag != "" {
				protectedRefs[base+":"+m.Tag] = true
			}
		}
	}
	return protectedRefs, protectedIDs, nil
}

// ---- GET /api/images response ----

type imageEntry struct {
	Ref         string `json:"ref"`
	Tag         string `json:"tag,omitempty"`
	ShortID     string `json:"short_id,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"` // unique size (Size - SharedSize); approx
	FirstSeen   int64  `json:"first_seen,omitempty"` // unix seconds; 0 = unknown
	LastSeen    int64  `json:"last_seen,omitempty"`
	IsCurrent   bool   `json:"is_current,omitempty"`
	IsStable    bool   `json:"is_stable,omitempty"`
	StableLabel string `json:"stable_label,omitempty"`
	MarkedBy    string `json:"marked_by,omitempty"`
	OnDisk      bool   `json:"on_disk,omitempty"`
	Referenced  bool   `json:"referenced,omitempty"` // some container (running or stopped) uses it
	Protected   bool   `json:"protected,omitempty"`
	DeleteToken string `json:"delete_token,omitempty"` // present only when on_disk && !protected

	// internal (not serialized) — used by the prune handler
	fullID  string
	tagged  bool
	created int64
}

type imageServiceInfo struct {
	Service          string       `json:"service"`
	Repos            []string     `json:"repos"`
	CurrentRef       string       `json:"current_ref,omitempty"`
	Entries          []imageEntry `json:"entries"`
	ReclaimableBytes int64        `json:"reclaimable_bytes"`
}

type imagesInfoResp struct {
	Services              []imageServiceInfo `json:"services"`
	TotalReclaimableBytes int64              `json:"total_reclaimable_bytes"`
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// buildImagesInfo joins on-disk images, history, current services, and
// stable marks into the per-service view the Images panel renders.
// Protection here is computed live (protectedSets) — and recomputed again
// at actual delete time.
func buildImagesInfo(ctx context.Context, dc *dockerClient, rs *ReleasesStore, ih *ImageHistoryStore, svcs []Service, onb []OnboardedService) (*imagesInfoResp, error) {
	merged := mergedServices(svcs, onb)
	protectedRefs, protectedIDs, err := protectedSets(ctx, dc, rs, merged)
	if err != nil {
		return nil, err
	}
	onDisk, err := dc.listImages(ctx)
	if err != nil {
		return nil, err
	}
	marks := rs.All()

	svcByName := map[string]Service{}
	names := map[string]bool{}
	for _, s := range merged {
		svcByName[s.Name] = s
		names[s.Name] = true
	}
	// Include history-only services so already-replaced services' leftovers
	// stay visible and prunable even after the service itself is gone.
	for _, n := range ih.ServiceNames() {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	digestBase := func(rd string) string {
		if i := strings.Index(rd, "@"); i != -1 {
			return rd[:i]
		}
		return rd
	}

	resp := &imagesInfoResp{Services: []imageServiceInfo{}}
	globalCounted := map[string]bool{}
	for _, name := range sorted {
		s := svcByName[name]
		hist := ih.List(name)
		repos := map[string]bool{}
		if s.Image != "" {
			b, _ := splitImageRef(s.Image)
			repos[b] = true
		}
		if s.CanaryImage != "" {
			b, _ := splitImageRef(s.CanaryImage)
			repos[b] = true
		}
		for _, r := range hist {
			if r.Repo != "" {
				repos[r.Repo] = true
			}
		}
		if len(repos) == 0 {
			continue
		}

		entries := map[string]*imageEntry{}
		ensure := func(key string) *imageEntry {
			e, ok := entries[key]
			if !ok {
				e = &imageEntry{}
				entries[key] = e
			}
			return e
		}
		// 1. On-disk images whose tag (or digest) base matches this service.
		for _, img := range onDisk {
			matchedTag := false
			for _, rt := range img.RepoTags {
				if rt == "<none>:<none>" {
					continue
				}
				b, _ := splitImageRef(rt)
				if !repos[b] {
					continue
				}
				matchedTag = true
				e := ensure(rt)
				e.Ref = rt
				e.OnDisk = true
				e.fullID = img.Id
				e.tagged = true
				e.created = img.Created
				if sz := img.Size - img.SharedSize; sz > 0 {
					e.SizeBytes = sz
				}
			}
			if !matchedTag {
				for _, rd := range img.RepoDigests {
					if !repos[digestBase(rd)] {
						continue
					}
					// Dangling (untagged) image from one of our repos.
					e := ensure(img.Id)
					e.Ref = rd
					e.OnDisk = true
					e.fullID = img.Id
					e.tagged = false
					e.created = img.Created
					if sz := img.Size - img.SharedSize; sz > 0 {
						e.SizeBytes = sz
					}
					break
				}
			}
		}
		// 2. History records: merge seen-times into disk entries, surface
		// off-disk history as informational rows.
		for _, r := range hist {
			key := r.Ref
			if key == "" {
				key = r.ImageID
			}
			if key == "" {
				continue
			}
			e, ok := entries[key]
			if !ok && r.ImageID != "" {
				e, ok = entries[r.ImageID]
			}
			if !ok {
				e = ensure(key)
				e.Ref = r.Ref
				if e.Ref == "" {
					e.Ref = r.ImageID
				}
			}
			if e.fullID == "" {
				e.fullID = r.ImageID
			}
			if e.Tag == "" {
				e.Tag = r.Tag
			}
			if !r.FirstSeen.IsZero() {
				e.FirstSeen = r.FirstSeen.Unix()
			}
			if !r.LastSeen.IsZero() {
				e.LastSeen = r.LastSeen.Unix()
			}
		}

		info := imageServiceInfo{Service: name, CurrentRef: s.Image, Entries: []imageEntry{}}
		for b := range repos {
			info.Repos = append(info.Repos, b)
		}
		sort.Strings(info.Repos)
		svcCounted := map[string]bool{}
		for _, e := range entries {
			if e.Tag == "" && e.tagged {
				_, e.Tag = splitImageRef(e.Ref)
			}
			if e.fullID != "" {
				e.ShortID = shortImageID(e.fullID)
			}
			if e.Ref == s.Image || (s.CanaryImage != "" && e.Ref == s.CanaryImage) ||
				(s.ImageID != "" && e.fullID == s.ImageID) {
				e.IsCurrent = true
			}
			if e.Tag != "" {
				base, _ := splitImageRef(e.Ref)
				for _, m := range marks[base] {
					if m.Tag == e.Tag {
						e.IsStable = true
						e.StableLabel = m.Label
						e.MarkedBy = m.MarkedBy
					}
				}
			}
			e.Referenced = e.fullID != "" && protectedIDs[e.fullID]
			e.Protected = protectedRefs[e.Ref] || e.Referenced
			if e.OnDisk && !e.Protected {
				if e.tagged {
					e.DeleteToken = e.Ref
				} else {
					e.DeleteToken = e.fullID
				}
				if e.fullID != "" && !svcCounted[e.fullID] {
					svcCounted[e.fullID] = true
					info.ReclaimableBytes += e.SizeBytes
				}
				if e.fullID != "" && !globalCounted[e.fullID] {
					globalCounted[e.fullID] = true
					resp.TotalReclaimableBytes += e.SizeBytes
				}
			}
			info.Entries = append(info.Entries, *e)
		}
		if len(info.Entries) == 0 {
			continue
		}
		sort.Slice(info.Entries, func(i, j int) bool {
			a, b := info.Entries[i], info.Entries[j]
			if a.LastSeen != b.LastSeen {
				return a.LastSeen > b.LastSeen
			}
			if a.created != b.created {
				return a.created > b.created
			}
			return a.Ref < b.Ref
		})
		resp.Services = append(resp.Services, info)
	}
	return resp, nil
}

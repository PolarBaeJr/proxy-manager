// Per-app maintenance pages — an app ships its own 503 page, the dashboard
// caches it where nginx can find it.
//
// nginx side (snippets/maintenance.conf, on the Pi):
//
//	location @maintenance {
//	    root /var/www/maintenance;
//	    try_files /hosts/$host/index.html /index.html =503;
//	}
//
// So a host with hosts/<host>/index.html gets its own page and every other
// host falls back to the shared default. That lookup is pure nginx: it works
// whether the file was put there by this file or dropped by hand.
//
// App side: a container carrying
//
//	proxy.maintenance: "/app/maintenance.html"
//
// declares a path INSIDE ITS OWN IMAGE. The dashboard copies that file out to
// hosts/<proxy.host>/index.html.
//
// The copy is eager, on a timer — NOT on maintenance-on. That is the whole
// design constraint: maintenance is typically switched on because the app is
// being redeployed or has fallen over, so extracting the page at flip time
// would fail in exactly the case the feature exists for. Syncing continuously
// means the page is already on disk before it's ever needed. Docker's archive
// endpoint reads a STOPPED container's filesystem too, so a restart loop still
// refreshes fine; only removing the container entirely drops back to default.
//
// Reconciliation only ever deletes directories carrying a .managed marker, so
// a hand-written hosts/<host>/index.html is left alone forever.
package main

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultMaintPageDir = "/var/www/maint-pages"

	// labelMaintPage is an absolute path inside the container's own filesystem.
	labelMaintPage = "proxy.maintenance"

	// maintPageMarker distinguishes a directory this process created from one
	// the operator wrote by hand. Only marked directories are ever removed.
	maintPageMarker = ".managed"

	// maxMaintPageBytes caps what we copy out. The page is served to the public
	// from the host's own name, and an image can hold anything at that path —
	// an oversized file is skipped with a log rather than truncated, since half
	// an HTML document renders as garbage.
	maxMaintPageBytes = 256 << 10

	maintPageSyncInterval = 2 * time.Minute
)

// maintPageStore is the bind-mounted per-host page directory. A nil store means
// the feature isn't configured; like maintStore, every method degrades to an
// error instead of panicking so the caller can stay unconditional.
type maintPageStore struct {
	dir string
}

// newMaintPageFromEnv resolves MAINT_PAGE_DIR (default /var/www/maint-pages).
//
// Deliberately does NOT MkdirAll, for the same reason newMaintFromEnv doesn't:
// on a host without the bind mount that would create the directory inside the
// container's ephemeral layer, and every page written there would be read by
// no nginx while the dashboard reported the feature working.
func newMaintPageFromEnv(getenv func(string) string) (*maintPageStore, []string) {
	dir := strings.TrimSpace(getenv("MAINT_PAGE_DIR"))
	if dir == "" {
		dir = defaultMaintPageDir
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, []string{"⚠ per-app maintenance pages disabled: " + dir + " not available: " + err.Error()}
	}
	if !st.IsDir() {
		return nil, []string{"⚠ per-app maintenance pages disabled: " + dir + " is not a directory"}
	}
	return &maintPageStore{dir: dir}, nil
}

// hostDir is the ONLY place a caller-supplied host becomes a filesystem path.
// Same idiom as maintStore.path: normalize, check against the filename
// allowlist, then re-check the joined result is still a direct child of the
// page directory. This one matters more than the flag-file version — what
// lands here is HTML served to the public, not an empty marker.
func (s *maintPageStore) hostDir(host string) (string, error) {
	if s == nil {
		return "", os.ErrInvalid
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if !validMaintHost(h) {
		return "", os.ErrInvalid
	}
	p := filepath.Join(s.dir, h)
	if filepath.Dir(p) != filepath.Clean(s.dir) {
		return "", os.ErrInvalid
	}
	return p, nil
}

// Write installs html as the maintenance page for host and marks the directory
// as ours. The page is written to a temp file and renamed so nginx, which may
// be serving it concurrently, never sees a half-written document.
func (s *maintPageStore) Write(host string, html []byte) error {
	dir, err := s.hostDir(host)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, maintPageMarker), nil, 0o644); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".index.html.tmp")
	if err := os.WriteFile(tmp, html, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "index.html")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Remove deletes a page directory, but only one this process created. A
// directory without the marker was hand-made and is left untouched — losing an
// operator's page to a label typo would be a nasty surprise.
func (s *maintPageStore) Remove(host string) error {
	dir, err := s.hostDir(host)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, maintPageMarker)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}

// List returns the hosts that currently have a custom page, sorted. Both
// managed and hand-written pages count: the point is to tell the UI which
// hosts will NOT get the default page, and nginx doesn't care who wrote it.
func (s *maintPageStore) List() ([]string, error) {
	if s == nil {
		return nil, os.ErrInvalid
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if !e.IsDir() || !validMaintHost(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.dir, e.Name(), "index.html")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// managedHosts lists only the directories this process owns — the set
// reconciliation is allowed to delete from.
func (s *maintPageStore) managedHosts() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if !e.IsDir() || !validMaintHost(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.dir, e.Name(), maintPageMarker)); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// Sync makes the page directory match the labels currently on disk: every host
// whose container declares proxy.maintenance gets its page (re)written, and
// every managed host that no longer declares one is removed. Idempotent and
// authoritative, so it self-heals after a label edit, a rename, or an
// offboarding without anyone having to clean up by hand.
//
// A per-container failure is logged and skipped rather than aborting the pass —
// one unreadable image must not stop every other app's page from refreshing.
func (s *maintPageStore) Sync(ctx context.Context, dc *dockerClient) error {
	if s == nil {
		return os.ErrInvalid
	}
	cts, err := dc.listAll(ctx, "")
	if err != nil {
		return err
	}

	want := map[string]dockerContainer{}
	for _, ct := range cts {
		if ct.Labels[labelEnable] != "true" {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(ct.Labels[labelHost]))
		src := strings.TrimSpace(ct.Labels[labelMaintPage])
		if host == "" || src == "" || !validMaintHost(host) {
			continue
		}
		// Replicas share a host; any one of them can supply the page, and
		// they're all the same image. First writer wins, deterministically
		// enough — a later pass would produce identical bytes anyway.
		if _, dup := want[host]; !dup {
			want[host] = ct
		}
	}

	for host, ct := range want {
		src := strings.TrimSpace(ct.Labels[labelMaintPage])
		html, err := dc.copyFileFromContainer(ctx, ct.ID, src, maxMaintPageBytes)
		if err != nil {
			log.Printf("maintenance page: %s (%s): %v", host, ct.name(), err)
			continue
		}
		if err := s.Write(host, html); err != nil {
			log.Printf("maintenance page: write %s: %v", host, err)
		}
	}

	managed, err := s.managedHosts()
	if err != nil {
		return err
	}
	for _, host := range managed {
		if _, ok := want[host]; ok {
			continue
		}
		if err := s.Remove(host); err != nil {
			log.Printf("maintenance page: remove %s: %v", host, err)
		}
	}
	return nil
}

// SyncLoop runs Sync immediately, then on a timer. Given its own ticker rather
// than piggybacking on imageChecker.Loop: that cycle is paced for registry
// digest polling (10 min) and its single afterCycle hook already belongs to the
// auto-updater, whereas a page needs to be on disk well before the operator
// flips maintenance on.
func (s *maintPageStore) SyncLoop(ctx context.Context, dc *dockerClient) {
	t := time.NewTicker(maintPageSyncInterval)
	defer t.Stop()
	for {
		if err := s.Sync(ctx, dc); err != nil {
			log.Printf("maintenance page: sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// copyFileFromContainer reads a single regular file out of a container's
// filesystem via Docker's archive endpoint, which returns a tar stream. Works
// on a stopped container, which is the whole point here.
//
// Reads are bounded by max+1 so an oversized file is reported as such instead
// of being silently truncated into broken HTML, and the tar walk stops at the
// first regular entry so a directory path can't stream the world into memory.
func (c *dockerClient) copyFileFromContainer(ctx context.Context, id, path string, max int64) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path %q must be absolute", path)
	}
	body, err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(path))
	if err != nil {
		return nil, err
	}
	defer body.Close()

	tr := tar.NewReader(body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s: no regular file in archive", path)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > max {
			return nil, fmt.Errorf("%s: %d bytes exceeds %d limit", path, h.Size, max)
		}
		b, err := io.ReadAll(io.LimitReader(tr, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > max {
			return nil, fmt.Errorf("%s: exceeds %d limit", path, max)
		}
		return b, nil
	}
}

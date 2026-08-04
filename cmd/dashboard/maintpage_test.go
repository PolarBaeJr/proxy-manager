package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestPageStore(t *testing.T) *maintPageStore {
	t.Helper()
	return &maintPageStore{dir: t.TempDir()}
}

func TestMaintPageWriteAndList(t *testing.T) {
	s := newTestPageStore(t)
	if err := s.Write("app.polardev.org", []byte("<h1>bye</h1>")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.dir, "app.polardev.org", "index.html"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "<h1>bye</h1>" {
		t.Fatalf("content = %q", got)
	}
	// The temp file must not survive — nginx would happily serve it as a
	// dotfile-named sibling, but more importantly it signals a failed rename.
	if _, err := os.Stat(filepath.Join(s.dir, "app.polardev.org", ".index.html.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	hosts, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "app.polardev.org" {
		t.Fatalf("list = %v", hosts)
	}
}

// A host with a directory but no index.html isn't serving anything, so it must
// not be reported as having a custom page.
func TestMaintPageListSkipsEmptyDir(t *testing.T) {
	s := newTestPageStore(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "app.polardev.org"), 0o755); err != nil {
		t.Fatal(err)
	}
	hosts, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("list = %v, want empty", hosts)
	}
}

func TestMaintPageRejectsTraversal(t *testing.T) {
	s := newTestPageStore(t)
	for _, host := range []string{
		"../etc/passwd",
		"a/../../b",
		"foo/bar",
		"UPPER.polardev.org/../x",
		"..",
		"",
		"/absolute",
	} {
		if _, err := s.hostDir(host); err == nil {
			t.Errorf("hostDir(%q) accepted, want error", host)
		}
		if err := s.Write(host, []byte("x")); err == nil {
			t.Errorf("Write(%q) accepted, want error", host)
		}
	}
	// Nothing may have escaped into the parent of the page dir.
	ents, err := os.ReadDir(filepath.Dir(s.dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != filepath.Base(s.dir) {
			t.Fatalf("stray entry beside page dir: %s", e.Name())
		}
	}
}

// Removing is bounded by the .managed marker: a page the operator wrote by
// hand must survive reconciliation forever.
func TestMaintPageRemoveOnlyManaged(t *testing.T) {
	s := newTestPageStore(t)

	handmade := filepath.Join(s.dir, "manual.polardev.org")
	if err := os.MkdirAll(handmade, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handmade, "index.html"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("managed.polardev.org", []byte("theirs")); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("manual.polardev.org"); err != nil {
		t.Fatalf("remove unmanaged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handmade, "index.html")); err != nil {
		t.Fatalf("hand-written page was deleted: %v", err)
	}

	if err := s.Remove("managed.polardev.org"); err != nil {
		t.Fatalf("remove managed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "managed.polardev.org")); !os.IsNotExist(err) {
		t.Fatalf("managed dir survived: %v", err)
	}

	managed, err := s.managedHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 0 {
		t.Fatalf("managedHosts = %v", managed)
	}
}

func TestNewMaintPageFromEnv(t *testing.T) {
	dir := t.TempDir()
	s, msgs := newMaintPageFromEnv(func(string) string { return dir })
	if s == nil {
		t.Fatalf("want store, got nil (%v)", msgs)
	}
	if s.dir != dir {
		t.Fatalf("dir = %q", s.dir)
	}

	// Absent directory must disable the feature rather than create it — a
	// dashboard that reports pages working while writing into its own
	// ephemeral layer is the failure this guards.
	missing := filepath.Join(dir, "nope")
	s, msgs = newMaintPageFromEnv(func(string) string { return missing })
	if s != nil {
		t.Fatalf("want nil store for missing dir")
	}
	if len(msgs) == 0 {
		t.Fatalf("want a warning message")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("directory was created: %v", err)
	}

	// A plain file where the directory should be is also "off", not a panic.
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if s, _ := newMaintPageFromEnv(func(string) string { return file }); s != nil {
		t.Fatalf("want nil store for non-directory")
	}
}

// A nil store is the "not configured" case and every method must survive it.
func TestMaintPageNilStore(t *testing.T) {
	var s *maintPageStore
	if _, err := s.hostDir("a.polardev.org"); err == nil {
		t.Error("hostDir on nil store: want error")
	}
	if err := s.Write("a.polardev.org", nil); err == nil {
		t.Error("Write on nil store: want error")
	}
	if _, err := s.List(); err == nil {
		t.Error("List on nil store: want error")
	}
	if err := s.Sync(context.Background(), nil); err == nil {
		t.Error("Sync on nil store: want error")
	}
}

// ---- Docker archive extraction ----

func tarBytes(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		body := strings.Repeat("x", int(h.Size))
		if h.Typeflag == tar.TypeReg && h.Name == "maintenance.html" {
			body = "<h1>app page</h1>"
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	return buf.Bytes()
}

// dockerStub stands in for the daemon. The real client dials a unix socket and
// ignores the URL host; the stub keeps that shape — a transport that ignores
// the address — but backs it with TCP, since a socket under t.TempDir() blows
// past the ~104-byte sun_path limit on macOS.
func dockerStub(t *testing.T, h http.Handler) *dockerClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return &dockerClient{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			},
		},
	}}
}

func TestCopyFileFromContainer(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.URL.Query().Get("path"); got != "/app/maintenance.html" {
			t.Errorf("path query = %q", got)
		}
		w.Write(tarBytes(t,
			&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755},
			&tar.Header{Name: "maintenance.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		))
	}))
	got, err := dc.copyFileFromContainer(context.Background(), "abc123", "/app/maintenance.html", maxMaintPageBytes)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if string(got) != "<h1>app page</h1>" {
		t.Fatalf("content = %q", got)
	}
}

// An image can hold anything at the labelled path, and this content is served
// to the public — an oversized file is refused, never truncated.
func TestCopyFileFromContainerSizeCap(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(tarBytes(t, &tar.Header{Name: "big.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5000}))
	}))
	if _, err := dc.copyFileFromContainer(context.Background(), "abc", "/big.html", 100); err == nil {
		t.Fatal("want size error")
	}
}

func TestCopyFileFromContainerRejectsRelativePath(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("daemon should not be contacted for a relative path")
	}))
	if _, err := dc.copyFileFromContainer(context.Background(), "abc", "app/x.html", 100); err == nil {
		t.Fatal("want error for relative path")
	}
}

func TestCopyFileFromContainerNoRegularFile(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(tarBytes(t, &tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}))
	}))
	if _, err := dc.copyFileFromContainer(context.Background(), "abc", "/dir", 100); err == nil {
		t.Fatal("want error when archive holds no regular file")
	}
}

// ---- Reconciliation ----

func TestMaintPageSyncReconciles(t *testing.T) {
	labels := []map[string]string{
		{labelEnable: "true", labelHost: "keep.polardev.org", labelMaintPage: "/app/maintenance.html"},
		{labelEnable: "true", labelHost: "nopage.polardev.org"},                                        // no label -> default page
		{labelEnable: "false", labelHost: "off.polardev.org", labelMaintPage: "/app/maintenance.html"}, // not proxied
	}
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/archive") {
			w.Write(tarBytes(t, &tar.Header{Name: "maintenance.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}))
			return
		}
		var cts []dockerContainer
		for i, l := range labels {
			cts = append(cts, dockerContainer{ID: "id" + string(rune('a'+i)), Names: []string{"/c"}, Labels: l})
		}
		json.NewEncoder(w).Encode(cts)
	}))

	s := newTestPageStore(t)
	// A stale managed page from a service that has since lost the label, and a
	// hand-written one that must outlive the sync.
	if err := s.Write("stale.polardev.org", []byte("old")); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(s.dir, "manual.polardev.org")
	os.MkdirAll(manual, 0o755)
	os.WriteFile(filepath.Join(manual, "index.html"), []byte("mine"), 0o644)

	if err := s.Sync(context.Background(), dc); err != nil {
		t.Fatalf("sync: %v", err)
	}

	hosts, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"keep.polardev.org": true, "manual.polardev.org": true}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want keys of %v", hosts, want)
	}
	for _, h := range hosts {
		if !want[h] {
			t.Errorf("unexpected host %q", h)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "stale.polardev.org")); !os.IsNotExist(err) {
		t.Errorf("stale managed page survived: %v", err)
	}
}

// One unreadable image must not stop the rest of the pass.
func TestMaintPageSyncSkipsFailures(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/archive") {
			if strings.Contains(req.URL.Path, "bad") {
				http.Error(w, "no such file", http.StatusNotFound)
				return
			}
			w.Write(tarBytes(t, &tar.Header{Name: "maintenance.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}))
			return
		}
		json.NewEncoder(w).Encode([]dockerContainer{
			{ID: "bad", Names: []string{"/bad"}, Labels: map[string]string{labelEnable: "true", labelHost: "bad.polardev.org", labelMaintPage: "/missing.html"}},
			{ID: "good", Names: []string{"/good"}, Labels: map[string]string{labelEnable: "true", labelHost: "good.polardev.org", labelMaintPage: "/app/maintenance.html"}},
		})
	}))
	s := newTestPageStore(t)
	if err := s.Sync(context.Background(), dc); err != nil {
		t.Fatalf("sync: %v", err)
	}
	hosts, _ := s.List()
	if len(hosts) != 1 || hosts[0] != "good.polardev.org" {
		t.Fatalf("hosts = %v, want [good.polardev.org]", hosts)
	}
}

// A label carrying a host that would escape the page directory must never
// reach the filesystem, even though it arrives from Docker rather than a user.
func TestMaintPageSyncIgnoresBadHostLabel(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/archive") {
			t.Error("archive fetched for an invalid host")
			return
		}
		json.NewEncoder(w).Encode([]dockerContainer{
			{ID: "x", Names: []string{"/x"}, Labels: map[string]string{labelEnable: "true", labelHost: "../../etc/nginx", labelMaintPage: "/app/maintenance.html"}},
		})
	}))
	s := newTestPageStore(t)
	if err := s.Sync(context.Background(), dc); err != nil {
		t.Fatalf("sync: %v", err)
	}
	hosts, _ := s.List()
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want empty", hosts)
	}
}

// The /api/maintenance payload feeds the UI's "custom 503" pill; an
// unconfigured page store must still yield a well-formed empty list.
func TestMaintenanceAPIReportsPages(t *testing.T) {
	s := newTestPageStore(t)
	if err := s.Write("app.polardev.org", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		store *maintPageStore
		want  int
	}{
		{"configured", s, 1},
		{"nil store", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, _ := newConfirmedStore(t, "alice", "correct horse")
			tok, _, err := auth.CreateToken("alice", "test")
			if err != nil {
				t.Fatalf("CreateToken: %v", err)
			}
			mux := http.NewServeMux()
			registerMaintenanceRoutes(mux, nil, auth, nil, tc.store, nil, "")
			req := httptest.NewRequest("GET", "/api/maintenance", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Pages []string `json:"pages"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got.Pages) != tc.want {
				t.Fatalf("pages = %v, want %d", got.Pages, tc.want)
			}
		})
	}
}

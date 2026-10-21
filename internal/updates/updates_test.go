package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0", "0.1.1", -1},
		{"0.2.0", "0.1.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-rc.1", "1.0.0-beta.11", 1},
		{"1.0.0+build.5", "1.0.0", 0},
	} {
		a, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("parse %s: %v", c.a, err)
		}
		b, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("parse %s: %v", c.b, err)
		}
		if got := a.Compare(b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"", "dev", "1.2", "1.2.x", "01.2.3", "1.2.3-", "1.2.3-a..b", "test"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) should fail", bad)
		}
	}
	if Newer("dev", "9.9.9") || Newer("", "0.1.0") {
		t.Error("a build that is not a release never sees an update")
	}
	if !Newer("0.1.0", "v0.1.2") || Newer("0.1.2", "0.1.2") || Newer("0.2.0", "0.1.9") {
		t.Error("Newer")
	}
}

// fakeGitHub serves a release and its manifest, counts the requests and
// remembers the headers of the last one.
type fakeGitHub struct {
	mu        sync.Mutex
	srv       *httptest.Server
	tag       string
	noAsset   bool
	status    int
	releases  int
	manifests int
	notMod    int
	ua        string
	accept    string
	query     string
}

func newFake(t *testing.T, tag string) *fakeGitHub {
	f := &fakeGitHub{tag: tag}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/repos/dbfactory-io/pgcellar/releases/latest":
			f.releases++
			f.ua, f.accept, f.query = r.Header.Get("User-Agent"), r.Header.Get("Accept"), r.URL.RawQuery
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			etag := `"` + f.tag + `"`
			if r.Header.Get("If-None-Match") == etag {
				f.notMod++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			assets := fmt.Sprintf(`[{"name":"checksums.txt","browser_download_url":"%[1]s/dl/checksums.txt"},{"name":"manifest.json","browser_download_url":"%[1]s/dl/%[2]s/manifest.json"}]`, f.srv.URL, f.tag)
			if f.noAsset {
				assets = "[]"
			}
			_, _ = fmt.Fprintf(w, `{"tag_name":"%s","published_at":"2026-10-21T08:00:00Z","html_url":"https://github.com/dbfactory-io/pgcellar/releases/tag/%s","draft":false,"prerelease":false,"assets":%s}`, f.tag, f.tag, assets)
		case "/dl/" + f.tag + "/manifest.json":
			f.manifests++
			v := strings.TrimPrefix(f.tag, "v")
			_, _ = fmt.Fprintf(w, `{"version":"%s","published_at":"2026-10-21T08:00:00Z","image":"ghcr.io/dbfactory-io/pgcellar:%s","image_digest":"sha256:abc","binaries":{"linux_amd64":{"url":"https://example.com/a","sha256":"aa"},"linux_arm64":{"url":"https://example.com/b","sha256":"bb"}},"notes_url":"https://example.com/notes/%s"}`, v, v, v)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) source() string {
	return f.srv.URL + "/repos/dbfactory-io/pgcellar/releases/latest"
}

func TestCheckFindsNewRelease(t *testing.T) {
	f := newFake(t, "v0.1.2")
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	c := New(Options{Version: "0.1.0", Source: f.source(), Now: func() time.Time { return now }})
	if s := c.Latest(); s.Available || s.Current != "0.1.0" || !s.CurrentKnown || !s.CheckedAt.IsZero() {
		t.Fatalf("before any check: %+v", s)
	}
	s, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Available || s.Latest != "0.1.2" || !s.CheckedAt.Equal(now) || s.NotesURL != "https://example.com/notes/0.1.2" || s.PublishedAt.IsZero() {
		t.Fatalf("status: %+v", s)
	}
	if b, ok := s.Manifest.Binary("linux_arm64"); !ok || b.SHA256 != "bb" || s.Manifest.ImageDigest != "sha256:abc" {
		t.Fatalf("manifest: %+v", s.Manifest)
	}
	if f.ua != "pgcellar/0.1.0" || f.accept != "application/vnd.github+json" || f.query != "" {
		t.Errorf("request headers: ua=%q accept=%q query=%q", f.ua, f.accept, f.query)
	}
	if c.Latest().Latest != "0.1.2" {
		t.Error("Latest keeps the result")
	}

	// Same release: If-None-Match gives a 304, the manifest is not fetched
	// again, the findings stay.
	now = now.Add(time.Hour)
	s, err = c.Check(context.Background())
	if err != nil || f.notMod != 1 || f.manifests != 1 || !s.Available || s.Manifest == nil || !s.CheckedAt.Equal(now) {
		t.Fatalf("second check: err=%v notMod=%d manifests=%d %+v", err, f.notMod, f.manifests, s)
	}
}

func TestCheckUpToDateAndDev(t *testing.T) {
	f := newFake(t, "v0.1.0")
	s, err := New(Options{Version: "0.1.0", Source: f.source()}).Check(context.Background())
	if err != nil || s.Available || s.Latest != "0.1.0" {
		t.Fatalf("up to date: %v %+v", err, s)
	}
	s, err = New(Options{Version: "dev", Source: f.source()}).Check(context.Background())
	if err != nil || s.Available || s.CurrentKnown || s.Latest != "0.1.0" {
		t.Fatalf("dev: %v %+v", err, s)
	}
	if f.ua != "pgcellar/dev" {
		t.Errorf("user agent %q", f.ua)
	}
}

func TestCheckWithoutManifest(t *testing.T) {
	f := newFake(t, "v0.2.0")
	f.noAsset = true
	s, err := New(Options{Version: "0.1.0", Source: f.source()}).Check(context.Background())
	if err != nil || !s.Available || s.Manifest != nil || !strings.Contains(s.NotesURL, "/releases/tag/v0.2.0") {
		t.Fatalf("%v %+v", err, s)
	}
}

func TestCheckErrorKeepsFindings(t *testing.T) {
	f := newFake(t, "v0.1.2")
	c := New(Options{Version: "0.1.0", Source: f.source()})
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = http.StatusForbidden
	f.mu.Unlock()
	s, err := c.Check(context.Background())
	if err == nil || !strings.Contains(s.Error, "rate limit") || !s.Available || s.Latest != "0.1.2" {
		t.Fatalf("%v %+v", err, s)
	}
	f.mu.Lock()
	f.status = 0
	f.mu.Unlock()
	if s, err := c.Check(context.Background()); err != nil || s.Error != "" {
		t.Fatalf("recovered: %v %+v", err, s)
	}

	c = New(Options{Version: "0.1.0", Source: "http://127.0.0.1:1/nothing"})
	if s, err := c.Check(context.Background()); err == nil || !strings.Contains(s.Error, "cannot reach 127.0.0.1:1") {
		t.Fatalf("unreachable: %v %+v", err, s)
	}
}

func TestRestoreRecomputesAvailable(t *testing.T) {
	c := New(Options{Version: "0.1.2"})
	c.Restore(Status{Current: "0.1.0", Latest: "0.1.2", Available: true, CheckedAt: time.Now()})
	if s := c.Latest(); s.Available || s.Current != "0.1.2" {
		t.Fatalf("after an update the saved status must not claim one: %+v", s)
	}
	b, _ := json.Marshal(c.Latest())
	if !strings.Contains(string(b), `"current":"0.1.2"`) || strings.Contains(string(b), `"manifest"`) {
		t.Errorf("json: %s", b)
	}
}

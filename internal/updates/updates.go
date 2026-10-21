// Package updates knows whether a newer pgCellar exists. It reads the latest
// release from the GitHub API (or any URL that answers in the same format),
// downloads the release's manifest.json, and compares versions.
//
// It installs nothing: applying an update is the job of the self-update code
// (binary installs) or of the updater on the Docker host.
//
// What leaves the machine: one GET on the release URL and one GET on the
// manifest, with "User-Agent: pgcellar/<version>". No identifier, no counts,
// no configuration, no cookie. The ETag of the last answer is kept in memory
// so that an unchanged release costs a 304.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Timeout bounds one check (release + manifest).
const Timeout = 15 * time.Second

// maxBody bounds what is read from the release API and from the manifest.
const maxBody = 1 << 20

// ManifestAsset is the name of the release asset that describes the build.
const ManifestAsset = "manifest.json"

// Binary is one downloadable binary of a release.
type Binary struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Manifest is manifest.json, attached to every release.
type Manifest struct {
	Version     string            `json:"version"`
	PublishedAt time.Time         `json:"published_at"`
	Image       string            `json:"image"`
	ImageDigest string            `json:"image_digest"`
	Binaries    map[string]Binary `json:"binaries"` // "linux_amd64", "linux_arm64"
	NotesURL    string            `json:"notes_url"`
}

// Binary returns the binary for goos_goarch ("linux_amd64"), if any.
func (m *Manifest) Binary(platform string) (Binary, bool) {
	if m == nil {
		return Binary{}, false
	}
	b, ok := m.Binaries[platform]
	return b, ok && b.URL != "" && b.SHA256 != ""
}

// Status is what is known after the last check.
type Status struct {
	// Current is the running version; CurrentKnown is false for a build that
	// is not a release ("dev"): such a build never reports an update.
	Current      string `json:"current"`
	CurrentKnown bool   `json:"current_known"`
	// Latest is the newest release seen, without the leading "v".
	Latest string `json:"latest,omitempty"`
	// Available: Latest is newer than Current.
	Available   bool      `json:"available"`
	CheckedAt   time.Time `json:"checked_at,omitzero"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	NotesURL    string    `json:"notes_url,omitempty"`
	// Manifest of the latest release; nil when the release has none.
	Manifest *Manifest `json:"manifest,omitempty"`
	// Error of the last attempt, empty when it worked. The fields above keep
	// what the last successful check found.
	Error string `json:"error,omitempty"`
}

// Options builds a Checker.
type Options struct {
	Version string       // the running version (main.Version)
	Source  string       // the release URL (GitHub API "releases/latest" format)
	Client  *http.Client // default: a client with Timeout
	Now     func() time.Time
}

// Checker looks for new releases. It is safe for concurrent use.
type Checker struct {
	version string
	client  *http.Client
	now     func() time.Time

	check sync.Mutex // one check at a time

	mu     sync.RWMutex
	source string
	etag   string
	status Status
}

// New builds a Checker. Nothing is fetched until Check is called.
func New(o Options) *Checker {
	c := &Checker{version: strings.TrimPrefix(strings.TrimSpace(o.Version), "v"), client: o.Client, now: o.Now, source: o.Source}
	if c.client == nil {
		c.client = &http.Client{Timeout: Timeout}
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.status = Status{Current: c.version, CurrentKnown: IsRelease(c.version)}
	return c
}

// Version is the running version the checker compares with.
func (c *Checker) Version() string { return c.version }

// SetSource changes the release URL (the configuration was edited). The
// remembered ETag belongs to the old URL and is dropped.
func (c *Checker) SetSource(src string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if src != c.source {
		c.source, c.etag = src, ""
	}
}

// Latest returns what the last check found. Before any check it holds only
// the current version (or what Restore put back).
func (c *Checker) Latest() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.status
	return s
}

// Restore seeds the status with what was saved before a restart, so that the
// banner does not disappear until the next check. Current is always the
// running version, and Available is computed again against it.
func (c *Checker) Restore(s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.Current = c.version
	s.CurrentKnown = IsRelease(c.version)
	s.Available = Newer(c.version, s.Latest)
	c.status = s
}

// release is the part of the GitHub release object pgCellar reads.
type release struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check fetches the latest release now and returns the new status. On error
// the previous findings are kept, Error says what went wrong, and the error
// is returned too.
func (c *Checker) Check(ctx context.Context) (Status, error) {
	c.check.Lock()
	defer c.check.Unlock()
	c.mu.RLock()
	src, etag := c.source, c.etag
	c.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	rel, newTag, notModified, err := c.fetchRelease(ctx, src, etag)
	var man *Manifest
	if err == nil && !notModified {
		if u := manifestURL(rel); u != "" {
			man, err = c.fetchManifest(ctx, u)
			if err == nil && man.Version != "" && man.Version != strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v") {
				err = fmt.Errorf("the release manifest describes %s but the release is %s", man.Version, rel.TagName)
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Current, s.CurrentKnown = c.version, IsRelease(c.version)
	s.CheckedAt = c.now()
	if err != nil {
		s.Error = err.Error()
		c.status = s
		return s, err
	}
	s.Error = ""
	if !notModified {
		c.etag = newTag
		s.Latest = strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
		s.PublishedAt = rel.PublishedAt
		s.NotesURL = rel.HTMLURL
		s.Manifest = man
		if man != nil {
			if man.NotesURL != "" {
				s.NotesURL = man.NotesURL
			}
			if s.PublishedAt.IsZero() {
				s.PublishedAt = man.PublishedAt
			}
		}
	}
	s.Available = Newer(c.version, s.Latest)
	c.status = s
	return s, nil
}

func (c *Checker) get(ctx context.Context, u, accept, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	ua := "pgcellar/" + c.version
	if c.version == "" {
		ua = "pgcellar/unknown"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", accept)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return c.client.Do(req)
}

func (c *Checker) fetchRelease(ctx context.Context, src, etag string) (release, string, bool, error) {
	var rel release
	if src == "" {
		return rel, "", false, errors.New("no release URL is configured")
	}
	resp, err := c.get(ctx, src, "application/vnd.github+json", etag)
	if err != nil {
		return rel, "", false, fmt.Errorf("cannot reach %s: %w", hostOf(src), unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && etag != "":
		return rel, etag, true, nil
	case resp.StatusCode == http.StatusNotFound:
		return rel, "", false, fmt.Errorf("%s answered 404: no release is published there yet", hostOf(src))
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return rel, "", false, fmt.Errorf("%s refused the request (HTTP %d, probably its rate limit); the next check will try again", hostOf(src), resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return rel, "", false, fmt.Errorf("%s answered HTTP %d", hostOf(src), resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&rel); err != nil {
		return rel, "", false, fmt.Errorf("the answer of %s is not a release: %w", hostOf(src), err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return rel, "", false, fmt.Errorf("the answer of %s has no tag_name", hostOf(src))
	}
	if _, err := ParseVersion(rel.TagName); err != nil {
		return rel, "", false, fmt.Errorf("the latest release is tagged %q, which is not a version", rel.TagName)
	}
	return rel, resp.Header.Get("ETag"), false, nil
}

func manifestURL(rel release) string {
	for _, a := range rel.Assets {
		if a.Name == ManifestAsset && a.URL != "" {
			return a.URL
		}
	}
	return ""
}

func (c *Checker) fetchManifest(ctx context.Context, u string) (*Manifest, error) {
	resp, err := c.get(ctx, u, "application/json, application/octet-stream", "")
	if err != nil {
		return nil, fmt.Errorf("cannot download the release manifest: %w", unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the release manifest answered HTTP %d", resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&m); err != nil {
		return nil, fmt.Errorf("the release manifest cannot be read: %w", err)
	}
	m.Version = strings.TrimPrefix(strings.TrimSpace(m.Version), "v")
	return &m, nil
}

func hostOf(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return u
	}
	return s
}

// unwrapURLError drops the "Get "<url>":" prefix of net/http errors, which
// repeats what the message already says.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

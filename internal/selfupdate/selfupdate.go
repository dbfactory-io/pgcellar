// Package selfupdate installs a new pgCellar binary by itself: download over
// HTTPS, SHA-256 against the release manifest, a `version` run of the new
// binary, then a marker file, two renames and an exec. A binary that does not
// stay up is rolled back at its third start and its version is never tried
// again. It never runs in a container (the image is updated, not the binary).
//
// The package knows nothing about pgcellar.yaml nor about the release
// manifest: the application gives it a Source (the latest release), a Guard
// (is anything running?) and a window function.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Binary is one platform's archive in a release.
type Binary struct {
	URL    string
	SHA256 string
}

// Release is the latest published version, with one archive per platform
// (keys GOOS_GOARCH: linux_amd64, linux_arm64, darwin_arm64).
type Release struct {
	Version  string
	Binaries map[string]Binary
}

// Source tells which release is the latest. ok is false when that is not
// known (no check yet, network error, checks disabled).
type Source interface {
	Latest() (Release, bool)
}

// Guard says whether now is a quiet moment: no backup, restore or drill in
// progress. reason explains a "no" in a sentence.
type Guard interface {
	Ready() (ok bool, reason string)
}

// Status is what the /updates page shows about automatic updates.
type Status struct {
	Enabled        bool            `json:"enabled"`
	Current        string          `json:"current"`
	RefusedReason  string          `json:"refused_reason,omitempty"`
	LastAttempt    *time.Time      `json:"last_attempt,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	FailedVersions []FailedVersion `json:"failed_versions"`
}

// Updater checks every Interval whether a newer release should be installed.
// Set the exported fields before Run; they are not changed afterwards.
type Updater struct {
	// Current is the running version (main.Version).
	Current string
	// Enabled is updates.auto.
	Enabled bool
	Source  Source
	Guard   Guard
	// InWindow reports whether t is inside updates.window; nil = always.
	InWindow func(t time.Time) bool
	// Logf receives one line per decision worth keeping.
	Logf func(format string, args ...any)
	// HealthURL is the local /healthz, polled to confirm a fresh install.
	HealthURL string
	// Confirm is set from Startup: this process is a new version on
	// probation, to be confirmed after SettleDelay of good health.
	Confirm bool

	Interval    time.Duration // default 10 minutes
	SettleDelay time.Duration // default 120 seconds
	Sys         System

	retry time.Duration // between two health probes after the delay; 10 s

	mu     sync.Mutex
	status Status
}

// New returns an Updater on the real machine.
func New(current string, enabled bool, src Source, guard Guard) *Updater {
	return &Updater{Current: current, Enabled: enabled, Source: src, Guard: guard, Sys: DefaultSystem()}
}

func (u *Updater) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

func (u *Updater) interval() time.Duration {
	if u.Interval > 0 {
		return u.Interval
	}
	return 10 * time.Minute
}

func (u *Updater) settleDelay() time.Duration {
	if u.SettleDelay > 0 {
		return u.SettleDelay
	}
	return 120 * time.Second
}

// Status is a copy of the current state, refreshed with the permanent
// refusals (container, version, permissions) and the failed versions file.
func (u *Updater) Status() Status {
	u.mu.Lock()
	s := u.status
	u.mu.Unlock()
	s.Enabled = u.Enabled
	s.Current = u.Current
	exe, err := u.Sys.Executable()
	if reason := u.staticRefusal(exe, err); reason != "" {
		s.RefusedReason = reason
	}
	if err == nil {
		if list, ferr := readFailed(exe); ferr == nil {
			s.FailedVersions = list
		}
	}
	if s.FailedVersions == nil {
		s.FailedVersions = []FailedVersion{}
	}
	if s.LastAttempt != nil {
		t := *s.LastAttempt
		s.LastAttempt = &t
	}
	return s
}

func (u *Updater) setStatus(f func(*Status)) {
	u.mu.Lock()
	f(&u.status)
	u.mu.Unlock()
}

// staticRefusal is the reason automatic updates can never work on this
// installation as it is, or "".
func (u *Updater) staticRefusal(exe string, exeErr error) string {
	switch {
	case u.Sys.InContainer():
		return "pgCellar runs in a container: update the image instead (docker compose pull && docker compose up -d)"
	case normalize(u.Current) == "" || normalize(u.Current) == "dev":
		return "this is a development build (version " + quoteOr(u.Current, "empty") + "), it is never replaced"
	case exeErr != nil:
		return "the path of the running binary is unknown: " + exeErr.Error()
	}
	dir := filepath.Dir(exe)
	if err := u.Sys.Writable(dir); err != nil {
		return fmt.Sprintf("the directory of the binary (%s) is not writable by the service user", dir)
	}
	if err := u.Sys.Writable(exe); err != nil {
		return fmt.Sprintf("the binary (%s) is not writable by the service user", exe)
	}
	return ""
}

func quoteOr(s, empty string) string {
	if s == "" {
		return empty
	}
	return fmt.Sprintf("%q", s)
}

// Run confirms a fresh install when Confirm is set, then, when Enabled,
// checks every Interval until ctx ends.
func (u *Updater) Run(ctx context.Context) {
	if u.Confirm {
		u.confirm(ctx)
	}
	if !u.Enabled {
		u.setStatus(func(s *Status) { s.RefusedReason = "automatic updates are off (updates.auto)" })
		return
	}
	t := time.NewTicker(u.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.Tick(ctx)
		}
	}
}

// Tick decides once whether to install, and does it. When it installs, it
// does not return (the process is replaced) unless something failed.
func (u *Updater) Tick(ctx context.Context) {
	refuse := func(reason string) {
		u.setStatus(func(s *Status) { s.RefusedReason = reason })
	}
	exe, exeErr := u.Sys.Executable()
	if r := u.staticRefusal(exe, exeErr); r != "" {
		refuse(r)
		return
	}
	if m, err := readMarker(exe); err != nil || m != nil {
		refuse("the previous update is not confirmed yet")
		return
	}
	if u.Source == nil {
		refuse("no release information")
		return
	}
	rel, ok := u.Source.Latest()
	if !ok {
		refuse("the latest release is not known yet")
		return
	}
	if !newer(rel.Version, u.Current) {
		refuse("")
		return
	}
	failed, _ := readFailed(exe)
	for _, f := range failed {
		if normalize(f.Version) == normalize(rel.Version) {
			refuse(fmt.Sprintf("version %s failed to start here before; it is not tried again", normalize(rel.Version)))
			return
		}
	}
	bin, ok := rel.Binaries[u.Sys.Platform]
	if !ok || bin.URL == "" {
		refuse(fmt.Sprintf("release %s has no binary for %s", normalize(rel.Version), u.Sys.Platform))
		return
	}
	now := u.Sys.Now()
	if u.InWindow != nil && !u.InWindow(now) {
		refuse(fmt.Sprintf("version %s is waiting for the update window", normalize(rel.Version)))
		return
	}
	if u.Guard != nil {
		if ready, why := u.Guard.Ready(); !ready {
			if why == "" {
				why = "a job is running"
			}
			refuse(fmt.Sprintf("version %s is waiting: %s", normalize(rel.Version), why))
			return
		}
	}

	u.logf("selfupdate: installing %s (running %s)", normalize(rel.Version), normalize(u.Current))
	u.setStatus(func(s *Status) {
		t := now
		s.LastAttempt = &t
		s.RefusedReason = ""
		s.LastError = ""
	})
	err := u.install(ctx, exe, rel, bin)
	if err == nil {
		return
	}
	u.logf("selfupdate: update to %s abandoned: %v", normalize(rel.Version), err)
	u.setStatus(func(s *Status) { s.LastError = err.Error() })
}

// StartupResult tells serve what Startup found.
type StartupResult struct {
	// Confirm: this process is a new version on probation; set
	// Updater.Confirm so Run removes the marker once healthy.
	Confirm bool
	// Note is a line for the log, empty when there was nothing to do.
	Note string
}

// Startup runs first thing in `serve`. With a marker next to the binary:
//   - the running version is the new one: count this start; at the third
//     start roll back (old binary in place, version recorded as failed,
//     exec of the old binary);
//   - the running version is the old one: the service was restarted without
//     the new binary, the marker is stale and removed.
//
// It returns only when this process should go on.
func Startup(sys System, current string, logf func(string, ...any)) StartupResult {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	exe, err := sys.Executable()
	if err != nil {
		return StartupResult{}
	}
	m, err := readMarker(exe)
	if err != nil {
		logf("selfupdate: unreadable marker %s removed: %v", markerPath(exe), err)
		_ = os.Remove(markerPath(exe))
		return StartupResult{Note: "unreadable update marker removed"}
	}
	if m == nil {
		return StartupResult{}
	}
	cur := normalize(current)
	switch cur {
	case m.To:
		m.Attempts++
		if m.Attempts <= 2 {
			if err := writeMarker(exe, *m); err != nil {
				logf("selfupdate: cannot count this start: %v", err)
			}
			note := fmt.Sprintf("started %s after an update from %s (start %d of 3 before a rollback)", m.To, m.From, m.Attempts)
			logf("selfupdate: %s", note)
			return StartupResult{Confirm: true, Note: note}
		}
		return rollback(sys, exe, *m, logf)
	case m.From:
		_ = os.Remove(markerPath(exe))
		note := fmt.Sprintf("the update to %s did not take place (running %s again), marker removed", m.To, m.From)
		logf("selfupdate: %s", note)
		return StartupResult{Note: note}
	default:
		_ = os.Remove(markerPath(exe))
		note := fmt.Sprintf("stale update marker (%s to %s) removed, running %s", m.From, m.To, cur)
		logf("selfupdate: %s", note)
		return StartupResult{Note: note}
	}
}

func rollback(sys System, exe string, m Marker, logf func(string, ...any)) StartupResult {
	prev := exe + prevSuffix
	reason := fmt.Sprintf("did not stay up: %d starts within the probation", m.Attempts)
	if err := addFailed(exe, FailedVersion{Version: m.To, FailedAt: sys.Now().UTC(), Reason: reason}); err != nil {
		logf("selfupdate: cannot record %s as failed: %v", m.To, err)
	}
	if _, err := os.Stat(prev); err != nil {
		_ = os.Remove(markerPath(exe))
		note := fmt.Sprintf("version %s failed to start but %s is missing: no rollback possible, going on", m.To, prev)
		logf("selfupdate: %s", note)
		return StartupResult{Note: note}
	}
	if err := os.Rename(prev, exe); err != nil {
		note := fmt.Sprintf("version %s failed to start and the rollback failed: %v", m.To, err)
		logf("selfupdate: %s", note)
		return StartupResult{Note: note}
	}
	_ = os.Remove(markerPath(exe))
	logf("selfupdate: version %s did not stay up, rolled back to %s", m.To, m.From)
	err := sys.Exec(exe, sys.Args(), sys.Environ())
	note := fmt.Sprintf("rolled back to %s on disk but exec failed: %v; restart the service", m.From, err)
	logf("selfupdate: %s", note)
	return StartupResult{Note: note}
}

// confirm waits SettleDelay, then removes the marker as soon as the local
// /healthz answers 200. The .prev binary is kept for a manual return.
func (u *Updater) confirm(ctx context.Context) {
	exe, err := u.Sys.Executable()
	if err != nil {
		return
	}
	wait := u.settleDelay()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := u.healthy(ctx); err != nil {
			u.logf("selfupdate: not confirmed yet: %v", err)
			wait = u.retry
			if wait <= 0 {
				wait = 10 * time.Second
			}
			continue
		}
		m, _ := readMarker(exe)
		if m == nil {
			return
		}
		if err := os.Remove(markerPath(exe)); err != nil {
			u.logf("selfupdate: cannot remove the marker: %v", err)
			return
		}
		u.logf("selfupdate: version %s confirmed after %s; the previous binary stays at %s", m.To, u.settleDelay(), exe+prevSuffix)
		return
	}
}

func (u *Updater) healthy(ctx context.Context) error {
	if u.HealthURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.HealthURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("healthz answered " + resp.Status)
	}
	return nil
}

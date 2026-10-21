package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/selfupdate"
	"github.com/dbfactory-io/pgcellar/internal/updates"
)

// Alert kinds of the update check (they travel in the webhook payload and the
// alert journal like the kinds of internal/alerts).
const (
	KindUpdateAvailable = "update_available" // a newer release exists, said once per version
	KindUpdateApplied   = "update_applied"   // the running version changed since the last start
)

const (
	settingUpdatesState = "updates.state"
	// updatesTick is how often the loop looks at the clock; the check itself
	// runs every updates.interval.
	updatesTick = time.Minute
	// updatesRetry is the delay before trying again after a failed check.
	updatesRetry = time.Hour
	// UpdaterCommand is what runs on the Docker host to update the container
	// (shown on the Updates page; see docs/manual/16-releases-and-updates.md).
	UpdaterCommand = "sudo pgcellar-updater"
)

// updatesState is persisted in the settings table: what was announced, the
// last version that started, and the last findings (for the banner after a
// restart). All of it can be lost: the next check rebuilds it.
type updatesState struct {
	LastNotified string         `json:"last_notified,omitempty"`
	LastStarted  string         `json:"last_started,omitempty"`
	Status       updates.Status `json:"status"`
}

type updatesHolder struct {
	once    sync.Once
	checker *updates.Checker
	mu      sync.Mutex // the state in the settings table
	self    atomic.Pointer[selfupdate.Updater]
}

// InDocker reports whether pgCellar runs in a container: PGCELLAR_IN_DOCKER=1,
// or the marker file Docker (/.dockerenv) or Podman (/run/.containerenv)
// leaves. Tests replace it.
var InDocker = func() bool {
	if v := strings.TrimSpace(os.Getenv("PGCELLAR_IN_DOCKER")); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// Updates returns the release checker, built on first use with the running
// version and the last findings saved in the database. The self-update code
// reads Updates().Latest() to know what to install.
func (a *App) Updates() *updates.Checker {
	a.upd.once.Do(func() {
		a.upd.checker = updates.New(updates.Options{Version: a.Version, Source: a.Config().Updates.EffectiveSource(), Now: a.now})
		if a.Store != nil {
			if st := a.loadUpdatesState(); !st.Status.CheckedAt.IsZero() {
				a.upd.checker.Restore(st.Status)
			}
		}
	})
	return a.upd.checker
}

func (a *App) loadUpdatesState() updatesState {
	var st updatesState
	if v, ok, err := a.Store.GetSetting(settingUpdatesState); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &st)
	}
	return st
}

func (a *App) saveUpdatesState(st updatesState) {
	b, err := json.Marshal(st)
	if err == nil {
		err = a.Store.SetSetting(settingUpdatesState, string(b))
	}
	if err != nil {
		a.Logf("cannot save the update state: %v", err)
	}
}

// UpdateView is what the Updates page and GET /api/v1/updates show: the
// findings of the last check and how this instance is set up.
type UpdateView struct {
	updates.Status
	Check       bool      `json:"check"`    // updates.check
	Interval    string    `json:"interval"` // updates.interval
	Auto        bool      `json:"auto"`     // updates.auto (applied by the self-update code)
	Window      string    `json:"window"`   // updates.window
	InWindow    bool      `json:"in_window"`
	Docker      bool      `json:"docker"`                    // running in a container: the host updater applies updates
	NextCheckAt time.Time `json:"next_check_at,omitzero"`    // zero when no automatic check runs
	Updater     string    `json:"updater_command,omitempty"` // in a container: the command to run on the host
	// SelfUpdate is the state of automatic updates of a binary installation
	// (internal/selfupdate); absent when its loop is not running.
	SelfUpdate *selfupdate.Status `json:"self_update,omitempty"`
}

// UpdateStatus describes the update check for the pages and the API.
func (a *App) UpdateStatus() UpdateView {
	cfg := a.Config().Updates
	v := UpdateView{
		Status:   a.Updates().Latest(),
		Check:    cfg.IsCheckEnabled(),
		Interval: HumanDuration(cfg.EffectiveInterval()),
		Auto:     cfg.IsAuto(),
		Window:   cfg.ParsedWindow().String(),
		InWindow: cfg.ParsedWindow().Contains(a.now()),
		Docker:   InDocker(),
	}
	if v.Docker {
		v.Updater = UpdaterCommand
	}
	v.SelfUpdate = a.SelfUpdateStatus()
	if a.autoCheck() {
		v.NextCheckAt = a.nextUpdateCheck(v.Status)
	}
	return v
}

// autoCheck: the background check runs only for a release build, outside the
// dev and demo modes, and when updates.check is not false.
func (a *App) autoCheck() bool {
	return !a.Dev && !a.Demo && updates.IsRelease(a.Version) && a.Config().Updates.IsCheckEnabled()
}

func (a *App) nextUpdateCheck(s updates.Status) time.Time {
	if s.CheckedAt.IsZero() {
		return a.now()
	}
	wait := a.Config().Updates.EffectiveInterval()
	if s.Error != "" && wait > updatesRetry {
		wait = updatesRetry
	}
	return s.CheckedAt.Add(wait)
}

// CheckUpdates asks for the latest release now. A check started by a person
// (actor not empty) is audited. A newer version is announced through the
// alert channels once per version.
func (a *App) CheckUpdates(ctx context.Context, actor string) (updates.Status, error) {
	c := a.Updates()
	c.SetSource(a.Config().Updates.EffectiveSource())
	s, err := c.Check(ctx)

	a.upd.mu.Lock()
	st := a.loadUpdatesState()
	st.Status = s
	var msg *alerts.Message
	if err == nil && s.Available && s.Latest != st.LastNotified && !a.Dev && !a.Demo {
		if chans := a.alertChannels(); len(chans) > 0 {
			st.LastNotified = s.Latest
			m := a.updateAvailableMessage(s)
			msg = &m
		}
	}
	a.saveUpdatesState(st) // before sending: at most once per version
	a.upd.mu.Unlock()

	if actor != "" && a.Store != nil {
		details := "latest " + orDash(s.Latest)
		if err != nil {
			details = "failed: " + shorten(err.Error(), 200)
		} else if s.Available {
			details += ", update available"
		}
		_ = a.Store.AddAudit(actor, "updates.check", "", details)
	}
	if msg != nil {
		a.deliver(ctx, a.alertChannels(), "", *msg)
	}
	if err != nil && actor == "" {
		a.Logf("update check failed: %v", err)
	}
	return s, err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *App) updateAvailableMessage(s updates.Status) alerts.Message {
	m := alerts.Message{
		Kind: KindUpdateAvailable, Level: alerts.LevelOK,
		Title: fmt.Sprintf("pgCellar %s is available", s.Latest),
		Lines: []string{fmt.Sprintf("This instance runs %s.", s.Current)},
		URL:   a.alertURL("/updates"), At: a.now(),
	}
	if s.NotesURL != "" {
		m.Lines = append(m.Lines, "What changed: "+s.NotesURL)
	}
	if a.Config().Updates.IsAuto() && !InDocker() {
		m.Lines = append(m.Lines, fmt.Sprintf("It will be installed during the update window (%s).", a.Config().Updates.ParsedWindow()))
	}
	return m
}

// NoteRunningVersion compares the running version with the one that started
// last time and, when it changed, announces it once (update_applied). Called
// at startup; outside the dev and demo modes only.
func (a *App) NoteRunningVersion(ctx context.Context) {
	if a.Store == nil || a.Dev || a.Demo {
		return
	}
	cur := strings.TrimPrefix(strings.TrimSpace(a.Version), "v")
	a.upd.mu.Lock()
	st := a.loadUpdatesState()
	prev := st.LastStarted
	if prev == cur {
		a.upd.mu.Unlock()
		return
	}
	st.LastStarted = cur
	a.saveUpdatesState(st)
	a.upd.mu.Unlock()
	if prev == "" || !updates.IsRelease(prev) || !updates.IsRelease(cur) {
		return // first start, or a development build: nothing worth a message
	}
	a.Logf("version changed from %s to %s", prev, cur)
	_ = a.Store.AddAudit(systemActor, "updates.applied", "", prev+" -> "+cur)
	chans := a.alertChannels()
	if len(chans) == 0 {
		return
	}
	verb := "updated"
	if !updates.Newer(prev, cur) {
		verb = "moved back"
	}
	notes := st.Status.NotesURL
	if st.Status.Latest != cur || notes == "" {
		notes = "https://github.com/dbfactory-io/pgcellar/releases/tag/v" + cur
	}
	a.deliver(ctx, chans, "", alerts.Message{
		Kind: KindUpdateApplied, Level: alerts.LevelOK,
		Title: fmt.Sprintf("pgCellar %s from %s to %s", verb, prev, cur),
		Lines: []string{"What changed: " + notes},
		URL:   a.alertURL("/updates"), At: a.now(),
	})
}

// UpdateReady tells an external updater (the script on the Docker host)
// whether this is a good moment to replace pgCellar: no backup, restore or
// drill in progress, and the clock inside updates.window (unless
// ignoreWindow). The reason says why not.
func (a *App) UpdateReady(now time.Time, ignoreWindow bool) (bool, string) {
	if n := a.runningCount(); n > 0 {
		what := "1 run is"
		if n > 1 {
			what = fmt.Sprintf("%d runs are", n)
		}
		return false, what + " in progress (backup, restore or drill); try again when it ends"
	}
	if w := a.Config().Updates.ParsedWindow(); !ignoreWindow && !w.Contains(now) {
		return false, fmt.Sprintf("outside the update window %s (local time of the pgCellar server, now %s)", w, now.Format("15:04"))
	}
	return true, ""
}

// startUpdates notes the running version, then checks for releases every
// updates.interval. Development builds ("dev") and the dev and demo modes
// never check by themselves; "Check now" still works.
func (a *App) startUpdates(ctx context.Context) {
	if a.Dev || a.Demo {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.NoteRunningVersion(ctx)
		if !updates.IsRelease(a.Version) {
			return
		}
		t := time.NewTicker(updatesTick)
		defer t.Stop()
		for {
			if a.Config().Updates.IsCheckEnabled() && !a.now().Before(a.nextUpdateCheck(a.Updates().Latest())) {
				_, _ = a.CheckUpdates(ctx, "")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

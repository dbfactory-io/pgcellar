package app_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/updates"
)

// releaseServer answers like the GitHub "latest release" endpoint, with the
// tag that the test sets.
type releaseServer struct {
	mu  sync.Mutex
	tag string
	srv *httptest.Server
}

func newReleaseServer(t *testing.T, tag string) *releaseServer {
	t.Helper()
	rs := &releaseServer{tag: tag}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		tag := rs.tag
		rs.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"tag_name":"%s","published_at":"2026-10-21T08:00:00Z","html_url":"https://example.com/notes/%s","assets":[]}`, tag, tag)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *releaseServer) set(tag string) {
	rs.mu.Lock()
	rs.tag = tag
	rs.mu.Unlock()
}

func updatesEnv(t *testing.T, version, tag string) (*apptest.Env, *sink, *releaseServer) {
	t.Helper()
	rs := newReleaseServer(t, tag)
	e, sk := alertEnv(t, func(c *config.Config) {
		c.Updates.Source = config.Ptr(rs.srv.URL + "/releases/latest")
	})
	e.App.Version = version
	return e, sk, rs
}

func kinds(sk *sink) []string {
	var out []string
	for _, m := range sk.take("") {
		out = append(out, m.Kind+": "+m.Title)
	}
	return out
}

func TestUpdateAvailableIsAnnouncedOncePerVersion(t *testing.T) {
	e, sk, rs := updatesEnv(t, "0.1.0", "v0.1.2")
	ctx := context.Background()
	s, err := e.App.CheckUpdates(ctx, "gilles")
	if err != nil || !s.Available || s.Latest != "0.1.2" {
		t.Fatalf("%v %+v", err, s)
	}
	if got := kinds(sk); len(got) != 1 || got[0] != app.KindUpdateAvailable+": pgCellar 0.1.2 is available" {
		t.Fatalf("first check: %v", got)
	}
	if _, err := e.App.CheckUpdates(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("the same version is announced once: %v", got)
	}

	// A restart: the state comes back from the database, still no repeat.
	_ = e.App.Close() // the cleanup closes again, harmlessly
	again, err := app.Load(e.App.ConfigPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	again.Version = "0.1.0"
	if s := again.UpdateStatus(); !s.Available || s.Latest != "0.1.2" {
		t.Fatalf("the findings survive a restart: %+v", s)
	}
	if _, err := again.CheckUpdates(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("no repeat after a restart: %v", got)
	}

	rs.set("v0.1.3")
	if _, err := again.CheckUpdates(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := kinds(sk); len(got) != 1 || !strings.Contains(got[0], "0.1.3") {
		t.Fatalf("a newer version is announced: %v", got)
	}
	audit, _ := again.Store.ListAudit(50)
	found := false
	for _, l := range audit {
		if l.Action == "updates.check" && l.Actor == "gilles" {
			found = true
		}
	}
	if !found {
		t.Error("a check started by a person is audited")
	}
}

func TestUpdateCheckOnDevBuild(t *testing.T) {
	e, sk, _ := updatesEnv(t, "dev", "v0.1.2")
	s, err := e.App.CheckUpdates(context.Background(), "")
	if err != nil || s.Available || s.CurrentKnown || s.Latest != "0.1.2" {
		t.Fatalf("%v %+v", err, s)
	}
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("a dev build announces nothing: %v", got)
	}
	if !e.App.UpdateStatus().NextCheckAt.IsZero() {
		t.Error("a dev build never checks by itself")
	}
}

func TestUpdateAppliedOnVersionChange(t *testing.T) {
	e, sk, _ := updatesEnv(t, "0.1.0", "v0.1.0")
	ctx := context.Background()
	e.App.NoteRunningVersion(ctx) // first start: nothing to say
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("first start: %v", got)
	}
	e.App.NoteRunningVersion(ctx)
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("same version: %v", got)
	}
	e.App.Version = "0.1.2"
	e.App.NoteRunningVersion(ctx)
	got := sk.take("")
	if len(got) != 1 || got[0].Kind != app.KindUpdateApplied || got[0].Title != "pgCellar updated from 0.1.0 to 0.1.2" || !strings.Contains(strings.Join(got[0].Lines, " "), "/releases/tag/v0.1.2") {
		t.Fatalf("update applied: %+v", got)
	}
	e.App.NoteRunningVersion(ctx)
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("said once: %v", got)
	}
	e.App.Version = "dev"
	e.App.NoteRunningVersion(ctx)
	if got := kinds(sk); len(got) != 0 {
		t.Fatalf("a dev build is not news: %v", got)
	}
}

func TestUpdateReady(t *testing.T) {
	e, _, _ := updatesEnv(t, "0.1.0", "v0.1.0")
	in := time.Date(2026, 10, 21, 3, 0, 0, 0, time.Local)
	out := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	if ok, why := e.App.UpdateReady(in, false); !ok {
		t.Fatalf("inside the window, nothing running: %s", why)
	}
	if ok, why := e.App.UpdateReady(out, false); ok || !strings.Contains(why, "outside the update window 02:00-05:00") {
		t.Fatalf("outside the window: %v %s", ok, why)
	}
	if ok, _ := e.App.UpdateReady(out, true); !ok {
		t.Fatal("the window can be ignored")
	}

	t.Setenv("FAKE_BARMAN_BACKUP_DELAY", "1")
	id, err := e.App.StartBackup(context.Background(), "gilles", "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := e.App.UpdateReady(in, true); ok || !strings.Contains(why, "1 run is in progress") {
		t.Fatalf("during a backup: %v %s", ok, why)
	}
	apptest.WaitRun(t, e.App, id)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := e.App.UpdateReady(in, false); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ready again once the backup ended")
}

func TestUpdateStatusInDocker(t *testing.T) {
	e, _, _ := updatesEnv(t, "0.1.0", "v0.1.0")
	old := app.InDocker
	t.Cleanup(func() { app.InDocker = old })
	app.InDocker = func() bool { return true }
	v := e.App.UpdateStatus()
	if !v.Docker || v.Updater == "" || v.Window != "02:00-05:00" || !v.Check || v.Auto {
		t.Fatalf("%+v", v)
	}
	app.InDocker = func() bool { return false }
	if v := e.App.UpdateStatus(); v.Docker || v.Updater != "" {
		t.Fatalf("%+v", v)
	}
	if !updates.IsRelease(v.Current) {
		t.Fatalf("current %q", v.Current)
	}
}

package app

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/selfupdate"
	"github.com/dbfactory-io/pgcellar/internal/updates"
)

func selfUpdateApp(now time.Time) *App {
	cfg := config.Defaults()
	cfg.Updates.Window = config.Ptr("02:00-05:00")
	return &App{cfg: cfg, Version: "0.1.0", Now: func() time.Time { return now }, running: map[string]int64{}}
}

func TestSelfUpdateSource(t *testing.T) {
	a := selfUpdateApp(time.Date(2026, 10, 21, 3, 0, 0, 0, time.Local))
	src := selfUpdateSource{a}
	if _, ok := src.Latest(); ok {
		t.Fatal("a release before any check")
	}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	a.Updates().Restore(updates.Status{
		Current: "0.1.0", CurrentKnown: true, Latest: "0.2.0", Available: true, CheckedAt: a.now(),
		Manifest: &updates.Manifest{Version: "0.2.0", Binaries: map[string]updates.Binary{
			platform: {URL: "https://example.com/p.tar.gz", SHA256: strings.Repeat("a", 64)},
		}},
	})
	rel, ok := src.Latest()
	if !ok || rel.Version != "0.2.0" || rel.Binaries[platform].URL != "https://example.com/p.tar.gz" {
		t.Fatalf("release: %+v %v", rel, ok)
	}

	a.Updates().Restore(updates.Status{Current: "0.1.0", Latest: "0.2.0", Available: true, CheckedAt: a.now(), Error: "timeout"})
	if _, ok := src.Latest(); ok {
		t.Fatal("a release from a failed check")
	}
}

func TestSelfUpdateGuard(t *testing.T) {
	a := selfUpdateApp(time.Date(2026, 10, 21, 3, 0, 0, 0, time.Local))
	g := selfUpdateGuard{a}
	if ok, why := g.Ready(); !ok {
		t.Fatalf("quiet and inside the window: %s", why)
	}
	a.running["pg-main"] = 1
	if ok, why := g.Ready(); ok || !strings.Contains(why, "in progress") {
		t.Fatalf("busy: %v %q", ok, why)
	}
	delete(a.running, "pg-main")
	a.Now = func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local) }
	if ok, why := g.Ready(); ok || !strings.Contains(why, "window") {
		t.Fatalf("outside the window: %v %q", ok, why)
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0:8080":   "http://127.0.0.1:8080/healthz",
		"[::]:8080":      "http://[::1]:8080/healthz",
		"10.0.0.5:9000":  "http://10.0.0.5:9000/healthz",
		"127.0.0.1:8080": "http://127.0.0.1:8080/healthz",
		"nonsense":       "",
	} {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSelfUpdateNotInDevOrDemo(t *testing.T) {
	a := selfUpdateApp(time.Now())
	a.Dev = true
	a.StartSelfUpdate(context.Background(), selfupdate.StartupResult{}, "127.0.0.1:1")
	if a.SelfUpdateStatus() != nil {
		t.Fatal("self-update started in dev mode")
	}
}

func TestSelfUpdateStatusInView(t *testing.T) {
	a := selfUpdateApp(time.Now())
	a.Version = "dev"
	ctx, cancel := context.WithCancel(context.Background())
	a.StartSelfUpdate(ctx, selfupdate.StartupResult{}, "127.0.0.1:1")
	cancel()
	a.wg.Wait()
	st := a.UpdateStatus().SelfUpdate
	if st == nil || st.Enabled || !strings.Contains(st.RefusedReason, "development build") && !strings.Contains(st.RefusedReason, "container") {
		t.Fatalf("self-update status: %+v", st)
	}
}

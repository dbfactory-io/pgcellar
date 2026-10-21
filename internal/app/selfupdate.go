package app

import (
	"context"
	"io"
	"log"
	"net"
	"runtime"

	"github.com/dbfactory-io/pgcellar/internal/selfupdate"
)

// The binary installation updates itself (updates.auto: true): the release
// found by the update check (internal/updates) is downloaded, checked and
// switched to by internal/selfupdate, at a quiet moment inside
// updates.window. In a container nothing happens: the host updater replaces
// the container.

// selfUpdateSource hands the latest release found by the update check to the
// self-update code.
type selfUpdateSource struct{ a *App }

func (s selfUpdateSource) Latest() (selfupdate.Release, bool) {
	st := s.a.Updates().Latest()
	if !st.Available || st.Manifest == nil || st.Error != "" || st.Latest == "" {
		return selfupdate.Release{}, false
	}
	rel := selfupdate.Release{Version: st.Latest, Binaries: map[string]selfupdate.Binary{}}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	if b, ok := st.Manifest.Binary(platform); ok {
		rel.Binaries[platform] = selfupdate.Binary{URL: b.URL, SHA256: b.SHA256}
	}
	return rel, true
}

// selfUpdateGuard: no backup, restore or drill in progress, and the clock
// inside updates.window.
type selfUpdateGuard struct{ a *App }

func (g selfUpdateGuard) Ready() (bool, string) { return g.a.UpdateReady(g.a.now(), false) }

func selfUpdateSystem() selfupdate.System {
	sys := selfupdate.DefaultSystem()
	sys.InContainer = func() bool { return InDocker() }
	return sys
}

// SelfUpdateStartup runs first thing in `serve`, before the configuration is
// read: after an automatic update it counts the starts of the new version and
// rolls back to the previous binary at the third (in which case it does not
// return: the old binary is executed in its place).
func SelfUpdateStartup(version string, logw io.Writer) selfupdate.StartupResult {
	l := log.New(logw, "", log.LstdFlags)
	return selfupdate.Startup(selfUpdateSystem(), version, l.Printf)
}

// StartSelfUpdate starts the self-update loop (every 10 minutes, does
// something only when updates.auto is true) and, when this process is a new
// version on probation, its confirmation once listenAddr answers /healthz.
// Never in the dev or demo modes.
func (a *App) StartSelfUpdate(ctx context.Context, boot selfupdate.StartupResult, listenAddr string) {
	if a.Dev || a.Demo {
		return
	}
	u := &selfupdate.Updater{
		Current:   a.Version,
		IsEnabled: func() bool { return a.Config().Updates.IsAuto() },
		Source:    selfUpdateSource{a},
		Guard:     selfUpdateGuard{a},
		Logf:      a.Logf,
		HealthURL: healthURL(listenAddr),
		Confirm:   boot.Confirm,
		Sys:       selfUpdateSystem(),
	}
	a.upd.self.Store(u)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		u.Run(ctx)
	}()
}

// SelfUpdateStatus is the state of automatic updates for the Updates page
// and the API; nil when the loop is not running (dev, demo, tests).
func (a *App) SelfUpdateStatus() *selfupdate.Status {
	u := a.upd.self.Load()
	if u == nil {
		return nil
	}
	st := u.Status()
	return &st
}

// healthURL is the local address of /healthz for a listening address
// (0.0.0.0 and :: become the loopback).
func healthURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

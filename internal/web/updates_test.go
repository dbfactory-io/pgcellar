package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/selfupdate"
)

func TestUpdatesPageBannerAndMetrics(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"tag_name":"v0.1.2","published_at":"2026-10-21T08:00:00Z","html_url":"https://example.com/notes","assets":[]}`)
	}))
	defer gh.Close()
	env := apptest.NewWith(t, func(c *config.Config) { c.Updates.Source = config.Ptr(gh.URL) })
	env.App.Version = "0.1.0"
	old := app.InDocker
	t.Cleanup(func() { app.InDocker = old })
	app.InDocker = func() bool { return true }
	s, err := New(env.App)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{env: env, srv: s, h: s.Handler()}
	viewer, vcsrf := f.login(t, "viewer")
	op, csrf := f.login(t, "operator")

	rec := f.get("/updates", viewer)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "not checked yet") || strings.Contains(body, `action="/updates/check"`) {
		t.Fatalf("viewer page: %d\n%s", rec.Code, body)
	}
	if rec := f.get("/status", viewer); strings.Contains(rec.Body.String(), "update-note") {
		t.Error("no banner before a check")
	}
	if rec := f.post("/updates/check", viewer, url.Values{"csrf": {vcsrf}}); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer cannot check: %d", rec.Code)
	}
	if rec := f.post("/updates/check", op, url.Values{"csrf": {csrf}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get("/updates?ok=checked", op)
	body = rec.Body.String()
	for _, want := range []string{"pgCellar 0.1.2</strong> is available", "https://example.com/notes", app.UpdaterCommand, `action="/updates/check"`, "Checked just now."} {
		if !strings.Contains(body, want) {
			t.Errorf("operator page lacks %q", want)
		}
	}
	if rec := f.get("/status", viewer); !strings.Contains(rec.Body.String(), "pgCellar 0.1.2 is available.") {
		t.Error("the Status page shows the banner")
	}
	rec = f.get("/metrics", nil)
	if !strings.Contains(rec.Body.String(), "pgcellar_update_available 1") || !strings.Contains(rec.Body.String(), `pgcellar_build_info{version="0.1.0"} 1`) {
		t.Errorf("metrics:\n%s", rec.Body.String())
	}
}

func TestUpdatesPageShowsSelfUpdate(t *testing.T) {
	env := apptest.New(t)
	env.App.Version = "0.1.0"
	old := app.InDocker
	t.Cleanup(func() { app.InDocker = old })
	app.InDocker = func() bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.App.StartSelfUpdate(ctx, selfupdate.StartupResult{}, "127.0.0.1:1")
	s, err := New(env.App)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{env: env, srv: s, h: s.Handler()}
	viewer, _ := f.login(t, "viewer")
	body := f.get("/updates", viewer).Body.String()
	for _, want := range []string{"Automatic updates", "<strong>off</strong>", "runs in a container"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

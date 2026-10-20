package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/report"
)

func newDrillFixture(t *testing.T) *fixture {
	t.Helper()
	env := apptest.NewWith(t, func(c *config.Config) { c.Servers[0].Drills.Schedule = "weekly sun 04:30" })
	s, err := New(env.App)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, srv: s, h: s.Handler()}
}

func TestDrillSectionAndRun(t *testing.T) {
	f := newDrillFixture(t)
	admin, _ := f.login(t, "admin")
	operator, ocsrf := f.login(t, "operator")
	viewer, vcsrf := f.login(t, "viewer")

	page := f.get("/servers/pg-main", admin).Body.String()
	for _, want := range []string{"Restore drills", "No drill yet.", "weekly sun 04:30", "Run a drill now", "Next drill"} {
		if !strings.Contains(page, want) {
			t.Errorf("server page lacks %q", want)
		}
	}
	if strings.Contains(f.get("/servers/pg-main", viewer).Body.String(), "Run a drill now") {
		t.Error("a viewer must not see the drill button")
	}
	if rec := f.post("/servers/pg-main/drill", viewer, url.Values{}, "HX-Request", "true", "X-CSRF-Token", vcsrf); rec.Code != http.StatusForbidden {
		t.Errorf("viewer starts a drill: %d", rec.Code)
	}
	if rec := f.post("/servers/pg-main/drill", operator, url.Values{}, "HX-Request", "true"); rec.Code != http.StatusForbidden {
		t.Errorf("drill without CSRF: %d", rec.Code)
	}

	rec := f.post("/servers/pg-main/drill", operator, url.Values{}, "HX-Request", "true", "X-CSRF-Token", ocsrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "drill of pg-main") || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	m := regexp.MustCompile(`/runs/(\d+)`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no run link: %s", rec.Body.String())
	}
	run, _ := strconv.ParseInt(m[1], 10, 64)
	apptest.WaitRun(t, f.env.App, run)
	d, ok := f.env.App.DrillOfRun(run)
	if !ok {
		t.Fatal("no drill for the run")
	}

	// The finished run shows the verdict and the report link.
	body := f.get("/runs/"+strconv.FormatInt(run, 10), operator).Body.String()
	for _, want := range []string{"Restore verified", "was restored and started successfully", "/drills/" + strconv.FormatInt(d.ID, 10) + "/report", "== Start PostgreSQL"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %q", want)
		}
	}

	// The server page: the last result in big, the history, the status line.
	page = f.get("/servers/pg-main", admin).Body.String()
	for _, want := range []string{"✨", "Restore verified", "2 databases, 14 tables", "Report (PDF)", "verified"} {
		if !strings.Contains(page, want) {
			t.Errorf("server page after a drill lacks %q", want)
		}
	}
	status := f.get("/status", admin).Body.String()
	if !strings.Contains(status, "Last drill: just now ✔") {
		t.Errorf("status page lacks the drill line:\n%s", status)
	}
	if strings.Contains(f.get("/servers/pg-main", viewer).Body.String(), "/report") {
		t.Error("a viewer must not get the report link")
	}

	// The report download: operators and admins, a valid PDF.
	if rec := f.get("/drills/"+strconv.FormatInt(d.ID, 10)+"/report", viewer); rec.Code != http.StatusForbidden {
		t.Errorf("viewer report: %d", rec.Code)
	}
	rec = f.get("/drills/"+strconv.FormatInt(d.ID, 10)+"/report", operator)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/pdf" || !strings.Contains(rec.Header().Get("Content-Disposition"), "drill-") {
		t.Fatalf("report: %d %v", rec.Code, rec.Header())
	}
	if _, err := report.Check(rec.Body.Bytes()); err != nil {
		t.Error(err)
	}
	if rec := f.get("/drills/9999/report", admin); rec.Code != http.StatusNotFound {
		t.Errorf("unknown drill: %d", rec.Code)
	}
}

func TestDrillFailureShownDry(t *testing.T) {
	f := newDrillFixture(t)
	admin, _ := f.login(t, "admin")
	t.Setenv("FAKE_PGCTL_FAIL", "1")
	id, err := f.env.App.StartDrill(context.Background(), "admin-user", "pg-main", app.DrillInput{})
	if err != nil {
		t.Fatal(err)
	}
	apptest.WaitRun(t, f.env.App, id)
	page := f.get("/servers/pg-main", admin).Body.String()
	for _, want := range []string{"Restore drill failed", "FAILED at the step &#34;Start PostgreSQL&#34;", "pill bad"} {
		if !strings.Contains(page, want) {
			t.Errorf("failed drill: page lacks %q", want)
		}
	}
	if strings.Contains(page, "✨") {
		t.Error("no sparkle for a failed drill")
	}
	status := f.get("/status", admin).Body.String()
	if !strings.Contains(status, "Last drill: just now ✘") || !strings.Contains(status, "The last restore drill failed") || !strings.Contains(status, "lv-warn") {
		t.Errorf("status page of a failed drill:\n%s", status)
	}
}

func TestDrillMetrics(t *testing.T) {
	f := newDrillFixture(t)
	body := f.get("/metrics", nil).Body.String()
	if strings.Contains(body, `pgcellar_last_drill_ok{`) {
		t.Error("no drill yet: no sample")
	}
	if !strings.Contains(body, "# TYPE pgcellar_last_drill_ok gauge") || !strings.Contains(body, "# TYPE pgcellar_last_drill_age_seconds gauge") {
		t.Error("families missing")
	}
	id, err := f.env.App.StartDrill(context.Background(), "admin-user", "pg-main", app.DrillInput{})
	if err != nil {
		t.Fatal(err)
	}
	apptest.WaitRun(t, f.env.App, id)
	body = f.get("/metrics", nil).Body.String()
	if !strings.Contains(body, `pgcellar_last_drill_ok{server="pg-main"} 1`) || !strings.Contains(body, `pgcellar_last_drill_age_seconds{server="pg-main"} 0`) {
		t.Errorf("metrics after a drill:\n%s", body)
	}
	t.Setenv("FAKE_PGCTL_FAIL", "1")
	id, _ = f.env.App.StartDrill(context.Background(), "admin-user", "pg-main", app.DrillInput{})
	apptest.WaitRun(t, f.env.App, id)
	if body = f.get("/metrics", nil).Body.String(); !strings.Contains(body, `pgcellar_last_drill_ok{server="pg-main"} 0`) {
		t.Errorf("a failed drill must give 0:\n%s", body)
	}
}

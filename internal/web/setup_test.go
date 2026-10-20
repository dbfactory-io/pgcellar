package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestServerSetupSection(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	body := f.get("/servers/pg-main", admin).Body.String()
	for _, want := range []string{
		`id="setup"`,
		"Server setup",
		"What pgCellar found on the PostgreSQL server, and what to change there. Nothing here is changed by pgCellar.",
		"archive_timeout = <code>0 s</code>",
		"A quiet database never fills a WAL file; set archive_timeout = &#39;1h&#39; for a bounded recovery point.",
		"ALTER SYSTEM SET archive_timeout = &#39;1h&#39;;",
		"SELECT pg_reload_conf();",
		"reload is enough",
		"settings fine",
		"summarize_wal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("server page lacks %q", want)
		}
	}
	// The warning comes first, the fine settings are folded.
	if i, j := strings.Index(body, "archive_timeout = <code>"), regexp.MustCompile(`<summary><span aria-hidden="true">✅</span> \d+ settings fine`).FindStringIndex(body); i < 0 || j == nil || i > j[0] {
		t.Errorf("order: warning at %d, folded oks at %v", i, j)
	}
	// Overview is above the setup, backups below.
	if o, s, b := strings.Index(body, "Overview"), strings.Index(body, `id="setup"`), strings.Index(body, "Backups</h2>"); o >= s || s >= b {
		t.Errorf("section order: %d %d %d", o, s, b)
	}
}

func TestSetupPillsLinkToTheSection(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	for _, path := range []string{"/status", "/servers"} {
		body := f.get(path, admin).Body.String()
		if !strings.Contains(body, `<a class="pill warn" href="/servers/pg-main#setup"><span aria-hidden="true">🔧</span> 1 to review</a>`) {
			t.Errorf("%s lacks the setup pill:\n%s", path, body)
		}
	}
	// The pill never sits inside the link of the card (no nested anchors).
	body := f.get("/status", admin).Body.String()
	i := strings.Index(body, `href="/servers/pg-main#setup"`)
	if j := strings.LastIndex(body[:i], "<a "); strings.Contains(body[j:i], "</a>") == false && !strings.HasPrefix(body[j:], `<a class="pill`) {
		t.Errorf("nested anchor around the pill")
	}
}

func TestSetupMetricAndNoCommandOnPageViews(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	before, _ := os.ReadFile(f.env.PsqlLog)
	f.get("/servers", admin)
	f.get("/servers/pg-main", admin)
	f.get("/status", admin)
	if after, _ := os.ReadFile(f.env.PsqlLog); string(after) != string(before) {
		t.Error("a page view ran psql")
	}
	m := f.get("/metrics", nil).Body.String()
	if !strings.Contains(m, "# TYPE pgcellar_setup_level gauge") || !strings.Contains(m, `pgcellar_setup_level{server="pg-main"} 1`) {
		t.Errorf("metrics lacks pgcellar_setup_level:\n%s", m)
	}
	if strings.Contains(m, `pgcellar_setup_level{server="broken"}`) {
		t.Error("a server never read has no setup level")
	}
}

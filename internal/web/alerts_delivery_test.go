package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

// refused writes n refused Telegram sends into the journal, one an hour from
// 21 Oct 01:00.
func refused(t *testing.T, f *fixture, n int) {
	t.Helper()
	start := time.Date(2026, 10, 21, 1, 0, 0, 0, time.Local)
	for i := range n {
		if err := f.env.App.Store.AddAlert(store.AlertSent{At: start.Add(time.Duration(i) * time.Hour), Channel: alerts.Telegram, Kind: alerts.KindBackup, Level: alerts.LevelFail,
			Text: "❌ Backup of main failed", Error: "the Telegram API answered 404: Not Found"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAlertsNotGettingThroughBanner(t *testing.T) {
	t.Setenv("PGC_BANNER_TG", "123:dead")
	f := fixtureWith(t, func(c *config.Config) {
		c.Alerts.Telegram = &config.TelegramAlert{TokenEnv: "PGC_BANNER_TG", ChatID: "-1001"}
	})
	admin, _ := f.login(t, "admin")
	viewer, _ := f.login(t, "viewer")
	const want = "⚠️ Your alerts are not getting through: Telegram answered 404 Not Found since 21 Oct 01:00 (12 messages lost)"

	if body := f.get("/status", admin).Body.String(); strings.Contains(body, "not getting through") || strings.Contains(body, "No alert channel configured") {
		t.Fatalf("banner with an empty journal:\n%s", body)
	}
	refused(t, f, 12)

	body := f.get("/status", admin).Body.String()
	for _, s := range []string{want, `class="flash bad alerts-banner"`, `hx-post="/alerts/test"`, "Send a test", `href="/alerts"`} {
		if !strings.Contains(body, s) {
			t.Errorf("admin status lacks %q", s)
		}
	}
	// The part that refreshes every 30 s carries it too.
	if frag := f.get("/status/table", admin, "HX-Request", "true").Body.String(); !strings.Contains(frag, want) {
		t.Errorf("fragment lacks the banner:\n%s", frag)
	}
	// A viewer sees it, without the button.
	vb := f.get("/status", viewer).Body.String()
	if !strings.Contains(vb, want) || strings.Contains(vb, `hx-post="/alerts/test"`) {
		t.Errorf("viewer status:\n%s", vb)
	}
	// Elsewhere, a discreet line for administrators only.
	if b := f.get("/servers", admin).Body.String(); !strings.Contains(b, `class="alerts-down"`) {
		t.Errorf("admin layout lacks the reminder")
	}
	if b := f.get("/servers", viewer).Body.String(); strings.Contains(b, `class="alerts-down"`) {
		t.Errorf("viewer layout shows the reminder")
	}
	// The Alerts page puts the state first.
	if b := f.get("/alerts", admin).Body.String(); !strings.Contains(b, "❌ failing since") || !strings.Contains(b, "12 messages lost") || !strings.Contains(b, "never delivered a message") {
		t.Errorf("alerts page lacks the delivery state:\n%s", b)
	}
	// Metrics, and /healthz stays 200 whatever happens.
	m := f.get("/metrics", nil).Body.String()
	for _, s := range []string{`pgcellar_alert_channel_failing{channel="telegram"} 1`, `pgcellar_alert_channel_consecutive_failures{channel="telegram"} 12`} {
		if !strings.Contains(m, s) {
			t.Errorf("metrics lack %q", s)
		}
	}
	if rec := f.get("/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}

	// One message through: gone everywhere.
	if err := f.env.App.Store.AddAlert(store.AlertSent{At: apptest.Frozen, Channel: alerts.Telegram, Kind: alerts.KindTest, Level: alerts.LevelOK, Text: "✅ Test message from pgCellar", OK: true}); err != nil {
		t.Fatal(err)
	}
	if b := f.get("/status", admin).Body.String(); strings.Contains(b, "not getting through") {
		t.Errorf("banner after a success")
	}
	if b := f.get("/servers", admin).Body.String(); strings.Contains(b, `class="alerts-down"`) {
		t.Errorf("reminder after a success")
	}
	if b := f.get("/alerts", admin).Body.String(); !strings.Contains(b, "✅ delivering") {
		t.Errorf("alerts page after a success lacks the green pill")
	}
	if m := f.get("/metrics", nil).Body.String(); !strings.Contains(m, `pgcellar_alert_channel_failing{channel="telegram"} 0`) {
		t.Errorf("metric after a success:\n%s", m)
	}
}

func TestNoAlertChannelNoteOncePerSession(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	rec := f.get("/status", admin)
	body := rec.Body.String()
	if !strings.Contains(body, "No alert channel configured") || strings.Contains(body, "flash bad alerts-banner") {
		t.Fatalf("first visit:\n%s", body)
	}
	var seen *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == noChannelCookie {
			seen = c
		}
	}
	if seen == nil || seen.MaxAge != 0 || !seen.Expires.IsZero() {
		t.Fatalf("the note must be remembered for the browser session only: %+v", seen)
	}
	req, _ := http.NewRequest("GET", "/status", nil)
	req.AddCookie(admin)
	req.AddCookie(seen)
	if b := f.do(req).Body.String(); strings.Contains(b, "No alert channel configured") {
		t.Errorf("the note came back in the same session")
	}
}

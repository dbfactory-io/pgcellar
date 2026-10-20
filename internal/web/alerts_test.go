package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
)

func fixtureWith(t *testing.T, mutate func(*config.Config)) *fixture {
	t.Helper()
	env := apptest.NewWith(t, mutate)
	s, err := New(env.App)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, srv: s, h: s.Handler()}
}

func TestAlertsPageAndTestButton(t *testing.T) {
	hits := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer hook.Close()
	t.Setenv("PGC_TEST_TG", "123:topsecret")
	f := fixtureWith(t, func(c *config.Config) {
		c.Alerts.Webhook = &config.WebhookAlert{URL: hook.URL, SecretEnv: "PGC_TEST_HOOK_SECRET"}
		c.Alerts.Telegram = &config.TelegramAlert{TokenEnv: "PGC_TEST_TG", ChatID: "-1001"}
	})

	// only the administrator
	opCookie, _ := f.login(t, "operator")
	if rec := f.get("/alerts", opCookie); rec.Code != http.StatusForbidden && rec.Code != http.StatusSeeOther {
		t.Errorf("operator: %d", rec.Code)
	}
	cookie, csrf := f.login(t, "admin")
	rec := f.get("/alerts", cookie)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("alerts: %d", rec.Code)
	}
	for _, want := range []string{"telegram", "webhook", "PGC_TEST_TG", "is set", "PGC_TEST_HOOK_SECRET", "is missing", "Send a test message"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "topsecret") {
		t.Fatal("a secret must never appear on the page")
	}

	// the webhook secret is not set: the test fails and says why
	rec = f.post("/alerts/test", cookie, url.Values{"channel": {"webhook"}}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "could not be sent") || !strings.Contains(rec.Body.String(), "PGC_TEST_HOOK_SECRET") {
		t.Fatalf("failed test: %d %s", rec.Code, rec.Body.String())
	}
	t.Setenv("PGC_TEST_HOOK_SECRET", "s3")
	rec = f.post("/alerts/test", cookie, url.Values{"channel": {"webhook"}}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Test message sent") || hits != 1 {
		t.Fatalf("test: %d %s hits=%d", rec.Code, rec.Body.String(), hits)
	}
	// journal on the page, audited, counted
	body = f.get("/alerts", cookie).Body.String()
	if !strings.Contains(body, "Test message from pgCellar") || !strings.Contains(body, "failed") {
		t.Errorf("journal lacks the sends:\n%s", body)
	}
	if a, _ := f.env.App.Audit(5); len(a) == 0 || a[0].Action != "alerts.test" {
		t.Errorf("audit: %+v", a)
	}
	m := f.get("/metrics", nil).Body.String()
	if !strings.Contains(m, `pgcellar_alerts_sent_total{channel="webhook",ok="true"} 1`) || !strings.Contains(m, `pgcellar_alerts_sent_total{channel="webhook",ok="false"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}
	if !strings.Contains(m, "pgcellar_license_servers_max 0") {
		t.Errorf("licence metric (test licence is unlimited):\n%s", m)
	}
	// unknown channel
	rec = f.post("/alerts/test", cookie, url.Values{"channel": {"carrier-pigeon"}}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 404 {
		t.Errorf("unknown channel: %d", rec.Code)
	}
}

func TestLicenseFooterAndBanner(t *testing.T) {
	f := newFixture(t) // test licence: Business, unlimited
	cookie, _ := f.login(t, "viewer")
	body := f.get("/status", cookie).Body.String()
	if !strings.Contains(body, "Business edition · Test · 4 servers") {
		t.Errorf("footer:\n%s", body[len(body)-600:])
	}
	if strings.Contains(body, "licence-note") {
		t.Error("no banner for a valid licence")
	}

	// Free edition with 3 servers in barman.d: over the quota
	f = fixtureWith(t, func(c *config.Config) { c.License.Key = "" })
	cookie, _ = f.login(t, "viewer")
	body = f.get("/status", cookie).Body.String()
	if !strings.Contains(body, "Free edition · 4 of 2 servers") || !strings.Contains(body, "licence-note") || !strings.Contains(body, "dbfactory.io/pgcellar") {
		t.Errorf("over quota:\n%s", body)
	}
	if m := f.get("/metrics", nil).Body.String(); !strings.Contains(m, "pgcellar_license_servers_max 2") {
		t.Errorf("metrics:\n%s", m)
	}
}

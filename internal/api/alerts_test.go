package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

func TestAlertsAndLicenseAPI(t *testing.T) {
	hits := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer hook.Close()
	env := apptest.NewWith(t, func(c *config.Config) {
		c.Alerts.Webhook = &config.WebhookAlert{URL: hook.URL, SecretEnv: "PGC_API_HOOK"}
	})
	f := &fixture{env: env, h: New(env.App).Handler()}

	if code, _ := f.call(t, "GET", "/alerts", f.env.Keys["operator"], ""); code != 403 {
		t.Errorf("operator: %d", code)
	}
	if code, _ := f.call(t, "POST", "/alerts/test", f.env.Keys["operator"], ""); code != 403 {
		t.Errorf("operator test: %d", code)
	}
	code, out := f.call(t, "GET", "/alerts", f.env.Keys["admin"], "")
	data, _ := out["data"].(map[string]any)
	chans, _ := data["channels"].([]any)
	if code != 200 || len(chans) != 1 || data["sent"] == nil {
		t.Fatalf("alerts: %d %v", code, out)
	}
	ch := chans[0].(map[string]any)
	if ch["name"] != "webhook" || ch["ready"] != false {
		t.Errorf("channel: %v", ch)
	}

	// secret variable missing: a 200 with ok=false for the channel
	code, out = f.call(t, "POST", "/alerts/test", f.env.Keys["admin"], `{"channel":"webhook"}`)
	res := out["data"].([]any)
	if code != 200 || len(res) != 1 || res[0].(map[string]any)["ok"] != false {
		t.Fatalf("test without secret: %d %v", code, out)
	}
	t.Setenv("PGC_API_HOOK", "x")
	code, out = f.call(t, "POST", "/alerts/test", f.env.Keys["admin"], "")
	res = out["data"].([]any)
	if code != 200 || len(res) != 1 || res[0].(map[string]any)["ok"] != true || hits != 1 {
		t.Fatalf("test: %d %v hits=%d", code, out, hits)
	}
	if code, _ := f.call(t, "POST", "/alerts/test", f.env.Keys["admin"], `{"channel":"mail"}`); code != 404 {
		t.Errorf("unknown channel: %d", code)
	}

	code, out = f.call(t, "GET", "/license", f.env.Keys["viewer"], "")
	lic := out["data"].(map[string]any)
	if code != 200 || lic["edition"] != "business" || lic["state"] != "valid" || lic["licensee"] != "Test" || lic["servers_max"] != float64(0) || lic["servers_used"] != float64(4) {
		t.Errorf("license: %d %v", code, lic)
	}
	if code, _ := f.call(t, "GET", "/license", "", ""); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
}

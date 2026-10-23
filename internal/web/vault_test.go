package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/vault/s3test"
)

// newVaultFixture is a fixture with the vault on, aimed at a fake S3.
func newVaultFixture(t *testing.T, on bool) (*fixture, *s3test.Server) {
	t.Helper()
	s3 := s3test.New()
	t.Cleanup(s3.Close)
	env := apptest.NewWith(t, func(c *config.Config) {
		c.Vault = config.Vault{Enabled: on, Endpoint: s3.URL, Region: s3.Region, Bucket: s3.Bucket, AccessKey: s3.AccessKey}
	})
	cfg := env.App.Config()
	p := cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(s3.SecretKey), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(env.App)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, srv: s, h: s.Handler()}, s3
}

func TestVaultPages(t *testing.T) {
	f, s3 := newVaultFixture(t, true)
	a := f.env.App
	ctx := context.Background()
	if _, err := a.VaultCopyNow(ctx, "admin-user", "pg-main"); err != nil {
		t.Fatal(err)
	}
	a.VaultTick(ctx, a.Now())

	viewer, _ := f.login(t, "viewer")
	rec := f.get("/vault", viewer)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Where the copies go") || !strings.Contains(body, s3.Bucket) || !strings.Contains(body, "Off-site copy: ") {
		t.Fatalf("vault page for a viewer: %d\n%s", rec.Code, body)
	}
	if strings.Contains(body, `href="/vault/recovery-key"`) || strings.Contains(body, "Test connection</button>") {
		t.Error("a viewer sees no key download and no test button")
	}
	if strings.Contains(body, s3.SecretKey) {
		t.Fatal("the secret key reached a page")
	}
	if rec := f.get("/vault/recovery-key", viewer); rec.Code != http.StatusForbidden {
		t.Errorf("recovery key for a viewer: %d", rec.Code)
	}

	// Status page: the vault line and the server line.
	st := f.get("/status", viewer).Body.String()
	if !strings.Contains(st, "🔐 Off-site copy: 1 of 1 servers up to date") || !strings.Contains(st, "Off-site copy: ") {
		t.Errorf("status page lacks the vault:\n%s", st)
	}
	if list := f.get("/servers", viewer).Body.String(); !strings.Contains(list, "✅") || !strings.Contains(list, "Off-site copy") {
		t.Errorf("servers list lacks the vault column")
	}
	page := f.get("/servers/pg-main", viewer).Body.String()
	if !strings.Contains(page, `id="vault"`) || !strings.Contains(page, "Last good copy") || strings.Contains(page, "/vault-copy") {
		t.Errorf("server card for a viewer:\n%s", page)
	}
	if m := f.get("/metrics", nil).Body.String(); !strings.Contains(m, `pgcellar_vault_last_copy_ok{server="pg-main"} 1`) || !strings.Contains(m, `pgcellar_vault_last_copy_age_seconds{server="pg-main"}`) || !strings.Contains(m, `pgcellar_vault_bytes{server="pg-main"} 0`) {
		t.Errorf("metrics:\n%s", m)
	}

	// An operator copies now.
	op, opCSRF := f.login(t, "operator")
	if page := f.get("/servers/pg-main", op).Body.String(); !strings.Contains(page, "/servers/pg-main/vault-copy") || !strings.Contains(page, "Drill from the off-site copy") || !strings.Contains(page, `hx-vals='{"source": "vault"}'`) {
		t.Error("an operator gets the Copy now and the drill from the vault buttons")
	}
	rec = f.post("/servers/pg-main/vault-copy", op, url.Values{"csrf": {opCSRF}}, "HX-Request", "true", "X-CSRF-Token", opCSRF)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "Copy of pg-main queued (position 1)") {
		t.Errorf("copy now: %d %s", rec.Code, rec.Body.String())
	}

	// The admin downloads the key.
	admin, _ := f.login(t, "admin")
	rec = f.get("/vault/recovery-key", admin)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Disposition"), "pgcellar-recovery-key-") || !strings.Contains(rec.Body.String(), "restic password") {
		t.Errorf("recovery key: %d %v", rec.Code, rec.Header())
	}
	if body := f.get("/vault", admin).Body.String(); !strings.Contains(body, "Downloaded") || !strings.Contains(body, "admin-user") {
		t.Error("the download is shown")
	}
}

func TestVaultSettingsForm(t *testing.T) {
	f, s3 := newVaultFixture(t, false)
	admin, csrf := f.login(t, "admin")
	rec := f.get("/vault", admin)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "The off-site copy is off.") {
		t.Fatalf("vault off: %d", rec.Code)
	}
	if strings.Contains(f.get("/servers/new", admin).Body.String(), `name="vault"`) {
		t.Error("no vault box while the vault is off")
	}
	form := url.Values{"csrf": {csrf}, "enabled": {"1"}, "endpoint": {s3.URL}, "region": {s3.Region}, "bucket": {s3.Bucket}, "prefix": {"pgcellar"},
		"access_key": {s3.AccessKey}, "secret_key": {""}, "quota": {"500GB"}, "wal_interval": {"15m"}, "max_age": {"36h"},
		"keep_last": {"0"}, "keep_daily": {"7"}, "keep_weekly": {"4"}, "keep_monthly": {"0"}, "drill_source": {"local"}}

	// Test first, with the values of the form.
	s3.SetLock(14)
	rec = f.post("/vault/test", admin, form)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "The bucket works") || !strings.Contains(rec.Body.String(), "Locked for 14 days (governance)") {
		t.Fatalf("test: %d\n%s", rec.Code, rec.Body.String())
	}
	form.Set("wal_interval", "soon")
	if rec := f.post("/vault", admin, form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is not a duration") {
		t.Errorf("bad duration: %d", rec.Code)
	}
	form.Set("wal_interval", "15m")
	rec = f.post("/vault", admin, form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/vault?ok=saved" {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	cfg := f.env.App.Config()
	if !cfg.Vault.Enabled || cfg.Vault.Quota != "500GB" || cfg.Vault.WALInterval != nil || cfg.Vault.Prefix != nil {
		t.Errorf("saved: %+v", cfg.Vault)
	}
	if rec := f.get("/vault?ok=saved", admin); !strings.Contains(rec.Body.String(), "Vault settings saved") {
		t.Error("saved message")
	}

	// The server form now has the box, ticked for a new server.
	nf := f.get("/servers/new", admin).Body.String()
	if !strings.Contains(nf, `name="vault" value="1" checked`) {
		t.Errorf("new server form:\n%s", nf)
	}
	// Unticking it on pg-main writes vault: false.
	page := f.get("/servers/pg-main", admin).Body.String()
	if !strings.Contains(page, `name="vault" value="1" checked`) {
		t.Fatal("pg-main is copied by default")
	}
	in := url.Values{"csrf": {csrf}, "label": {"Main"}, "backup_method": {"postgres"}, "wal_method": {"streaming"}, "conninfo": {"host=db1 user=barman dbname=postgres"},
		"streaming_conninfo": {"host=db1 user=streaming_barman dbname=postgres"}, "slot_name": {"barman"}, "schedule_backup": {"daily 02:00"}, "schedule_enabled": {"1"},
		"recovery_window_days": {"7"}, "minimum_redundancy": {"1"}, "vault_field": {"1"}}
	if rec := f.post("/servers/pg-main", admin, in); rec.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	cs := f.env.App.Config().Servers[0]
	if cs.Vault == nil || *cs.Vault {
		t.Errorf("vault: false not written: %+v", cs)
	}
	raw, _ := os.ReadFile(filepath.Join(f.env.Dir, "pgcellar.yaml"))
	if !strings.Contains(string(raw), "vault: false") {
		t.Errorf("yaml:\n%s", raw)
	}
}

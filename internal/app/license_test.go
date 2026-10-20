package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/license"
)

func newServerInput(name string) app.ServerInput {
	return app.ServerInput{Name: name, BackupMethod: "postgres", ConnInfo: "host=db.example.com user=barman dbname=postgres", StreamingConnInfo: "host=db.example.com user=streaming_barman", Slot: "barman_" + strings.ReplaceAll(name, "-", "_")}
}

// The test environment holds 4 servers (barman.d, pgcellar.yaml, the fake
// Barman's list).
func TestLicenseQuotaOnlyLimitsAdding(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.License.Key = "" })
	a := e.App
	li := a.LicenseInfo()
	if li.State != license.StateFree || li.Max != 2 || li.Used != 4 || li.Footer != "Free edition · 4 of 2 servers" || li.Banner == "" {
		t.Fatalf("free: %+v", li)
	}
	_, err := a.CreateServer("tester", newServerInput("pg-new"))
	var conflict *app.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Msg, "dbfactory.io/pgcellar") || !strings.Contains(conflict.Msg, "keep working") {
		t.Fatalf("adding beyond the quota must be refused with a clear message: %v", err)
	}
	// ...but nothing that already exists is touched
	id, err := a.StartBackup(context.Background(), "tester", "pg-main")
	if err != nil {
		t.Fatalf("a backup is never blocked by the licence: %v", err)
	}
	if r := apptest.WaitRun(t, a, id); r.Status != app.RunOK {
		t.Errorf("backup: %+v", r)
	}
	if _, err := a.StartDrill(context.Background(), "tester", "pg-main", app.DrillInput{}); err != nil {
		t.Errorf("a drill is never blocked by the licence: %v", err)
	}
}

func TestLicenseKeyRaisesTheQuota(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.License.Key = "" })
	pro := e.LicenseKey(license.Pro, 5, "2027-12-31")
	if err := writeLicense(e, pro); err != nil {
		t.Fatal(err)
	}
	a := e.App
	li := a.LicenseInfo()
	if li.State != license.StateValid || li.Max != 5 || li.Licensee != "Test" || li.Footer != "Pro edition · Test · 4 of 5 servers" || li.Banner != "" {
		t.Fatalf("pro: %+v", li)
	}
	if _, err := a.CreateServer("tester", newServerInput("pg-fifth")); err != nil {
		t.Fatalf("one more server fits: %v", err)
	}
	if _, err := a.CreateServer("tester", newServerInput("pg-sixth")); err == nil {
		t.Fatal("the sixth server is over the quota")
	}
}

func writeLicense(e *apptest.Env, key string) error {
	return e.App.SetLicenseKey(key)
}

func TestLicenseGraceAndExpiry(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.License.Key = "" })
	a := e.App
	key := e.LicenseKey(license.Pro, 10, "2026-10-10")
	if err := a.SetLicenseKey(key); err != nil {
		t.Fatal(err)
	}
	// the app clock is 2026-10-20: ten days past expiry = grace
	li := a.LicenseInfo()
	if li.State != license.StateGrace || li.Max != 10 || !strings.Contains(li.Banner, "expired on 2026-10-10") {
		t.Fatalf("grace: %+v", li)
	}
	if _, err := a.CreateServer("tester", newServerInput("pg-grace")); err != nil {
		t.Fatalf("in grace the licence still counts: %v", err)
	}
	// 40 days past expiry: back to Free rules
	late := time.Date(2026, 11, 25, 12, 0, 0, 0, time.Local)
	a.Now = func() time.Time { return late }
	li = a.LicenseInfo()
	if li.State != license.StateExpired || li.Max != 2 || !strings.Contains(li.Banner, "Free edition") {
		t.Fatalf("expired: %+v", li)
	}
	if _, err := a.CreateServer("tester", newServerInput("pg-late")); err == nil {
		t.Fatal("after the grace period adding is limited again")
	}
}

func TestLicenseInvalidKeyFallsBackToFree(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.License.Key = "PGC1.bogus.bogus" })
	li := e.App.LicenseInfo()
	if li.State != license.StateInvalid || li.Max != 2 || li.Message == "" || li.Banner == "" {
		t.Fatalf("invalid: %+v", li)
	}
}

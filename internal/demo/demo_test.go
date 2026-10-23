package demo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

func init() { config.BcryptCost = 4 }

// open starts a demo instance at the given time. Open puts the fake programs
// on PATH and sets three variables: t.Setenv restores them at the end.
func open(t *testing.T, now time.Time) *Demo {
	t.Helper()
	t.Setenv("PATH", os.Getenv("PATH"))
	for _, k := range []string{"PGCELLAR_TELEGRAM_TOKEN", "PGCELLAR_SMTP_USER", "PGCELLAR_SMTP_PASSWORD", "PGCELLAR_BASE_URL"} {
		t.Setenv(k, "")
	}
	d, err := Open(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.App.Close() })
	return d
}

func at(h, m int) time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, time.Local)
}

func TestFixturesLoaded(t *testing.T) {
	d := open(t, at(15, 40))
	a := d.App
	states, _, _ := a.States()
	if len(states) != 5 {
		t.Fatalf("%d servers, want 5", len(states))
	}
	want := map[string]string{ErpProd: app.LevelOK, CrmProd: app.LevelOK, Analytics: app.LevelWarn, ShopStaging: app.LevelFail, Legacy9: app.LevelFail}
	for _, s := range states {
		if s.Level != want[s.Name] {
			t.Errorf("%s: level %s, want %s (problems %v)", s.Name, s.Level, want[s.Name], s.Problems)
		}
	}
	by := map[string]app.ServerState{}
	for _, s := range states {
		by[s.Name] = s
	}
	if n := by[ErpProd].BackupCount; n != 14 {
		t.Errorf("erp-prod has %d backups, want 14", n)
	}
	if !by[ErpProd].StreamingLive || !by[Analytics].StreamingLive {
		t.Error("erp-prod and analytics must have a live WAL stream")
	}
	if by[ShopStaging].StreamingLive || !by[ShopStaging].StreamingMonitored {
		t.Error("shop-staging: streaming must be monitored and stopped")
	}
	if by[CrmProd].SetupIssues != 1 {
		t.Errorf("crm-prod: %d setup issues, want 1 (max_slot_wal_keep_size)", by[CrmProd].SetupIssues)
	}
	if by[Legacy9].BackupCount != 1 {
		t.Errorf("legacy-9: %d backups, want 1", by[Legacy9].BackupCount)
	}
	if got := by[Analytics].LastBackupAgeSeconds; got == nil || *got < 30*3600 || *got > 32*3600 {
		t.Errorf("analytics: last backup age %v, want about 31 h", got)
	}

	// The off-site copy: every server but staging, all fresh, one hiccup on crm-prod.
	for _, n := range []string{ErpProd, CrmProd, Analytics, Legacy9} {
		v := by[n].Vault
		if v == nil || !v.Copied || v.Last == nil || !v.Last.OK || v.Stale || v.UsedBytes == 0 {
			t.Errorf("%s vault: %+v", n, v)
		}
	}
	if v := by[ShopStaging].Vault; v == nil || v.Copied {
		t.Errorf("shop-staging is not copied: %+v", v)
	}
	vv := a.VaultStatus()
	if !vv.Enabled || !vv.Lock.Locked() || vv.Lock.Days != 30 || vv.QuotaLevel != app.LevelOK || vv.UsedPct() < 40 || vv.UsedPct() > 60 || vv.KeyDownloadedBy != "maria" || vv.LastTest == nil {
		t.Errorf("vault view: %+v", vv)
	}
	if line := a.VaultSummary(states); line == nil || line.Text != "🔐 Off-site copy: 4 of 4 servers up to date · 252.6 GB of 500 GB used (51 %) · locked 30 days" {
		t.Errorf("vault line: %+v", line)
	}
	copies, _ := a.ListVaultCopies(CrmProd, 200, false)
	failed := 0
	for _, c := range copies {
		if !c.OK {
			failed++
		}
	}
	if len(copies) < 90 || failed != 1 {
		t.Errorf("crm-prod: %d copies, %d failed", len(copies), failed)
	}

	// The schedule: analytics runs daily, legacy-9 is off.
	d1, _ := a.GetServer(context.Background(), Legacy9)
	if d1.Schedule.IsEnabled() {
		t.Error("legacy-9 must have its schedule off")
	}

	// Timeline coherent with the backups.
	for _, name := range []string{ErpProd, CrmProd, Analytics, ShopStaging} {
		tl, err := a.GetTimeline(context.Background(), name)
		if err != nil || !tl.Available || len(tl.Backups) == 0 || tl.Totals.WALFiles == 0 {
			t.Errorf("%s: timeline %+v err %v", name, tl, err)
		}
	}
	tl, _ := a.GetTimeline(context.Background(), ErpProd)
	restorable := 0
	for _, b := range tl.Backups {
		if b.Restorable {
			restorable++
		}
	}
	if restorable < 13 {
		t.Errorf("erp-prod: %d restorable backups of %d", restorable, len(tl.Backups))
	}

	// Drills: one passed, one failed at "Start PostgreSQL".
	drills, _ := a.ListDrills("", 10)
	if len(drills) != 2 {
		t.Fatalf("%d drills, want 2", len(drills))
	}
	for _, dr := range drills {
		switch dr.Server {
		case ErpProd:
			if !dr.OK || !dr.HasReport {
				t.Errorf("erp-prod drill: %+v", dr)
			}
			if dr.StartedAt.Weekday() != time.Sunday {
				t.Errorf("erp-prod drill on %s, want Sunday", dr.StartedAt.Weekday())
			}
		case ShopStaging:
			if dr.OK || dr.StepFailed != app.StepStart || !dr.HasReport {
				t.Errorf("shop-staging drill: %+v", dr)
			}
		default:
			t.Errorf("drill of %s", dr.Server)
		}
	}

	runs, _ := a.Runs("", 100)
	if len(runs) < 15 || len(runs) > 25 {
		t.Errorf("%d runs, want about twenty", len(runs))
	}
	kinds, failed := map[string]int{}, 0
	for _, r := range runs {
		kinds[r.Kind]++
		if r.Status == app.RunFailed && r.Kind == "backup" {
			failed++
		}
	}
	if kinds["drill"] != 2 || kinds["recover"] != 1 || failed != 1 {
		t.Errorf("runs by kind %v, failed backups %d", kinds, failed)
	}
	recs, _ := a.ListRecoveries("")
	if len(recs) != 1 || recs[0].ArchiveBytes < 3*int64(gib) || recs[0].ArchiveBytes > 4*int64(gib) {
		t.Errorf("recoveries %+v", recs)
	}
	hist, err := a.History(20)
	if err != nil || len(hist) != 7 {
		t.Errorf("history: %d commits, err %v", len(hist), err)
	}
	if len(hist) > 0 && !strings.Contains(hist[0].Message, "Change retention of analytics to 14 days") {
		t.Errorf("newest commit %q", hist[0].Message)
	}
	audit, _ := a.Audit(200)
	if len(audit) < 40 || len(audit) > 45 {
		t.Errorf("%d audit lines, want about 40", len(audit))
	}
	journal, _ := a.AlertJournal(20)
	if len(journal) != 5 {
		t.Errorf("%d alerts sent, want 5", len(journal))
	}
	chans := a.AlertChannels()
	if len(chans) != 2 || !chans[0].Ready || !chans[1].Ready {
		t.Errorf("channels %+v", chans)
	}
	keys, _ := a.AuthorizedKeys()
	if len(keys) != 2 {
		t.Errorf("%d authorised keys, want 2", len(keys))
	}
	if li := a.LicenseInfo(); !strings.Contains(li.Footer, Company) || !li.Unlimited() {
		t.Errorf("licence %+v", li)
	}

	// Storage: a 4 TiB volume with 1.8 TiB free, and the servers' share is
	// below what is used.
	var mine int64
	for _, s := range states {
		if s.Storage == nil || s.Storage.FS == nil {
			t.Fatalf("%s: no storage figures", s.Name)
		}
		fs := s.Storage.FS
		if fs.TotalBytes != 4*tib || fs.FreeBytes != 1843*gib || fs.Path != "/var/lib/barman" {
			t.Errorf("%s: fs %+v", s.Name, fs)
		}
		if s.Storage.DaysUntilFull != nil {
			t.Errorf("%s: shows a full disk in %d days", s.Name, *s.Storage.DaysUntilFull)
		}
		mine += s.Storage.BackupDiskBytes + s.Storage.WALBytes + s.Storage.RecoveriesBytes
	}
	if used := 4*tib - 1843*gib; mine >= used || mine < 100*gib {
		t.Errorf("the servers hold %s of %s used", app.FormatSize(mine), app.FormatSize(used))
	}
}

// The ages the visitor sees do not depend on the time of the day at which the
// instance started.
func TestRelativeClock(t *testing.T) {
	ages := func(now time.Time) map[string]float64 {
		p := newPlan(now)
		out := map[string]float64{}
		for _, s := range servers() {
			list, _ := p.backups(s, now)
			out[s.Name] = now.Sub(list[0].End).Hours()
		}
		return out
	}
	early, late := ages(at(3, 10)), ages(at(15, 40))
	for name, a := range early {
		if d := a - late[name]; d > 0.1 || d < -0.1 {
			t.Errorf("%s: last backup %.2f h old at 03:10 and %.2f h at 15:40", name, a, late[name])
		}
	}
	if early[ErpProd] < 1.9 || early[ErpProd] > 2.2 || early[Analytics] < 30.9 || early[Analytics] > 31.2 {
		t.Errorf("ages at start: %v", early)
	}

	// The same through the application.
	one, two := open(t, at(3, 10)), open(t, at(15, 40))
	for _, name := range []string{ErpProd, CrmProd, Analytics, ShopStaging} {
		a, _ := one.App.State(name)
		b, _ := two.App.State(name)
		if a.LastBackupAgeSeconds == nil || b.LastBackupAgeSeconds == nil {
			t.Fatalf("%s: no age", name)
		}
		if d := *a.LastBackupAgeSeconds - *b.LastBackupAgeSeconds; d > 360 || d < -360 {
			t.Errorf("%s: %d s and %d s", name, *a.LastBackupAgeSeconds, *b.LastBackupAgeSeconds)
		}
		if a.Level != b.Level {
			t.Errorf("%s: level %s and %s", name, a.Level, b.Level)
		}
	}
}

// Whatever the hour, the five servers keep the story they are meant to tell.
func TestStoryAtAnyHour(t *testing.T) {
	for h := 0; h < 24; h += 3 {
		now := at(h, 17)
		p := newPlan(at(15, 40)) // times of day chosen at "start", then the clock moves
		s := map[string]server{}
		for _, x := range servers() {
			s[x.Name] = x
		}
		for _, name := range []string{ErpProd, CrmProd} {
			list, _ := p.backups(s[name], now)
			if age := now.Sub(list[0].End); age > 25*time.Hour || len(list) != 14 {
				t.Errorf("hour %d: %s last backup %s old, %d backups", h, name, age, len(list))
			}
		}
		list, skipped := p.backups(s[Analytics], now)
		if age := now.Sub(list[0].End); age < 26*time.Hour || age > 51*time.Hour || skipped.IsZero() {
			t.Errorf("hour %d: analytics last good backup %s old (skipped %v)", h, age, skipped)
		}
	}
}

func TestAssetsExtracted(t *testing.T) {
	dir := t.TempDir()
	if err := ExtractAssets(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"fake-barman", "fake-psql", "fake-pg_ctl", "fake-pg_isready", "fake-pg_checksums", "fake-pg_basebackup", "fake-ssh", "barman", "psql"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.Mode()&0o100 == 0 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fixtures", "psql-drill-lsn.tsv")); err != nil {
		t.Error(err)
	}
}

func TestBaseURLFromEnvironment(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("PGCELLAR_BASE_URL", "https://demo.example.test")
	d, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.App.Close() }()
	if got := d.App.Config().BaseURL; got != "https://demo.example.test" {
		t.Errorf("base_url = %q", got)
	}
	if !d.App.Demo || d.App.Config().Scheduler.IsEnabled() || d.App.Config().Barman.CronInterval != 0 {
		t.Error("the demo must not run a scheduler or barman cron")
	}
}

func TestCheckFakesRefusesNonExecutable(t *testing.T) {
	good := filepath.Join(t.TempDir(), "bin")
	if err := ExtractAssets(good); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", good+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := checkFakes(good); err != nil {
		t.Fatalf("extracted fakes refused: %v", err)
	}
	// Same files without the execute bit, like a noexec mount: whatever else is
	// on PATH must not be accepted instead.
	bad := filepath.Join(t.TempDir(), "bin")
	if err := ExtractAssets(bad); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(bad)
	for _, e := range entries {
		if !e.IsDir() && e.Type().IsRegular() {
			_ = os.Chmod(filepath.Join(bad, e.Name()), 0o644)
		}
	}
	t.Setenv("PATH", bad+string(os.PathListSeparator)+good)
	err := checkFakes(bad)
	if err == nil || !strings.Contains(err.Error(), "not executable (noexec mount?)") {
		t.Fatalf("checkFakes = %v", err)
	}
}

package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/report"
)

func startDrill(t *testing.T, e *apptest.Env, in app.DrillInput) (*app.RunView, *app.DrillView) {
	t.Helper()
	id, err := e.App.StartDrill(context.Background(), "gilles", "pg-main", in)
	if err != nil {
		t.Fatal(err)
	}
	run := apptest.WaitRun(t, e.App, id)
	d, ok := e.App.DrillOfRun(id)
	if !ok {
		t.Fatalf("no drill recorded for run %d: %+v", id, run)
	}
	return run, d
}

func stepStatus(d *app.DrillView, key string) string {
	for _, s := range d.Details.Steps {
		if s.Key == key {
			return s.Status
		}
	}
	return "absent"
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestDrillEndToEnd(t *testing.T) {
	e := apptest.New(t)
	ctl := filepath.Join(e.Dir, "pgctl.log")
	chk := filepath.Join(e.Dir, "checksums.log")
	t.Setenv("FAKE_PGCTL_LOG", ctl)
	t.Setenv("FAKE_CHECKSUMS_LOG", chk)

	run, d := startDrill(t, e, app.DrillInput{})
	if run.Status != app.RunOK || run.Kind != "drill" {
		t.Fatalf("run: %+v", run)
	}
	if !d.OK || d.StepFailed != "" {
		t.Fatalf("drill failed: %+v", d)
	}
	if d.BackupID != "20261020T020001" {
		t.Errorf("default backup = %s, want the latest DONE one", d.BackupID)
	}
	for _, k := range []string{"recover", "version", "configure", "start", "recovery", "checks", "stop", "checksums", "cleanup"} {
		if stepStatus(d, k) != "ok" {
			t.Errorf("step %s = %s", k, stepStatus(d, k))
		}
	}
	det := d.Details
	if det.PGMajor != "16" || !strings.HasPrefix(det.PGVersion, "PostgreSQL 16.4") || det.LSN != "0/7000148" {
		t.Errorf("details: %+v", det)
	}
	if det.DatabasesN != 2 || det.TablesN != 14 || len(det.Databases) != 2 || det.Databases[0].Name != "app" || det.Databases[0].Tables != 11 {
		t.Errorf("databases: %+v", det.Databases)
	}
	if det.Port < 54400 || det.Port > 54499 {
		t.Errorf("port %d", det.Port)
	}
	if det.Checksums != "verified" {
		t.Errorf("checksums = %q", det.Checksums)
	}
	if !strings.Contains(det.Summary, "was restored and started successfully") || !strings.Contains(det.Summary, "2 databases, 14 tables") ||
		!strings.HasPrefix(det.Summary, "On 20 Oct 2026 at 02:30 the backup 20261020T020001 of pg-main") {
		t.Errorf("summary: %q", det.Summary)
	}
	// What the run shows: the exact commands, in order.
	out := run.Stdout
	order := []string{"barman recover pg-main 20261020T020001", "pg_ctl -D", "start", "pg_isready", "select pg_is_in_recovery()", "select version()", "-m fast stop", "pg_checksums --check"}
	pos := 0
	for _, w := range order {
		i := strings.Index(out[pos:], w)
		if i < 0 {
			t.Fatalf("%q not found in order in the journal:\n%s", w, out)
		}
		pos += i
	}
	// The copy was configured for the drill, after Barman's own lines.
	if !det.ReportWritten || det.Fingerprint == "" {
		t.Fatalf("no report: %+v", det)
	}
	// pg_ctl was called with the port, -w, -t 300 and a log file.
	calls := strings.Join(logLines(t, ctl), "\n")
	if !strings.Contains(calls, "-o -p ") || !strings.Contains(calls, "-w -t 300 -l ") || !strings.Contains(calls, " start") || !strings.Contains(calls, "-m fast stop") {
		t.Errorf("pg_ctl calls:\n%s", calls)
	}
	// keep_last: the copy of the successful drill is still there, with our settings.
	if det.DataDir == "" {
		t.Fatal("the copy was not kept")
	}
	conf, err := os.ReadFile(filepath.Join(det.DataDir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"restore_command = 'cp barman_wal/%f %p'", "listen_addresses = '127.0.0.1'", "archive_mode = off", "max_connections = 10", "shared_buffers = 64MB", "unix_socket_directories = '", "hot_standby = off", "lc_messages = 'C'", "lc_monetary = 'C'", "lc_numeric = 'C'", "lc_time = 'C'"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(string(conf), "recovery_target_action") {
		t.Error("recovery_target_action must only be set with a target")
	}

	// The report: a valid PDF, 0640, in <recoveries>/<server>/reports.
	path, v, err := e.App.DrillReport("gilles", d.ID)
	if err != nil || v.ID != d.ID {
		t.Fatalf("report: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(e.App.Config().Recoveries.Dir, "pg-main", "reports") || !strings.HasPrefix(filepath.Base(path), "drill-") || !strings.HasSuffix(path, ".pdf") {
		t.Errorf("report path %s", path)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("report mode %v", fi.Mode().Perm())
	}
	pdf, _ := os.ReadFile(path)
	if _, err := report.Check(pdf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pdf), det.Fingerprint) || !strings.Contains(string(pdf), "RESTORE VERIFIED") || !strings.Contains(string(pdf), "pg-main") {
		t.Error("the report lacks its fingerprint, its verdict or the server")
	}
}

func TestDrillStartFailureNamesTheStep(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_PGCTL_FAIL", "1")
	run, d := startDrill(t, e, app.DrillInput{})
	if run.Status != app.RunFailed || d.OK || d.StepFailed != "start" {
		t.Fatalf("run %s, drill %+v", run.Status, d)
	}
	if stepStatus(d, "recover") != "ok" || stepStatus(d, "start") != "failed" || stepStatus(d, "recovery") != "absent" {
		t.Errorf("steps: %+v", d.Details.Steps)
	}
	if !strings.Contains(d.Details.Error, "could not start server") || !strings.Contains(d.Details.LogTail, "could not locate a valid checkpoint record") {
		t.Errorf("error %q, log %q", d.Details.Error, d.Details.LogTail)
	}
	if !strings.Contains(d.Details.Summary, "FAILED at the step \"Start PostgreSQL\"") {
		t.Errorf("summary %q", d.Details.Summary)
	}
	if strings.Contains(run.Stdout, "-m fast stop") {
		t.Error("nothing runs after a failed start: no stop expected")
	}
	// The copy of a failed drill is removed, the report is written.
	if d.Details.DataDir != "" {
		t.Errorf("a failed drill keeps no copy: %s", d.Details.DataDir)
	}
	dirs, _ := filepath.Glob(filepath.Join(e.App.Config().Recoveries.Dir, "pg-main", "drill-*"))
	if len(dirs) != 0 {
		t.Errorf("leftover copies: %v", dirs)
	}
	path, _, err := e.App.DrillReport("gilles", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	pdf, _ := os.ReadFile(path)
	if _, err := report.Check(pdf); err != nil || !strings.Contains(string(pdf), "FAILED") || !strings.Contains(string(pdf), "checkpoint record") {
		t.Errorf("failed report: %v", err)
	}
}

func TestDrillRestoreFailureAndMissingBinaries(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_BARMAN_RECOVER_FAIL", "1")
	_, d := startDrill(t, e, app.DrillInput{})
	if d.OK || d.StepFailed != "recover" || !strings.Contains(d.Details.Error, "No space left") {
		t.Errorf("restore failure: %+v", d)
	}

	// No fake: the real lookup under drills.bin_dir.
	e2 := apptest.NewWith(t, func(c *config.Config) { c.Drills.BinDir = filepath.Join(t.TempDir(), "pgbin") })
	e2.App.DrillTools = app.DrillTools{}
	t.Setenv("FAKE_BARMAN_RECOVER_FAIL", "")
	t.Setenv("FAKE_BARMAN_PG_VERSION", "17")
	_, d2 := startDrill(t, e2, app.DrillInput{})
	if d2.OK || d2.StepFailed != "version" || !strings.Contains(d2.Details.Error, "PostgreSQL 17 binaries are not in this image") {
		t.Errorf("missing binaries: %+v", d2)
	}
}

func TestDrillRecoveryTimeout(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.Drills.RecoveryTimeout = 300 * time.Millisecond })
	t.Setenv("FAKE_PGISREADY_NEVER", "1")
	_, d := startDrill(t, e, app.DrillInput{})
	if d.OK || d.StepFailed != "recovery" || !strings.Contains(d.Details.Error, "did not finish its recovery within drills.recovery_timeout") {
		t.Fatalf("%+v", d)
	}
	// The copy is stopped even so.
	if stepStatus(d, "stop") != "ok" {
		t.Errorf("stop step: %s", stepStatus(d, "stop"))
	}
}

func TestDrillOverallTimeout(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.Drills.Timeout = 400 * time.Millisecond })
	t.Setenv("FAKE_BARMAN_RECOVER_DELAY", "2")
	start := time.Now()
	run, d := startDrill(t, e, app.DrillInput{})
	if run.Status != app.RunFailed || d.OK || d.StepFailed != "recover" || !strings.Contains(d.Details.Error, "ran out of time") {
		t.Fatalf("%+v", d)
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("the timeout did not cut the restore short: %s", time.Since(start))
	}
}

func TestDrillStuckInRecovery(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.Drills.RecoveryTimeout = 300 * time.Millisecond })
	t.Setenv("FAKE_DRILL_STUCK", "1")
	_, d := startDrill(t, e, app.DrillInput{})
	if d.OK || d.StepFailed != "recovery" {
		t.Fatalf("%+v", d)
	}
}

func TestDrillChecksumFailure(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_CHECKSUMS_BAD", "1")
	_, d := startDrill(t, e, app.DrillInput{})
	if d.OK || d.StepFailed != "checksums" || d.Details.Checksums != "FAILED" || !strings.Contains(d.Details.Error, "checksum verification failed") {
		t.Fatalf("%+v", d)
	}
	if d.Details.DataDir != "" {
		t.Error("a failed drill must not keep its copy")
	}
}

func TestDrillChecksumsOffAndVerifyTables(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.Drills.VerifyTables = true })
	t.Setenv("FAKE_DRILL_CHECKSUMS", "off")
	run, d := startDrill(t, e, app.DrillInput{})
	if !d.OK || stepStatus(d, "checksums") != "skipped" || d.Details.Checksums != "not enabled" {
		t.Fatalf("%+v", d)
	}
	if strings.Contains(run.Stdout, "pg_checksums") && strings.Contains(run.Stdout, "$ fake-pg_checksums") {
		t.Error("pg_checksums must not run when data checksums are off")
	}
	app0 := d.Details.Databases[0]
	if app0.Rows == nil || *app0.Rows != 2000 { // two tables of the fixture, 1000 rows each
		t.Errorf("rows counted: %+v", app0)
	}
	t.Setenv("FAKE_DRILL_COUNT_FAIL", "1")
	_, d2 := startDrill(t, e, app.DrillInput{})
	if d2.OK || d2.StepFailed != "checks" || !strings.Contains(d2.Details.Error, "could not read block") {
		t.Errorf("count failure: %+v", d2)
	}
}

func TestDrillKeepLast(t *testing.T) {
	e := apptest.New(t)
	_, d1 := startDrill(t, e, app.DrillInput{})
	_, d2 := startDrill(t, e, app.DrillInput{})
	if !d1.OK || !d2.OK || d1.Details.DataDir == "" || d1.Details.DataDir == d2.Details.DataDir {
		t.Fatalf("%q %q", d1.Details.DataDir, d2.Details.DataDir)
	}
	if _, err := os.Stat(d1.Details.DataDir); !os.IsNotExist(err) {
		t.Errorf("the previous copy must be removed: %v", err)
	}
	if _, err := os.Stat(d2.Details.DataDir); err != nil {
		t.Errorf("the last copy must stay: %v", err)
	}
	old, _ := e.App.GetDrill(d1.ID)
	if old.Details.DataDir != "" || !old.HasReport {
		t.Errorf("the record of the first drill keeps the report but not the copy: %+v", old)
	}
	// A failed drill does not touch the last good copy.
	t.Setenv("FAKE_PGCTL_FAIL", "1")
	_, d3 := startDrill(t, e, app.DrillInput{})
	if d3.OK {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(d2.Details.DataDir); err != nil {
		t.Errorf("the last good copy must survive a failed drill: %v", err)
	}
}

func TestDrillNoKeep(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) { c.Drills.KeepLast = config.Ptr(false) })
	_, d := startDrill(t, e, app.DrillInput{})
	if !d.OK || d.Details.DataDir != "" {
		t.Fatalf("%+v", d)
	}
	dirs, _ := filepath.Glob(filepath.Join(e.App.Config().Recoveries.Dir, "pg-main", "drill-*"))
	if len(dirs) != 0 {
		t.Errorf("copies left: %v", dirs)
	}
}

func TestDrillTargetsAndValidation(t *testing.T) {
	e := apptest.New(t)
	_, d := startDrill(t, e, app.DrillInput{Target: "backup_end", BackupID: "20261019T020001"})
	if !d.OK || d.BackupID != "20261019T020001" || d.Details.Target != "backup_end" {
		t.Fatalf("%+v", d)
	}
	conf, _ := os.ReadFile(filepath.Join(d.Details.DataDir, "postgresql.auto.conf"))
	if !strings.Contains(string(conf), "recovery_target_action = 'promote'") {
		t.Errorf("a drill with a target promotes at the target:\n%s", conf)
	}
	for _, in := range []app.DrillInput{{BackupID: "20250101T000000"}, {Target: "yesterday"}, {Target: "time", TargetTime: "soon"}} {
		_, err := e.App.StartDrill(context.Background(), "gilles", "pg-main", in)
		var pe *app.ProblemsError
		if !errors.As(err, &pe) {
			t.Errorf("%+v: want a ProblemsError, got %v", in, err)
		}
	}
	if _, err := e.App.StartDrill(context.Background(), "gilles", "nope", app.DrillInput{}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("unknown server: %v", err)
	}
}

func TestDrillOneAtATimeAndNeverDuringABackup(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_BARMAN_RECOVER_DELAY", "1")
	id, err := e.App.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{})
	if err != nil {
		t.Fatal(err)
	}
	_, err2 := e.App.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{})
	var ce *app.ConflictError
	if !errors.As(err2, &ce) || !ce.Retry {
		t.Errorf("a second drill: %v", err2)
	}
	_, err3 := e.App.StartBackup(context.Background(), "gilles", "pg-main")
	if !errors.As(err3, &ce) || !strings.Contains(ce.Msg, "restore drill") {
		t.Errorf("a backup during a drill: %v", err3)
	}
	apptest.WaitRun(t, e.App, id)

	t.Setenv("FAKE_BARMAN_RECOVER_DELAY", "")
	t.Setenv("FAKE_BARMAN_BACKUP_DELAY", "1")
	var bid int64
	for i := 0; i < 100; i++ { // the drill frees its slot just after its run is recorded
		if bid, err = e.App.StartBackup(context.Background(), "gilles", "pg-main"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, err4 := e.App.StartDrill(context.Background(), "gilles", "pg-main", app.DrillInput{})
	if !errors.As(err4, &ce) || !ce.Retry || !strings.Contains(ce.Msg, "backup") {
		t.Errorf("a drill during a backup: %v", err4)
	}
	apptest.WaitRun(t, e.App, bid)
}

func TestDrillSurvivesAFailingReportDir(t *testing.T) {
	e := apptest.New(t)
	root := e.App.Config().Recoveries.Dir
	if err := os.MkdirAll(filepath.Join(root, "pg-main"), 0o750); err != nil {
		t.Fatal(err)
	}
	// "reports" is a file: the report cannot be written, the drill is still recorded.
	if err := os.WriteFile(filepath.Join(root, "pg-main", "reports"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	run, d := startDrill(t, e, app.DrillInput{})
	if !d.OK || d.HasReport || !strings.Contains(run.Stdout, "The report could not be written") {
		t.Fatalf("%+v", d)
	}
	if _, _, err := e.App.DrillReport("gilles", d.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("no report: %v", err)
	}
}

func TestDrillStatusAndLevel(t *testing.T) {
	e := apptest.NewWith(t, func(c *config.Config) {
		c.Servers[0].Drills.Schedule = "weekly sun 04:30"
	})
	now := e.App.Now()
	st, _ := e.App.State("pg-main")
	if st.Drill == nil || !st.Drill.Scheduled || st.Drill.Text != "No drill yet" || st.Drill.Stale || st.Level != app.LevelOK {
		t.Fatalf("before any drill: %+v level %s", st.Drill, st.Level)
	}
	if st.Drill.Next.IsZero() {
		t.Error("next drill time missing")
	}
	_, d := startDrill(t, e, app.DrillInput{})
	st, _ = e.App.State("pg-main")
	if !d.OK || st.Drill.Text != "Last drill: just now \u2714" || st.Drill.Stale || st.Level != app.LevelOK {
		t.Fatalf("after a good drill: %+v level %s", st.Drill, st.Level)
	}
	// Eight days and a bit later: orange.
	e.App.Now = func() time.Time { return now.Add(9 * 24 * time.Hour) }
	st, _ = e.App.State("pg-main")
	if !st.Drill.Stale || !strings.Contains(st.Drill.Text, "9 d ago \u2714") || st.Level != app.LevelWarn || !strings.Contains(strings.Join(st.Problems, " "), "No restore drill succeeded") {
		t.Fatalf("overdue: %+v level %s problems %v", st.Drill, st.Level, st.Problems)
	}
	// A failed drill is orange too, with the step.
	e.App.Now = func() time.Time { return now.Add(time.Hour) }
	t.Setenv("FAKE_PGCTL_FAIL", "1")
	startDrill(t, e, app.DrillInput{})
	e.App.Now = func() time.Time { return now.Add(2 * time.Hour) }
	st, _ = e.App.State("pg-main")
	if !st.Drill.Stale || st.Level != app.LevelWarn || !strings.Contains(st.Drill.Text, "failed at \"Start PostgreSQL\"") {
		t.Fatalf("failed: %+v level %s", st.Drill, st.Level)
	}
	// Without a drill schedule nothing turns orange.
	e2 := apptest.New(t)
	t.Setenv("FAKE_PGCTL_FAIL", "1")
	startDrill(t, e2, app.DrillInput{})
	st2, _ := e2.App.State("pg-main")
	if st2.Drill.Scheduled || st2.Drill.Stale || st2.Level != app.LevelOK {
		t.Errorf("no schedule: %+v level %s", st2.Drill, st2.Level)
	}
}

func TestAuditOfADrill(t *testing.T) {
	e := apptest.New(t)
	startDrill(t, e, app.DrillInput{})
	lines, err := e.App.Store.ListAudit(200)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, l := range lines {
		all = append(all, l.Actor+"|"+l.Action)
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{"gilles|drill.start", "gilles|drill.end", "gilles|fake-pg_ctl -D", "gilles|fake-pg_isready", "gilles|fake-pg_checksums --check"} {
		if !strings.Contains(text, want) {
			t.Errorf("the audit log lacks %q:\n%s", want, text)
		}
	}
}

func scheduledDrillRuns(t *testing.T, a *app.App, server string) int {
	t.Helper()
	runs, err := a.Runs(server, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range runs {
		if r.Actor == app.SchedulerActor && r.Kind == "drill" {
			n++
		}
	}
	return n
}

func drillScheduleEnv(t *testing.T, mutate func(*config.Config)) *apptest.Env {
	return apptest.NewWith(t, func(c *config.Config) {
		c.Servers[0].Schedule.Enabled = config.Ptr(false)
		c.Servers[0].Drills.Schedule = "daily 03:10"
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestSchedulerStartsDrillsOncePerDueTime(t *testing.T) {
	e := drillScheduleEnv(t, nil)
	a := e.App
	ctx := context.Background()
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	a.SchedulerTick(ctx, at(20, 3, 9, 59))
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("nothing is due yet, got %d", n)
	}
	a.SchedulerTick(ctx, at(20, 3, 10, 5))
	waitIdle(t, a, "pg-main")
	a.SchedulerTick(ctx, at(20, 3, 11, 0))
	a.SchedulerTick(ctx, at(20, 3, 40, 0))
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("one due time, one drill: got %d", n)
	}
	list, _ := a.ListDrills("pg-main", 10)
	if len(list) != 1 || !list[0].OK || list[0].Details.Actor != app.SchedulerActor {
		t.Fatalf("drills: %+v", list)
	}
	a.SchedulerRestart()
	a.SchedulerTick(ctx, at(20, 3, 45, 0))
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("a restart must not repeat the drill: got %d", n)
	}
	a.SchedulerTick(ctx, at(21, 3, 10, 30))
	waitIdle(t, a, "pg-main")
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 2 {
		t.Fatalf("next day: got %d", n)
	}
	st, _ := a.State("pg-main")
	if st.Drill.LastScheduled == nil || st.Drill.LastScheduled.Status != "ok" || !st.Drill.LastScheduled.DueAt.Equal(at(21, 3, 10, 0)) {
		t.Errorf("last scheduled drill: %+v", st.Drill.LastScheduled)
	}
}

func TestSchedulerDrillWaitsForTheBackupOfTheServer(t *testing.T) {
	e := drillScheduleEnv(t, nil)
	a := e.App
	ctx := context.Background()
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	t.Setenv("FAKE_BARMAN_BACKUP_DELAY", "1")
	bid, err := a.StartBackup(ctx, "gilles", "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	a.SchedulerTick(ctx, at(20, 3, 10, 5))
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("a drill never runs during a backup of the server: got %d", n)
	}
	apptest.WaitRun(t, a, bid)
	a.SchedulerTick(ctx, at(20, 3, 12, 0)) // the due time was kept: the drill starts now
	waitIdle(t, a, "pg-main")
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("the drill must start once the backup is over: got %d", n)
	}
	a.SchedulerTick(ctx, at(20, 3, 13, 0))
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("still one: got %d", n)
	}
}

func TestSchedulerDrillsOneAtATime(t *testing.T) {
	e := drillScheduleEnv(t, func(c *config.Config) {
		c.Servers = append(c.Servers, config.Server{Name: "pg-rsync", Drills: config.ServerDrills{Schedule: "daily 03:10"}})
		c.Scheduler.Parallel = 4
	})
	a := e.App
	ctx := context.Background()
	t.Setenv("FAKE_BARMAN_RECOVER_DELAY", "1")
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	a.SchedulerTick(ctx, at(20, 3, 10, 5))
	total := scheduledDrillRuns(t, a, "pg-main") + scheduledDrillRuns(t, a, "pg-rsync")
	if total != 1 {
		t.Fatalf("one drill at a time, got %d", total)
	}
	waitIdle(t, a, "pg-main")
	waitIdle(t, a, "pg-rsync")
	t.Setenv("FAKE_BARMAN_RECOVER_DELAY", "")
	a.SchedulerTick(ctx, at(20, 3, 11, 0))
	waitIdle(t, a, "pg-main")
	waitIdle(t, a, "pg-rsync")
	total = scheduledDrillRuns(t, a, "pg-main") + scheduledDrillRuns(t, a, "pg-rsync")
	if total != 2 {
		t.Fatalf("the second drill follows the first: got %d", total)
	}
}

func TestSchedulerDrillsSwitchedOff(t *testing.T) {
	e := drillScheduleEnv(t, func(c *config.Config) { c.Drills.Enabled = config.Ptr(false) })
	a := e.App
	a.SchedulerTick(context.Background(), at(20, 2, 30, 0))
	a.SchedulerTick(context.Background(), at(20, 3, 10, 5))
	if n := scheduledDrillRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("drills.enabled false: got %d", n)
	}
	st, _ := a.State("pg-main")
	if st.Drill.Scheduled {
		t.Error("not scheduled when drills.enabled is false")
	}
}

func TestPurgeRemovesAbandonedDrillCopies(t *testing.T) {
	e := apptest.New(t)
	_, d := startDrill(t, e, app.DrillInput{})
	kept := d.Details.DataDir
	root := e.App.Config().Recoveries.Dir
	old := filepath.Join(root, "pg-main", "drill-20250101T000000")
	fresh := filepath.Join(root, "pg-main", "drill-20261020T235959")
	other := filepath.Join(root, "pg-main", "20261019T020001-20261020T010000") // a restore, not a drill
	for _, p := range []string{old, fresh, other} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-72 * time.Hour)
	for _, p := range []string{old, kept, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	e.App.PurgeRecoveries()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the abandoned copy must go")
	}
	for name, p := range map[string]string{"the kept copy": kept, "a recent copy": fresh, "a restore": other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must stay: %v", name, err)
		}
	}
}

func TestDrillMissingLocaleIsExplained(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_PGCTL_LOCALE_FAIL", "1")
	run, d := startDrill(t, e, app.DrillInput{})
	if d.OK || d.StepFailed != "start" {
		t.Fatalf("%+v", d)
	}
	want := "The cluster uses locale en_US.utf8, which this image does not have. Rebuild with PG_LOCALES including en_US.UTF-8 (see deploy/Dockerfile)."
	if d.Details.Error != want {
		t.Errorf("error = %q", d.Details.Error)
	}
	if !strings.Contains(run.Stdout, "last lines of drill.log") || !strings.Contains(run.Stdout, `invalid value for parameter "lc_messages"`) {
		t.Errorf("the log tail must be in the run output:\n%s", run.Stdout)
	}
}

func TestDrillWALInPgWalIsNotAWarning(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_BARMAN_NO_RESTORE_COMMAND", "1")
	run, d := startDrill(t, e, app.DrillInput{})
	if !d.OK || !strings.Contains(run.Stdout, "Barman placed the required WAL in pg_wal; no restore_command needed") || strings.Contains(run.Stdout, "warning: no restore_command") {
		t.Errorf("%s", run.Stdout)
	}
}

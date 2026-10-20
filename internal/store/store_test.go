package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pgcellar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	for i := 0; i < 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(); err != nil {
			t.Fatal(err)
		}
		if v, err := s.SchemaVersion(); err != nil || v != len(migrations) {
			t.Fatalf("version = %d, %v", v, err)
		}
		_ = s.Close()
	}
}

func TestRuns(t *testing.T) {
	s := newStore(t)
	id, err := s.StartRun("pg1", KindBackup, "gilles")
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := s.ListRuns("pg1", 10)
	if len(runs) != 1 || runs[0].FinishedAt != nil || runs[0].ExitCode != nil {
		t.Fatalf("running run wrong: %+v", runs)
	}
	if err := s.FinishRun(id, 0, "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(9999, 0, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	id2, _ := s.StartRun("pg1", KindBackup, "cron")
	_ = s.FinishRun(id2, 1, "", "boom")
	other, _ := s.StartRun("pg2", KindCheck, "gilles")
	_ = s.FinishRun(other, 0, "ok", "")

	last, err := s.LastRun("pg1", KindBackup)
	if err != nil || last.ID != id2 || *last.ExitCode != 1 || last.StderrTail != "boom" || last.FinishedAt == nil {
		t.Fatalf("LastRun = %+v, %v", last, err)
	}
	if last.StartedAt.Location() != time.UTC {
		t.Error("timestamps must be UTC")
	}
	if _, err := s.LastRun("pg1", KindDrill); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if all, _ := s.ListRuns("", 10); len(all) != 3 {
		t.Errorf("all runs = %d", len(all))
	}
	if lim, _ := s.ListRuns("", 2); len(lim) != 2 {
		t.Errorf("limit ignored: %d", len(lim))
	}
	if _, err := s.StartRun("pg1", "bogus", "x"); err == nil {
		t.Error("unknown kind must be rejected")
	}
}

func TestAudit(t *testing.T) {
	s := newStore(t)
	for _, a := range []string{"login", "backup", "restore"} {
		if err := s.AddAudit("gilles", a, "pg1", "details "+a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAudit(2)
	if err != nil || len(got) != 2 || got[0].Action != "restore" || got[1].Action != "backup" || got[0].Details != "details restore" {
		t.Fatalf("audit = %+v, %v", got, err)
	}
	if got[0].At.IsZero() {
		t.Error("missing timestamp")
	}
}

func TestSessions(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	_ = s.CreateSession(Session{ID: "live", User: "gilles", ExpiresAt: now.Add(time.Hour)})
	_ = s.CreateSession(Session{ID: "dead", User: "gilles", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)})

	got, err := s.GetSession("live")
	if err != nil || got.User != "gilles" || got.CreatedAt.IsZero() {
		t.Fatalf("GetSession = %+v, %v", got, err)
	}
	if _, err := s.GetSession("dead"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session must not be returned: %v", err)
	}
	if _, err := s.GetSession("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	n, err := s.PurgeExpiredSessions()
	if err != nil || n != 1 {
		t.Errorf("purged %d, %v", n, err)
	}
	if err := s.DeleteSession("live"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession("live"); !errors.Is(err, ErrNotFound) {
		t.Error("deleted session still there")
	}
}

func TestSettings(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.GetSetting("k"); ok || err != nil {
		t.Fatalf("unset: %v %v", ok, err)
	}
	_ = s.SetSetting("k", "v1")
	_ = s.SetSetting("k", "v2")
	if v, ok, err := s.GetSetting("k"); !ok || err != nil || v != "v2" {
		t.Errorf("got %q %v %v", v, ok, err)
	}
}

func TestGetRunAndPruneAudit(t *testing.T) {
	s := newStore(t)
	id, err := s.StartRun("pg1", KindBackup, "gilles")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRun(id)
	if err != nil || r.ID != id || r.Server != "pg1" {
		t.Fatalf("GetRun = %+v, %v", r, err)
	}
	if _, err := s.GetRun(id + 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing run: err = %v, want ErrNotFound", err)
	}

	if err := s.AddAudit("a", "x", "", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneAudit(time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("recent entry pruned: n=%d err=%v", n, err)
	}
	if n, err := s.PruneAudit(time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("old entry kept: n=%d err=%v", n, err)
	}
}

func TestStatusCache(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 10, 20, 2, 30, 0, 0, time.UTC)
	if err := s.SaveStatuses([]StatusRow{{"a", `{"name":"a"}`, at}, {"b", `{"name":"b"}`, at}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStatus(StatusRow{"a", `{"name":"a","level":"ok"}`, at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadStatuses()
	if err != nil || len(rows) != 2 || rows[0].Server != "a" || !strings.Contains(rows[0].JSON, "level") || !rows[0].UpdatedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("%v %v", rows, err)
	}
	if err := s.SaveStatuses([]StatusRow{{"b", `{}`, at}}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.LoadStatuses(); len(rows) != 1 || rows[0].Server != "b" {
		t.Fatalf("a replaced cache keeps only the new servers: %v", rows)
	}
}

func TestRecoveries(t *testing.T) {
	s := newStore(t)
	old := time.Now().Add(-10 * 24 * time.Hour)
	a, err := s.AddRecovery(Recovery{Server: "pg-main", BackupID: "20261018T020001", Target: "time", TargetTime: "2026-10-18 12:00:00+02:00", Path: "/r/pg-main/a", ArchivePath: "/r/pg-main/a.tar.gz", SizeBytes: 100, ArchiveSize: 40, RunID: 7, CreatedAt: old, Actor: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.AddRecovery(Recovery{Server: "pg-main", BackupID: "20261019T020001", Path: "/r/pg-main/b"})
	if _, err := s.AddRecovery(Recovery{Server: "other", BackupID: "20261019T020001", Path: "/r/other/c"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRecovery(a)
	if err != nil || got.Server != "pg-main" || got.ArchivePath != "/r/pg-main/a.tar.gz" || got.ArchiveSize != 40 || got.Target != "time" || !got.CreatedAt.Equal(old.Truncate(time.Second)) {
		t.Fatalf("%+v %v", got, err)
	}
	if r, _ := s.GetRecovery(b); r.Target != "latest" {
		t.Errorf("default target: %q", r.Target)
	}
	list, _ := s.ListRecoveries("pg-main")
	if len(list) != 2 || list[0].ID != b {
		t.Errorf("newest first, one server: %+v", list)
	}
	if all, _ := s.ListRecoveries(""); len(all) != 3 {
		t.Errorf("all: %d", len(all))
	}
	if r, err := s.RecoveryForRun(7); err != nil || r.ID != a {
		t.Errorf("by run: %+v %v", r, err)
	}
	if _, err := s.RecoveryForRun(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: %v", err)
	}
	if olds, _ := s.RecoveriesOlderThan(time.Now().Add(-7 * 24 * time.Hour)); len(olds) != 1 || olds[0].ID != a {
		t.Errorf("older: %+v", olds)
	}
	if err := s.DeleteRecovery(a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRecovery(a); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}

	r1, _ := s.StartRun("pg-main", KindRecover, "x")
	_ = s.FinishRun(r1, 0, "", "")
	r2, _ := s.StartRun("pg-main", KindRecover, "x")
	_ = s.FinishRun(r2, 1, "", "")
	if n, _ := s.CountRuns(KindRecover); n["pg-main"] != 1 {
		t.Errorf("successful recover runs: %v", n)
	}
}

func TestDrills(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	a, err := s.AddDrill(Drill{Server: "pg-main", BackupID: "20261019T020001", StartedAt: t0, FinishedAt: t0.Add(40 * time.Second), OK: true, Details: `{"summary":"ok"}`, RunID: 5, ReportPath: "/r/pg-main/reports/a.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.AddDrill(Drill{Server: "pg-main", BackupID: "20261020T020001", StartedAt: t0.Add(24 * time.Hour), FinishedAt: t0.Add(24*time.Hour + time.Second), StepFailed: "start", RunID: 9})
	if _, err := s.AddDrill(Drill{Server: "other", StartedAt: t0, FinishedAt: t0}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDrill(a)
	if err != nil || !got.OK || got.StepFailed != "" || got.Details != `{"summary":"ok"}` || !got.FinishedAt.Equal(t0.Add(40*time.Second)) || got.ReportPath == "" {
		t.Fatalf("%+v %v", got, err)
	}
	if d, _ := s.GetDrill(b); d.OK || d.StepFailed != "start" || d.Details != "{}" {
		t.Errorf("failed drill: %+v", d)
	}
	list, _ := s.ListDrills("pg-main", 0)
	if len(list) != 2 || list[0].ID != b {
		t.Errorf("newest first, one server: %+v", list)
	}
	if all, _ := s.ListDrills("", 1); len(all) != 1 {
		t.Errorf("limit: %d", len(all))
	}
	if d, err := s.DrillForRun(5); err != nil || d.ID != a {
		t.Errorf("by run: %+v %v", d, err)
	}
	if err := s.UpdateDrillDetails(a, `{"data_dir":""}`, "/r/x.pdf"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDrill(a); d.Details != `{"data_dir":""}` || d.ReportPath != "/r/x.pdf" {
		t.Errorf("update: %+v", d)
	}
	if _, err := s.GetDrill(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if err := s.UpdateDrillDetails(999, "{}", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
}

func TestReplaceSnapshot(t *testing.T) {
	s := newStore(t)
	if _, err := s.StartRun("old", KindBackup, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAudit("x", "old", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(Session{ID: "keep", User: "demo", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	code := 0
	sn := Snapshot{
		Runs:       []Run{{ID: 7, Server: "a", Kind: KindBackup, StartedAt: at, FinishedAt: &at, ExitCode: &code, Actor: "scheduler"}, {ID: 8, Server: "a", Kind: KindCheck, StartedAt: at.Add(time.Hour)}},
		Audit:      []AuditEntry{{ID: 1, At: at, Actor: "maria", Action: "login"}},
		Drills:     []Drill{{ID: 3, Server: "a", StartedAt: at, FinishedAt: at, OK: true, RunID: 7}},
		Recoveries: []Recovery{{ID: 2, Server: "a", BackupID: "b", Path: "/x", CreatedAt: at, RunID: 7}},
		Alerts:     []AlertSent{{ID: 1, At: at, Channel: "telegram", Kind: "level", OK: true}},
	}
	for i := 0; i < 2; i++ { // replacing twice gives the same content
		if err := s.ReplaceSnapshot(sn); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := s.ListRuns("", 10)
	if len(runs) != 2 || runs[1].ID != 7 || runs[0].FinishedAt != nil || runs[1].ExitCode == nil {
		t.Errorf("runs %+v", runs)
	}
	if a, _ := s.ListAudit(10); len(a) != 1 || a[0].Actor != "maria" {
		t.Errorf("audit %+v", a)
	}
	if d, err := s.GetDrill(3); err != nil || !d.OK || d.RunID != 7 {
		t.Errorf("drill %+v %v", d, err)
	}
	if r, _ := s.ListRecoveries("a"); len(r) != 1 || r[0].ID != 2 {
		t.Errorf("recoveries %+v", r)
	}
	if a, _ := s.ListAlerts(10); len(a) != 1 || !a[0].OK {
		t.Errorf("alerts %+v", a)
	}
	if _, err := s.GetSession("keep"); err != nil {
		t.Errorf("a session was lost: %v", err)
	}
	for i := 1; i <= 5; i++ {
		_ = s.AddAudit("x", "more", "", "")
	}
	if err := s.TrimAudit(2); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.ListAudit(10); len(a) != 2 {
		t.Errorf("after TrimAudit: %d lines", len(a))
	}
}

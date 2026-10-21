package app_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

func startRecovery(t *testing.T, e *apptest.Env, in app.RecoveryInput) *app.RunView {
	t.Helper()
	id, err := e.App.StartRecovery(context.Background(), "gilles", "pg-main", in)
	if err != nil {
		t.Fatal(err)
	}
	return apptest.WaitRun(t, e.App, id)
}

func tarNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
}

func TestRecoveryWithArchive(t *testing.T) {
	e := apptest.New(t)
	run := startRecovery(t, e, app.RecoveryInput{BackupID: "20261019T020001", Target: "latest", Archive: true})
	if run.Status != app.RunOK || run.Kind != "recover" || !strings.Contains(run.Stdout, "successfully prepared for recovery") {
		t.Fatalf("run: %+v", run)
	}

	list, err := e.App.ListRecoveries("pg-main")
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	r := list[0]
	root := filepath.Join(e.Dir, "recoveries", "pg-main")
	if filepath.Dir(r.Path) != root || !strings.HasPrefix(filepath.Base(r.Path), "20261019T020001-") {
		t.Errorf("path = %s", r.Path)
	}
	if r.ArchivePath != r.Path+".tar.gz" || r.ArchiveBytes <= 0 || r.SizeBytes < 4096 || !r.Downloadable || r.RunID != run.ID || r.Target != "latest" || r.TargetTime != "" || r.Actor != "gilles" {
		t.Errorf("view: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(r.Path, "PG_VERSION")); err != nil {
		t.Errorf("restored directory: %v", err)
	}
	names := tarNames(t, r.ArchivePath)
	base := filepath.Base(r.Path)
	for _, want := range []string{base + "/", base + "/PG_VERSION", base + "/base/1/1259", base + "/postgresql.auto.conf"} {
		if !names[want] {
			t.Errorf("archive lacks %s: %v", want, names)
		}
	}
	if got, ok := e.App.RecoveryOfRun(run.ID); !ok || got.ID != r.ID {
		t.Errorf("by run: %+v %v", got, ok)
	}

	// The exact command: local restore, no target, no remote command.
	want := "recover pg-main 20261019T020001 " + r.Path
	if calls := apptest.Calls(t, e); !strings.Contains(calls, want+"\n") || strings.Contains(calls, "remote-ssh") || strings.Contains(calls, "--target") {
		t.Errorf("calls:\n%s\nwant a line %q", calls, want)
	}

	// Download: audited, path inside the root.
	path, _, err := e.App.RecoveryArchive("gilles", r.ID)
	if err != nil || path != r.ArchivePath {
		t.Errorf("archive: %q %v", path, err)
	}
	audit, _ := e.App.Audit(100)
	seen := map[string]bool{}
	for _, a := range audit {
		seen[a.Action] = true
	}
	if !seen["recover.start"] || !seen["recovery.download"] {
		t.Errorf("audit: %v", seen)
	}

	// Deleting removes the directory, the archive and the record.
	if err := e.App.DeleteRecovery("gilles", r.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{r.Path, r.ArchivePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must be gone: %v", p, err)
		}
	}
	if list, _ := e.App.ListRecoveries("pg-main"); len(list) != 0 {
		t.Errorf("still listed: %+v", list)
	}
	if err := e.App.DeleteRecovery("gilles", r.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestRecoveryTargets(t *testing.T) {
	e := apptest.New(t)

	// A point in time after the end of the backup is passed to Barman.
	run := startRecovery(t, e, app.RecoveryInput{BackupID: "20261019T020001", Target: "time", TargetTime: "2026-10-20 12:00:00+02:00"})
	if run.Status != app.RunOK {
		t.Fatalf("%+v", run)
	}
	list, _ := e.App.ListRecoveries("pg-main")
	if len(list) != 1 || list[0].Target != "time" || list[0].TargetTime != "2026-10-20 12:00:00+02:00" || list[0].ArchivePath != "" || list[0].Downloadable {
		t.Fatalf("%+v", list)
	}
	if calls := apptest.Calls(t, e); !strings.Contains(calls, "recover --target-time 2026-10-20 12:00:00+02:00 pg-main 20261019T020001 ") {
		t.Errorf("calls:\n%s", calls)
	}
	if _, _, err := e.App.RecoveryArchive("gilles", list[0].ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("no archive: %v", err)
	}

	// The datetime-local form (no zone, no seconds) and RFC 3339 are accepted.
	for _, tt := range []string{"2026-10-20T12:00", "2026-10-20T12:00:00Z"} {
		if run := startRecovery(t, e, app.RecoveryInput{BackupID: "20261019T020001", Target: "time", TargetTime: tt}); run.Status != app.RunOK {
			t.Errorf("%s: %+v", tt, run)
		}
	}

	// End of the backup.
	if run := startRecovery(t, e, app.RecoveryInput{BackupID: "20261019T020001", Target: "backup_end"}); run.Status != app.RunOK {
		t.Fatalf("%+v", run)
	}
	if calls := apptest.Calls(t, e); !strings.Contains(calls, "recover --target-immediate pg-main 20261019T020001 ") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestRecoveryValidation(t *testing.T) {
	e := apptest.New(t)
	ctx := context.Background()
	problems := func(in app.RecoveryInput) string {
		t.Helper()
		_, err := e.App.StartRecovery(ctx, "gilles", "pg-main", in)
		var pe *app.ProblemsError
		if !errors.As(err, &pe) {
			t.Fatalf("%+v: want problems, got %v", in, err)
		}
		return pe.Error()
	}
	if msg := problems(app.RecoveryInput{BackupID: "20261019T020001", Target: "time", TargetTime: "2026-10-01 00:00:00+00:00"}); !strings.Contains(msg, "before the end of backup 20261019T020001") {
		t.Errorf("early target: %s", msg)
	}
	if msg := problems(app.RecoveryInput{BackupID: "20261019T020001", Target: "time", TargetTime: "yesterday"}); !strings.Contains(msg, "not a date and time") {
		t.Errorf("bad time: %s", msg)
	}
	if msg := problems(app.RecoveryInput{BackupID: "20261019T020001", Target: "time"}); !strings.Contains(msg, "Point in time") {
		t.Errorf("missing time: %s", msg)
	}
	if msg := problems(app.RecoveryInput{BackupID: "../../etc", Target: "latest"}); !strings.Contains(msg, "Choose the backup") {
		t.Errorf("bad id: %s", msg)
	}
	if msg := problems(app.RecoveryInput{BackupID: "19990101T000000", Target: "latest"}); !strings.Contains(msg, "not in the list") {
		t.Errorf("unknown backup: %s", msg)
	}
	if msg := problems(app.RecoveryInput{BackupID: "20261019T020001", Target: "tomorrow"}); !strings.Contains(msg, "unknown") {
		t.Errorf("bad target: %s", msg)
	}
	if _, err := e.App.StartRecovery(ctx, "gilles", "../..", app.RecoveryInput{BackupID: "20261019T020001"}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a server name is never a path: %v", err)
	}
	if _, err := e.App.StartRecovery(ctx, "gilles", "nope", app.RecoveryInput{BackupID: "20261019T020001"}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("unknown server: %v", err)
	}
	// Only a complete backup can be restored.
	_, err := e.App.StartRecovery(ctx, "gilles", "pg-rsync", app.RecoveryInput{BackupID: "20261020T230001"})
	var pe *app.ProblemsError
	if !errors.As(err, &pe) || !strings.Contains(pe.Error(), "only a complete backup") {
		t.Errorf("failed backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "recoveries")); !os.IsNotExist(err) {
		t.Error("nothing may be created for a refused request")
	}
}

func TestRecoveryFailureLeavesNothingBehind(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_BARMAN_RECOVER_FAIL", "1")
	run := startRecovery(t, e, app.RecoveryInput{BackupID: "20261019T020001", Target: "latest", Archive: true})
	if run.Status != app.RunFailed || !strings.Contains(run.Stderr, "No space left on device") {
		t.Fatalf("run: %+v", run)
	}
	if list, _ := e.App.ListRecoveries(""); len(list) != 0 {
		t.Errorf("no record for a failed restore: %+v", list)
	}
	left, _ := filepath.Glob(filepath.Join(e.Dir, "recoveries", "pg-main", "*"))
	if len(left) != 0 {
		t.Errorf("half restore left behind: %v", left)
	}
}

func TestRecoveryOnePerServer(t *testing.T) {
	e := apptest.New(t)
	t.Setenv("FAKE_BARMAN_RECOVER_DELAY", "1")
	id, err := e.App.StartRecovery(context.Background(), "gilles", "pg-main", app.RecoveryInput{BackupID: "20261019T020001"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.App.StartRecovery(context.Background(), "gilles", "pg-main", app.RecoveryInput{BackupID: "20261018T020001"})
	var ce *app.ConflictError
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "already running") {
		t.Errorf("second restore: %v", err)
	}
	apptest.WaitRun(t, e.App, id)
}

func TestRecoveryDownloadLimit(t *testing.T) {
	e := apptest.New(t)
	run := startRecovery(t, e, app.RecoveryInput{BackupID: "20261019T020001", Archive: true})
	r, _ := e.App.RecoveryOfRun(run.ID)
	// The limit is read from pgcellar.yaml: lower it to one byte.
	rewriteConfig(t, e, "max_download_bytes: 2147483648", "max_download_bytes: 1")
	waitConfig(t, e, func() bool { return e.App.Config().Recoveries.MaxDownloadBytes == 1 })
	if _, _, err := e.App.RecoveryArchive("gilles", r.ID); !errors.Is(err, app.ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	if v, _ := e.App.GetRecovery(r.ID); v.Downloadable {
		t.Error("no download link above the limit")
	}
}

func rewriteConfig(t *testing.T, e *apptest.Env, old, new string) {
	t.Helper()
	path := filepath.Join(e.Dir, "pgcellar.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%q not in the config:\n%s", old, b)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)
}

func waitConfig(t *testing.T, e *apptest.Env, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the configuration was not reloaded")
}

func TestDeleteRecoveryRefusesPathsOutsideTheDirectory(t *testing.T) {
	e := apptest.New(t)
	victim := filepath.Join(t.TempDir(), "keep", "me")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	id, err := e.App.Store.AddRecovery(store.Recovery{Server: "pg-main", BackupID: "20261019T020001", Path: victim})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.App.DeleteRecovery("gilles", id); err == nil || !strings.Contains(err.Error(), "recoveries directory") {
		t.Fatalf("delete outside the directory: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the foreign directory was removed: %v", err)
	}
	if v, err := e.App.GetRecovery(id); err != nil || v == nil {
		t.Errorf("the record must stay: %v", err)
	}
	if _, _, err := e.App.RecoveryArchive("gilles", id); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("no archive: %v", err)
	}
}

func TestPurgeRecoveries(t *testing.T) {
	e := apptest.New(t) // keep_days is 7 by default
	root := filepath.Join(e.Dir, "recoveries", "pg-main")
	mk := func(name string, age time.Duration) int64 {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+".tar.gz", []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		id, err := e.App.Store.AddRecovery(store.Recovery{Server: "pg-main", BackupID: "20261019T020001", Path: dir, ArchivePath: dir + ".tar.gz", CreatedAt: time.Now().Add(-age)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldID := mk("20261001T020001-20261001T100000", 8*24*time.Hour)
	newID := mk("20261019T020001-20261019T100000", 24*time.Hour)

	if n := e.App.PurgeRecoveries(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	if _, err := e.App.GetRecovery(oldID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("old record: %v", err)
	}
	if _, err := e.App.GetRecovery(newID); err != nil {
		t.Errorf("recent record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "20261001T020001-20261001T100000")); !os.IsNotExist(err) {
		t.Error("old directory still there")
	}
	if _, err := os.Stat(filepath.Join(root, "20261001T020001-20261001T100000.tar.gz")); !os.IsNotExist(err) {
		t.Error("old archive still there")
	}
	if _, err := os.Stat(filepath.Join(root, "20261019T020001-20261019T100000")); err != nil {
		t.Errorf("recent directory removed: %v", err)
	}
}

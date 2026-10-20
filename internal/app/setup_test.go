package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/barman"
)

func psqlCalls(t *testing.T, e *apptest.Env) int {
	t.Helper()
	b, _ := os.ReadFile(e.PsqlLog)
	return strings.Count(string(b), "\n")
}

// changeFixture gives the fake psql its own copy of the pg-main answer with
// old replaced by repl, so tests never touch the shared fixtures.
func changeFixture(t *testing.T, old, repl string) {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.ReadFile(filepath.Join("..", "barman", "testdata", "fixtures", "psql-pg-main.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(orig), old, repl, 1)
	if changed == string(orig) {
		t.Fatalf("%q not found in the fixture", old)
	}
	if err := os.WriteFile(filepath.Join(dir, "psql-pg-main.tsv"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PSQL_FIXTURES", dir)
}

func setupLevels(st app.ServerState) map[string]string {
	m := map[string]string{}
	for _, c := range st.Setup {
		m[c.Key] = c.Level
	}
	return m
}

func TestSetupIsReadAndCached(t *testing.T) {
	e := apptest.New(t)
	ctx := t.Context()
	st, ok := e.App.State("pg-main")
	if !ok {
		t.Fatal("no state for pg-main")
	}
	got := setupLevels(st)
	if st.SetupLevel != barman.SetupWarning || st.SetupIssues != 1 || got["archive_timeout"] != barman.SetupWarning || got["wal_level"] != barman.SetupOK {
		t.Fatalf("setup = %v level=%s issues=%d", got, st.SetupLevel, st.SetupIssues)
	}
	if got[barman.SetupKeyStreaming] != barman.SetupOK {
		t.Errorf("the lines derived from barman check are missing: %v", got)
	}
	// A warning does not change the health of the server.
	if st.Level != app.LevelOK {
		t.Errorf("health level %q, a setup warning must not change it", st.Level)
	}
	if n := psqlCalls(t, e); n != 1 {
		t.Fatalf("%d psql calls after the first refresh, want 1", n)
	}

	// Refreshes inside the interval do not query PostgreSQL again.
	for i := 0; i < 3; i++ {
		e.App.RefreshStatuses(ctx)
	}
	if n := psqlCalls(t, e); n != 1 {
		t.Errorf("%d psql calls after 3 more refreshes, want 1", n)
	}
	st, _ = e.App.State("pg-main")
	if st.SetupLevel != barman.SetupWarning {
		t.Errorf("the cached setup was lost: %q", st.SetupLevel)
	}

	// Past the interval it is read again.
	e.App.Now = func() time.Time { return time.Date(2026, 10, 20, 3, 5, 0, 0, time.Local) }
	e.App.RefreshStatuses(ctx)
	if n := psqlCalls(t, e); n != 2 {
		t.Errorf("%d psql calls after 35 minutes, want 2", n)
	}

	// "Run check now" reads it at once.
	if _, err := e.App.Check(ctx, "tester", "pg-main"); err != nil {
		t.Fatal(err)
	}
	if n := psqlCalls(t, e); n != 3 {
		t.Errorf("%d psql calls after Run check now, want 3", n)
	}
}

func TestSetupSettingsFixedOnTheServerAreSeenAfterACheck(t *testing.T) {
	e := apptest.New(t)
	changeFixture(t, "archive_timeout\t0\t", "archive_timeout\t3600\t")
	if _, err := e.App.Check(t.Context(), "tester", "pg-main"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.App.State("pg-main")
	if st.SetupLevel != barman.SetupOK || st.SetupIssues != 0 {
		t.Errorf("level %q issues %d after the fix", st.SetupLevel, st.SetupIssues)
	}
}

// An unreachable server gives one error line, retried sooner than the
// interval, and does not change the health level by itself.
func TestSetupUnreadableServer(t *testing.T) {
	e := apptest.New(t)
	conf := "[pg-rsync]\nconninfo = host=nowhere.example.com user=barman dbname=postgres\nbackup_method = rsync\nssh_command = ssh x\n"
	if err := os.WriteFile(filepath.Join(e.BarmanDir, "pg-rsync.conf"), []byte(conf), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	e.App.RefreshStatuses(ctx)
	st, _ := e.App.State("pg-rsync")
	if len(st.Setup) != 1 || st.Setup[0].Key != barman.SetupKeyUnreadable || st.SetupLevel != barman.SetupError {
		t.Fatalf("setup = %+v", st.Setup)
	}
	if !strings.HasPrefix(st.Setup[0].Why, "Could not read the server settings: ") {
		t.Errorf("why = %q", st.Setup[0].Why)
	}
	for _, p := range st.Problems {
		if strings.Contains(p, "Server setup") {
			t.Errorf("an unreadable setup must not add a health problem: %q", p)
		}
	}
	before := psqlCalls(t, e)
	e.App.RefreshStatuses(ctx) // inside the 5 minute retry
	if psqlCalls(t, e) != before {
		t.Error("retried too early")
	}
	e.App.Now = func() time.Time { return time.Date(2026, 10, 20, 2, 37, 0, 0, time.Local) }
	e.App.RefreshStatuses(ctx) // 7 minutes later
	if psqlCalls(t, e) != before+1 {
		t.Error("an unreadable server should be retried after 5 minutes")
	}
}

func TestSetupErrorFailsTheServer(t *testing.T) {
	e := apptest.New(t)
	changeFixture(t, "wal_level\treplica", "wal_level\tminimal")
	if _, err := e.App.Check(t.Context(), "tester", "pg-main"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.App.State("pg-main")
	if st.SetupLevel != barman.SetupError || st.Level != app.LevelFail {
		t.Fatalf("setup %q health %q", st.SetupLevel, st.Level)
	}
	v, err := e.App.Setup("pg-main")
	if err != nil || v.Level != "error" || len(v.Checks) == 0 {
		t.Errorf("Setup = %+v %v", v, err)
	}
	if _, err := e.App.Setup("nope"); err == nil {
		t.Error("unknown server should be refused")
	}
}

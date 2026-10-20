package barman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestEngine(t *testing.T) (*CLIEngine, *[]AuditEntry) {
	t.Helper()
	useFakeBarman(t)
	var entries []AuditEntry
	r := CLIRunner{Binary: "fake-barman", Audit: func(e AuditEntry) { entries = append(entries, e) }}
	return NewEngine(r), &entries
}

func TestEngineListServers(t *testing.T) {
	e, entries := newTestEngine(t)
	names, err := e.ListServers(context.Background())
	if err != nil || len(names) != 3 {
		t.Fatalf("%v %v", names, err)
	}
	if got := strings.Join((*entries)[0].Args, " "); got != "--format json list-servers" {
		t.Errorf("args = %q", got)
	}
}

func TestEngineCheck(t *testing.T) {
	e, entries := newTestEngine(t)
	res, err := e.Check(context.Background(), "broken")
	if err != nil {
		t.Fatalf("failed checks must not be an error: %v", err)
	}
	failed := 0
	for _, c := range res {
		if !c.OK {
			failed++
		}
	}
	if failed != 6 || (*entries)[0].ExitCode != 1 {
		t.Errorf("failed=%d exit=%d", failed, (*entries)[0].ExitCode)
	}
	if _, err := e.Check(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "Unknown server 'nope'") {
		t.Errorf("unknown server: %v", err)
	}
	good, _ := e.Check(context.Background(), "pg-main")
	for _, c := range good {
		if !c.OK {
			t.Errorf("pg-main check failed: %+v", c)
		}
	}
}

func TestEngineBackups(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()
	list, err := e.ListBackups(ctx, "pg-main")
	if err != nil || len(list) != 3 || list[0].ID != "20261018T020001" {
		t.Fatalf("%v %v", list, err)
	}
	b, err := e.ShowBackup(ctx, "pg-main", "latest")
	if err != nil || b.ID != "20261020T020001" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := e.ShowBackup(ctx, "pg-main", "19990101T000000"); err == nil {
		t.Error("unknown backup must fail")
	}
}

func TestEngineStatusHealthy(t *testing.T) {
	e, _ := newTestEngine(t)
	st, err := e.Status(context.Background(), "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Healthy || len(st.Problems) != 0 {
		t.Errorf("problems: %v", st.Problems)
	}
	if st.BackupCount != 3 || st.LastBackup == nil || st.LastBackup.ID != "20261020T020001" || st.LastBackup.PGVersion != "16.4" {
		t.Errorf("status = %+v last=%+v", st, st.LastBackup)
	}
	if st.LastWAL != "000000010000000000000007" || st.LastWALTime.Unix() != 1792455029 {
		t.Errorf("last WAL from diagnose: %q %v", st.LastWAL, st.LastWALTime)
	}
	if st.LastWALTime.IsZero() || len(st.Checks) == 0 {
		t.Errorf("wal time / checks missing: %+v", st)
	}
}

func TestEngineStatusBroken(t *testing.T) {
	e, _ := newTestEngine(t)
	st, err := e.Status(context.Background(), "broken")
	if err != nil {
		t.Fatal(err)
	}
	if st.Healthy {
		t.Error("broken server must not be healthy")
	}
	joined := strings.Join(st.Problems, "\n")
	for _, want := range []string{"PostgreSQL failed", "SSH connection failed", "WAL receiver failed", "barman cron", "most recent backup attempt (20261017T020001) failed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems miss %q:\n%s", want, joined)
		}
	}
	if st.LastBackup != nil {
		t.Errorf("no completed backup expected: %+v", st.LastBackup)
	}
}

func TestEngineStatusRsyncFailedLastBackup(t *testing.T) {
	e, _ := newTestEngine(t)
	st, err := e.Status(context.Background(), "pg-rsync")
	if err != nil {
		t.Fatal(err)
	}
	if st.Healthy || st.LastBackup == nil || st.LastBackup.ID != "20261019T230001" {
		t.Errorf("status = %+v", st)
	}
	if len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "20261020T230001") {
		t.Errorf("problems = %v", st.Problems)
	}
}

func TestEngineStatusUnknownServer(t *testing.T) {
	e, _ := newTestEngine(t)
	st, err := e.Status(context.Background(), "nope")
	if err != nil {
		t.Fatal(err)
	}
	if st.Healthy || len(st.Problems) < 3 {
		t.Errorf("problems = %v", st.Problems)
	}
}

func TestEngineActionsAuditedArgs(t *testing.T) {
	e, entries := newTestEngine(t)
	ctx := context.Background()
	if err := e.Backup(ctx, "pg-main"); err != nil {
		t.Fatal(err)
	}
	if err := e.SwitchWAL(ctx, "pg-main"); err != nil {
		t.Fatal(err)
	}
	if err := e.Cron(ctx); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, en := range *entries {
		got = append(got, strings.Join(en.Args, " "))
	}
	want := []string{"backup pg-main", "switch-wal --force --archive pg-main", "cron"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("audited args = %v", got)
	}
	if err := e.Backup(ctx, "broken"); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("backup on broken: %v", err)
	}
}

func TestFakeBarmanLog(t *testing.T) {
	e, _ := newTestEngine(t)
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_BARMAN_LOG", log)
	if _, err := e.ListBackups(context.Background(), "pg-main"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if strings.TrimSpace(string(data)) != "--format json list-backups pg-main" {
		t.Errorf("log = %q", data)
	}
}

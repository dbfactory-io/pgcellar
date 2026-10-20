// Package apptest builds a complete App for tests: temporary directories,
// the example Barman files, the fake Barman on PATH, three web users and
// three API keys (one per role).
package apptest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
)

// Env is what a test gets.
type Env struct {
	App       *app.App
	Dir       string            // holds pgcellar.yaml
	BarmanDir string            // the barman.d copy
	CallLog   string            // FAKE_BARMAN_LOG
	PsqlLog   string            // FAKE_PSQL_LOG
	Home      string            // barman_home of the test barman.conf
	Keys      map[string]string // role -> clear API key
}

// Password of every test user.
const Password = "secret-pass"

func init() {
	// Fast bcrypt for every test that builds an App.
	config.BcryptCost = 4
}

// bcrypt is slow on purpose: hash the test password once per process.
var (
	hashOnce sync.Once
	hash     string
	hashErr  error
)

// repoRoot finds the repository from this file's location.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// New creates the environment and refreshes the status cache once.
func New(t *testing.T) *Env {
	t.Helper()
	root := repoRoot()
	testdata := filepath.Join(root, "internal", "barman", "testdata")
	t.Setenv("PATH", testdata+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	logFile := filepath.Join(dir, "barman-calls.log")
	t.Setenv("FAKE_BARMAN_LOG", logFile)
	psqlLog := filepath.Join(t.TempDir(), "psql-calls.log")
	t.Setenv("FAKE_PSQL_LOG", psqlLog)

	barmanDir := filepath.Join(dir, "barman.d")
	if err := os.MkdirAll(barmanDir, 0o750); err != nil {
		t.Fatal(err)
	}
	examples, _ := filepath.Glob(filepath.Join(root, "deploy", "barman.d.example", "*.conf"))
	for _, f := range examples {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(barmanDir, filepath.Base(f)), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	hashOnce.Do(func() { hash, hashErr = config.HashPassword(Password) })
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	cfg := config.Defaults()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Recoveries.Dir = filepath.Join(dir, "recoveries")
	cfg.Barman.Binary = "fake-barman"
	cfg.Barman.User = ""
	cfg.Barman.ConfigDir = barmanDir
	cfg.Barman.ConfigFile = filepath.Join(dir, "barman.conf")
	home := filepath.Join(dir, "barman-home")
	for _, srv := range []string{"pg-main", "pg-rsync", "broken"} {
		wals := filepath.Join(home, srv, "wals")
		if err := os.MkdirAll(wals, 0o750); err != nil {
			t.Fatal(err)
		}
		line := ""
		if srv == "pg-main" { // last WAL archived at 02:10 on 2026-10-20
			line = fmt.Sprintf("000000010000000000000007\t16777216\t%d.0\tgzip\tNone\n", time.Date(2026, 10, 20, 2, 10, 29, 0, time.Local).Unix())
		}
		if err := os.WriteFile(filepath.Join(wals, srv+"-xlog.db"), []byte(line), 0o640); err != nil {
			t.Fatal(err)
		}
		if srv == "pg-main" { // the receiver wrote into its segment at 02:25
			streaming := filepath.Join(home, srv, "streaming")
			if err := os.MkdirAll(streaming, 0o750); err != nil {
				t.Fatal(err)
			}
			part := filepath.Join(streaming, "000000010000000000000008.partial")
			if err := os.WriteFile(part, []byte("x"), 0o640); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 10, 20, 2, 25, 0, 0, time.Local)
			if err := os.Chtimes(part, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(cfg.Barman.ConfigFile, []byte("[barman]\nbarman_home = "+home+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg.Alerts.MaxBackupAge = 26 * time.Hour
	cfg.Alerts.MaxWALAge = time.Hour
	cfg.Servers = []config.Server{{Name: "pg-main", Label: "Main", Schedule: config.Schedule{Backup: "daily 02:00", Enabled: true}}}
	keys := map[string]string{}
	for _, role := range []string{"admin", "operator", "viewer"} {
		cfg.Users = append(cfg.Users, config.User{Name: role + "-user", Role: role, PasswordHash: hash})
		clear, h := config.NewAPIKey()
		keys[role] = clear
		cfg.APIKeys = append(cfg.APIKeys, config.APIKey{Name: role + "-key", Role: role, KeyHash: h})
	}
	path := filepath.Join(dir, "pgcellar.yaml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	a, err := app.Load(path, false)
	if err != nil {
		t.Fatal(err)
	}
	a.Version = "test"
	a.SetupTools = barman.SetupTools{Psql: "fake-psql", Basebackup: "fake-pg_basebackup"}
	fixed := time.Date(2026, 10, 20, 2, 30, 0, 0, time.Local)
	a.Now = func() time.Time { return fixed }
	t.Cleanup(func() { _ = a.Close() })
	a.RefreshStatuses(context.Background())
	return &Env{App: a, Dir: dir, BarmanDir: barmanDir, CallLog: logFile, PsqlLog: psqlLog, Home: home, Keys: keys}
}

// WaitRun waits until a run has finished and returns it.
func WaitRun(t *testing.T, a *app.App, id int64) *app.RunView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, err := a.Run(id)
		if err != nil {
			t.Fatalf("run %d: %v", id, err)
		}
		if r.Status != app.RunRunning {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %d did not finish", id)
	return nil
}

// Calls returns the lines the fake Barman logged.
func Calls(t *testing.T, e *Env) string {
	t.Helper()
	b, err := os.ReadFile(e.CallLog)
	if err != nil {
		return ""
	}
	return fmt.Sprint(string(b))
}

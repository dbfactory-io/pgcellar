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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/license"
	"github.com/dbfactory-io/pgcellar/internal/tztest"
)

// Env is what a test gets.
type Env struct {
	App       *app.App
	Dir       string            // holds pgcellar.yaml
	BarmanDir string            // the barman.d copy
	CallLog   string            // FAKE_BARMAN_LOG
	PsqlLog   string            // FAKE_PSQL_LOG
	Home      string            // barman_home of the test barman.conf
	SSHHome   string            // home whose .ssh holds the test key pair
	SSHLog    string            // FAKE_SSH_LOG
	ResticLog string            // FAKE_RESTIC_LOG
	Keys      map[string]string // role -> clear API key
	// LicenseKey issues a licence key that the App of this environment accepts.
	LicenseKey func(edition string, servers int, expires string) string
}

// fixtureZone is the offset of the Barman fixtures (Brussels, summer time on
// 2026-10-20). The dates built here are instants, not wall-clock times of the
// machine running the tests.
var fixtureZone = time.FixedZone("CEST", 2*60*60)

// Frozen is the instant the clock of every test App is stopped at:
// 2026-10-20 02:30 in Brussels, twenty minutes after the last pg-main backup.
var Frozen = time.Date(2026, 10, 20, 2, 30, 0, 0, fixtureZone)

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
func New(t *testing.T) *Env { return NewWith(t, nil) }

// NewWith is New with a last chance to change the configuration before it is
// written and loaded.
//
// The package of the test must pin the time zone with tztest.Main: the
// fixtures and the frozen clock are instants (2026-10-20 02:30 in Brussels),
// and wall-clock settings (schedules, daily summary) are read in time.Local.
func NewWith(t *testing.T, mutate func(*config.Config)) *Env {
	t.Helper()
	if !tztest.Pinned() {
		t.Fatal("time zone not pinned: add func TestMain(m *testing.M) { tztest.Main(m) } to this package")
	}
	root := repoRoot()
	testdata := filepath.Join(root, "internal", "barman", "testdata")
	t.Setenv("PATH", testdata+string(os.PathListSeparator)+os.Getenv("PATH"))

	// An "ssh" that never connects (see testdata/fake-ssh), first on PATH.
	sshDir := t.TempDir()
	if err := os.Symlink(filepath.Join(testdata, "fake-ssh"), filepath.Join(sshDir, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	sshLog := filepath.Join(t.TempDir(), "ssh-calls.log")
	t.Setenv("FAKE_SSH_LOG", sshLog)

	dir := t.TempDir()
	logFile := filepath.Join(dir, "barman-calls.log")
	t.Setenv("FAKE_BARMAN_LOG", logFile)
	psqlLog := filepath.Join(t.TempDir(), "psql-calls.log")
	t.Setenv("FAKE_PSQL_LOG", psqlLog)
	// The fake restic keeps its repositories here; FAKE_RESTIC_LOG lists the calls.
	t.Setenv("FAKE_RESTIC_ROOT", t.TempDir())
	resticLog := filepath.Join(t.TempDir(), "restic-calls.log")
	t.Setenv("FAKE_RESTIC_LOG", resticLog)

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
			line = pgMainCatalogue()
			writePgMainMeta(t, filepath.Join(home, srv, "meta"))
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
			at := time.Date(2026, 10, 20, 2, 25, 0, 0, fixtureZone)
			if err := os.Chtimes(part, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(cfg.Barman.ConfigFile, []byte("[barman]\nbarman_home = "+home+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg.Alerts.MaxBackupAge = 26 * time.Hour
	// The tests must not depend on how full the disk of the machine is.
	cfg.Alerts.MinFreePercent = 1
	cfg.Alerts.MaxWALAge = time.Hour
	cfg.Servers = []config.Server{{Name: "pg-main", Label: "Main", Schedule: config.Schedule{Backup: "daily 02:00", Enabled: config.Ptr(true)}}}
	keys := map[string]string{}
	for _, role := range []string{"admin", "operator", "viewer"} {
		cfg.Users = append(cfg.Users, config.User{Name: role + "-user", Role: role, PasswordHash: hash})
		clear, h := config.NewAPIKey()
		keys[role] = clear
		cfg.APIKeys = append(cfg.APIKeys, config.APIKey{Name: role + "-key", Role: role, KeyHash: h})
	}
	// A test licence (Business, unlimited, valid far ahead) signed by a throw-away
	// key, so that tests are not limited to the 2 servers of the Free edition.
	licPub, licPriv, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	issue := func(edition string, servers int, expires string) string {
		k, err := license.Sign(licPriv, license.Payload{ID: "test", Edition: edition, Licensee: "Test", Servers: servers, Issued: "2026-01-01", Expires: expires})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	cfg.License.Key = issue(license.Business, 0, "2099-12-31")
	if mutate != nil {
		mutate(cfg)
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
	a.LicensePublicKey = licPub
	a.SSHHome = filepath.Join(dir, "barman-home-dir")
	a.SetupTools = barman.SetupTools{Psql: "fake-psql", Basebackup: "fake-pg_basebackup"}
	a.DrillTools = app.DrillTools{PgCtl: "fake-pg_ctl", PgIsReady: "fake-pg_isready", PgChecksums: "fake-pg_checksums"}
	a.DrillPollInterval = 20 * time.Millisecond
	a.VaultTools = app.VaultTools{Restic: "fake-restic"}
	// Same instant in every zone, shown in the process zone like time.Now().
	a.Now = func() time.Time { return Frozen.In(time.Local) }
	t.Cleanup(func() { _ = a.Close() })
	a.RefreshStatuses(context.Background())
	return &Env{App: a, Dir: dir, BarmanDir: barmanDir, CallLog: logFile, PsqlLog: psqlLog, Home: home, SSHHome: a.SSHHome, SSHLog: sshLog, ResticLog: resticLog, Keys: keys, LicenseKey: issue}
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

// pgMainCatalogue is the WAL catalogue of pg-main: seven segments of 16 MiB on
// timeline 1, the last one (07) archived at 02:10 on 2026-10-20, the first
// ones around the three nightly backups of the fixtures.
func pgMainCatalogue() string {
	at := func(day, h, m int) int64 { return time.Date(2026, 10, day, h, m, 29, 0, fixtureZone).Unix() }
	stamps := []int64{at(18, 2, 5), at(18, 14, 0), at(19, 2, 4), at(19, 14, 0), at(20, 0, 30), at(20, 1, 40), at(20, 2, 10)}
	var b strings.Builder
	for i, ts := range stamps {
		fmt.Fprintf(&b, "%08X%08X%08X\t16777216\t%d.0\tgzip\tNone\n", 1, 0, i+1, ts)
	}
	return b.String()
}

// writePgMainMeta writes the backup.info files of the three pg-main backups
// of the fixtures (ids 20261018T020001, 20261019T020001, 20261020T020001).
func writePgMainMeta(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i, day := range []int{18, 19, 20} {
		begin := time.Date(2026, 10, day, 2, 0, 1, 0, fixtureZone)
		wal := []int{1, 3, 6}[i]
		sizes := []int{1503238553, 1509949440, 1516560384}
		layout := "2006-01-02 15:04:05.000000-07:00"
		body := fmt.Sprintf("begin_time=%s\nend_time=%s\nbegin_wal=%08X%08X%08X\nend_wal=%08X%08X%08X\ntimeline=1\nsize=%d\ndeduplicated_size=%d\nstatus=DONE\nparent_backup_id=None\nxlog_segment_size=16777216\n",
			begin.Format(layout), begin.Add(9*time.Minute).Format(layout), 1, 0, wal, 1, 0, wal, sizes[i], sizes[i])
		id := fmt.Sprintf("202610%02dT020001", day)
		if err := os.WriteFile(filepath.Join(dir, id+"-backup.info"), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

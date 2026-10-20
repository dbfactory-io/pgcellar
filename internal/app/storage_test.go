package app_test

import (
	"strings"
	"testing"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/store"
)

// The fixtures hold three DONE backups of pg-main (1503238553, 1509949440 and
// 1516560384 bytes) and seven archived WAL files of 16 MiB.
const (
	fixtureBackups = 1503238553 + 1509949440 + 1516560384
	fixtureWAL     = 7 * 16777216
)

func TestStorageBytesFromFixtures(t *testing.T) {
	e := apptest.New(t)
	if _, err := e.App.Store.AddRecovery(store.Recovery{Server: "pg-main", BackupID: "20261020T020001", Path: "/r/a", SizeBytes: 1000, ArchivePath: "/r/a.tar.gz", ArchiveSize: 500}); err != nil {
		t.Fatal(err)
	}
	e.App.RefreshStatuses(t.Context())
	st, ok := e.App.State("pg-main")
	if !ok || st.Storage == nil {
		t.Fatalf("no storage in %+v", st)
	}
	s := st.Storage
	if s.BackupBytes != fixtureBackups || s.BackupDiskBytes != fixtureBackups || s.Backups != 3 {
		t.Errorf("backups: %+v", s)
	}
	if s.WALBytes != fixtureWAL || s.WALFiles != 7 {
		t.Errorf("wal: %+v", s)
	}
	if s.RecoveriesBytes != 1500 {
		t.Errorf("recoveries: %d", s.RecoveriesBytes)
	}
	if s.TotalBytes != fixtureBackups+fixtureWAL+1500 {
		t.Errorf("total: %d", s.TotalBytes)
	}
	if s.FS == nil || s.FS.TotalBytes < s.FS.FreeBytes || s.FS.FreeBytes <= 0 {
		t.Errorf("fs: %+v", s.FS)
	}
	if s.AddedPerDay <= 0 {
		t.Errorf("added per day: %d", s.AddedPerDay)
	}
	// A server with an empty catalogue and no backup still has numbers.
	other, _ := e.App.State("broken")
	if other.Storage == nil || other.Storage.BackupBytes != 0 || other.Storage.WALBytes != 0 {
		t.Errorf("empty server: %+v", other.Storage)
	}
}

func lowSpace(mutate func(*config.Alerts)) func(*config.Config) {
	return func(c *config.Config) { mutate(&c.Alerts) }
}

func TestStorageThresholds(t *testing.T) {
	// Nothing is set off by the defaults on a test machine with some room.
	e := apptest.NewWith(t, lowSpace(func(a *config.Alerts) { a.MinFreePercent, a.MinFreeBytes = 0.0001, 0 }))
	if st, _ := e.App.State("pg-main"); problem(st, "free on") {
		t.Errorf("unexpected storage problem: %v", st.Problems)
	}

	// Far above any disk: red.
	e = apptest.NewWith(t, lowSpace(func(a *config.Alerts) { a.MinFreeBytes = 1 << 62 }))
	st, _ := e.App.State("pg-main")
	if st.Level != app.LevelFail || !problem(st, "% free on ") || !problem(st, "backups will start failing") {
		t.Errorf("red expected: %s %v", st.Level, st.Problems)
	}
	if st.Storage.FS.Level != app.LevelFail {
		t.Errorf("fs level %s", st.Storage.FS.Level)
	}

	// 100 % free is impossible: orange at least.
	e = apptest.NewWith(t, lowSpace(func(a *config.Alerts) { a.MinFreePercent = 100 }))
	st, _ = e.App.State("pg-main")
	if st.Level == app.LevelOK || !problem(st, "backups will start failing") {
		t.Errorf("alert expected: %s %v", st.Level, st.Problems)
	}
}

func problem(st app.ServerState, sub string) bool {
	for _, p := range st.Problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

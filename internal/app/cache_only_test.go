package app_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
)

func callCount(t *testing.T, e *apptest.Env) int {
	t.Helper()
	return strings.Count(apptest.Calls(t, e), "\n")
}

// Reading servers, one server, backups and state must not run Barman.
func TestReadsNeverRunBarman(t *testing.T) {
	e := apptest.New(t)
	before := callCount(t, e)
	if before == 0 {
		t.Fatal("the first refresh should have called the fake Barman")
	}
	ctx := t.Context()
	list, err := e.App.ListServers(ctx)
	if err != nil || len(list) < 3 {
		t.Fatalf("ListServers: %v %v", list, err)
	}
	for _, d := range list {
		if d.Name == "pg-legacy" { // in barman.d but not known to the fake Barman
			continue
		}
		if d.State == nil || !d.InBarman {
			t.Fatalf("%s: state/in_barman missing: %+v", d.Name, d)
		}
	}
	d, err := e.App.GetServer(ctx, "pg-main")
	if err != nil || d.State == nil {
		t.Fatalf("GetServer: %v %v", d, err)
	}
	backups, err := e.App.Backups(ctx, "tester", "pg-main")
	if err != nil || len(backups) == 0 {
		t.Fatalf("Backups from cache: %v %v", backups, err)
	}
	if backups[0].ID < backups[len(backups)-1].ID {
		t.Fatalf("backups should be newest first: %v", backups)
	}
	e.App.States()
	if _, err := e.App.GetServer(ctx, "nope"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown server: %v", err)
	}
	if after := callCount(t, e); after != before {
		t.Fatalf("Barman was run by a read: %d calls before, %d after\n%s", before, after, apptest.Calls(t, e))
	}
}

// An on-demand check does call Barman and refreshes the cache of that server.
func TestCheckUpdatesTheCache(t *testing.T) {
	e := apptest.New(t)
	before := callCount(t, e)
	if _, err := e.App.Check(t.Context(), "tester", "pg-main"); err != nil {
		t.Fatal(err)
	}
	if callCount(t, e) != before+1 {
		t.Fatalf("a check should run exactly one Barman command:\n%s", apptest.Calls(t, e))
	}
	st, _ := e.App.State("pg-main")
	if !st.CheckOK || len(st.Checks) == 0 {
		t.Fatalf("cache after check: %+v", st)
	}
}

// After a restart the last states are served at once, marked stale, until the
// first refresh completes.
func TestStatusCacheSurvivesARestart(t *testing.T) {
	e := apptest.New(t)
	before, _ := e.App.State("pg-main")
	if before.Stale || len(before.Backups) == 0 {
		t.Fatalf("fresh state: %+v", before)
	}
	if err := e.App.Close(); err != nil {
		t.Fatal(err)
	}
	a2, err := app.Load(filepath.Join(e.Dir, "pgcellar.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close() })
	after, ok := a2.State("pg-main")
	if !ok || !after.Stale || !a2.CacheStale() {
		t.Fatalf("reloaded state should be stale: ok=%v %+v", ok, after)
	}
	if after.Level != before.Level || len(after.Backups) != len(before.Backups) || after.LastBackupID != before.LastBackupID || !after.StreamingLive {
		t.Fatalf("reloaded state differs:\n%+v\n%+v", before, after)
	}
	if states, _, _ := a2.States(); len(states) < 3 {
		t.Fatalf("states after restart: %d", len(states))
	}
	a2.RefreshStatuses(t.Context())
	if st, _ := a2.State("pg-main"); st.Stale || a2.CacheStale() {
		t.Fatalf("still stale after a refresh: %+v", st)
	}
}

package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
)

// The fake Barman archived its last WAL at 02:10 on 2026-10-20 (it comes from
// `diagnose`, `status` has no such field), and the test config allows 1 hour.
func TestOldWALTurnsServerOrange(t *testing.T) {
	e := apptest.New(t)
	st, ok := e.App.State("pg-main")
	if !ok || st.LastWAL.IsZero() {
		t.Fatalf("last WAL missing: %+v", st)
	}
	for _, p := range st.Problems {
		if strings.Contains(p, "WAL") {
			t.Fatalf("fresh WAL flagged: %v", p)
		}
	}
	e.App.Now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local) }
	e.App.RefreshStatuses(t.Context())
	st, _ = e.App.State("pg-main")
	found := false
	for _, p := range st.Problems {
		found = found || strings.Contains(p, "last archived WAL file")
	}
	if !found || st.Level == app.LevelOK || st.Level == app.LevelFail && len(st.Problems) == 0 {
		t.Fatalf("old WAL not flagged: level=%v problems=%v", st.Level, st.Problems)
	}
}

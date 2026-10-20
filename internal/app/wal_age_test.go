package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/barman"
)

// The fake Barman archived its last WAL (closed segment) at 02:10 on
// 2026-10-20, the receiver wrote into its segment at 02:25, the clock says
// 02:30 and the test config allows 1 hour.
func problemsWith(st app.ServerState, sub string) bool {
	for _, p := range st.Problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func setStreamingTime(t *testing.T, e *apptest.Env, at time.Time) {
	t.Helper()
	part := filepath.Join(e.Home, "pg-main", "streaming", "000000010000000000000008.partial")
	if err := os.Chtimes(part, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestFreshStreamingIsHealthy(t *testing.T) {
	e := apptest.New(t)
	st, ok := e.App.State("pg-main")
	if !ok || st.LastWAL.IsZero() || !st.StreamingLive || st.LastDataReceived.IsZero() {
		t.Fatalf("WAL fields missing: %+v", st)
	}
	if problemsWith(st, "WAL") || len(st.Notes) != 0 {
		t.Fatalf("fresh WAL flagged: %v %v", st.Problems, st.Notes)
	}
	if st.LastDataReceivedAgeSeconds == nil || *st.LastDataReceivedAgeSeconds != 300 {
		t.Fatalf("received age = %v", st.LastDataReceivedAgeSeconds)
	}
}

// A quiet server: the last closed segment is old but data still arrives, so
// no alert, only a note about archive_timeout.
func TestOldSegmentWithLiveStreamingIsANote(t *testing.T) {
	e := apptest.New(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	setStreamingTime(t, e, now.Add(-3*time.Minute))
	e.App.Now = func() time.Time { return now }
	e.App.RefreshStatuses(t.Context())
	st, _ := e.App.State("pg-main")
	if problemsWith(st, "WAL") {
		t.Fatalf("a live receiver must not raise a WAL alert: %v", st.Problems)
	}
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0], "archive_timeout") {
		t.Fatalf("notes = %v", st.Notes)
	}
	if st.Level != app.LevelOK {
		t.Fatalf("level = %v problems=%v", st.Level, st.Problems)
	}
}

// Streaming is alive but nothing has arrived for longer than max_wal_age.
func TestNoDataForTooLongTurnsOrange(t *testing.T) {
	e := apptest.New(t)
	e.App.Now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local) }
	e.App.RefreshStatuses(t.Context())
	st, _ := e.App.State("pg-main")
	if !problemsWith(st, "No WAL data has arrived") || st.Level == app.LevelOK {
		t.Fatalf("stale streaming not flagged: level=%v problems=%v", st.Level, st.Problems)
	}
}

// stubEngine turns the receiver off in the digest of every server.
type stubEngine struct{ barman.Engine }

func (s stubEngine) Status(ctx context.Context, server string) (*barman.ServerStatus, error) {
	st, err := s.Engine.Status(ctx, server)
	if err != nil {
		return nil, err
	}
	var checks []barman.CheckResult
	for _, c := range st.Checks {
		if c.Name == "receive_wal_running" {
			c.OK = false
		}
		checks = append(checks, c)
	}
	return st.WithChecks(checks), nil
}

// Without a live receiver the age of the last archived segment is an alert again.
func TestOldSegmentWithoutStreamingTurnsOrange(t *testing.T) {
	e := apptest.New(t)
	e.App.Engine = stubEngine{e.App.Engine}
	e.App.Now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local) }
	e.App.RefreshStatuses(t.Context())
	st, _ := e.App.State("pg-main")
	if st.StreamingLive || !problemsWith(st, "last filled WAL file") || st.Level == app.LevelOK {
		t.Fatalf("old segment not flagged: live=%v level=%v problems=%v", st.StreamingLive, st.Level, st.Problems)
	}
}

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
)

func TestBackgroundCommandsAreNotAudited(t *testing.T) {
	e := apptest.New(t) // refreshes the status cache once, as the background loop does
	first, err := e.App.Store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	// The fixtures contain one failing server: its failure is logged once.
	for _, l := range first {
		if l.Actor != "system" || !strings.Contains(l.Details, "exit=") || strings.Contains(l.Details, "exit=0") {
			t.Fatalf("only failures may be audited in the background, got %+v", l)
		}
	}
	e.App.RefreshStatuses(context.Background())
	again, _ := e.App.Store.ListAudit(100)
	if len(again) != len(first) {
		t.Fatalf("a repeated identical failure was logged again: %d -> %d lines", len(first), len(again))
	}

	if _, err := e.App.Check(context.Background(), "gilles", "pg-main"); err != nil {
		t.Fatal(err)
	}
	list, _ := e.App.Store.ListAudit(100)
	if len(list) == 0 || list[0].Actor != "gilles" {
		t.Fatalf("a user check must be audited, got %+v", list)
	}
}

func TestPurgeAudit(t *testing.T) {
	e := apptest.New(t)
	if err := e.App.Store.AddAudit("a", "x", "", ""); err != nil {
		t.Fatal(err)
	}
	if n := e.App.PurgeAudit(); n != 0 { // default retention is 90 days: nothing old
		t.Errorf("fresh line purged: %d", n)
	}
	before, _ := e.App.Store.ListAudit(100)
	if n, _ := e.App.Store.PruneAudit(time.Now().Add(time.Minute)); int(n) != len(before) {
		t.Errorf("PruneAudit removed %d, want %d", n, len(before))
	}
}

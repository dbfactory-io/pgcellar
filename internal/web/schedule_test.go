package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
)

func TestActorLabel(t *testing.T) {
	if actorLabel(app.SchedulerActor) != "schedule" || actorLabel("alice") != "alice" {
		t.Fatal("the scheduler shows as schedule, others unchanged")
	}
}

func TestLastScheduledText(t *testing.T) {
	due := time.Now().Add(-time.Hour)
	ok := string(fmtLastScheduled(&app.ScheduledRun{DueAt: due, RunID: 3, Status: "ok"}))
	bad := string(fmtLastScheduled(&app.ScheduledRun{DueAt: due, RunID: 4, Status: "failed"}))
	if !strings.Contains(ok, "✓") || !strings.Contains(bad, "✗") {
		t.Fatalf("ok=%q bad=%q", ok, bad)
	}
	if fmtLastScheduled(nil) != "-" || fmtNext(time.Time{}) != "-" {
		t.Fatal("nothing to show")
	}
}

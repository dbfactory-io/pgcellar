package web

import (
	"html/template"
	"strconv"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

// fmtNext is "tonight 03:10": the next due time of a schedule, with the same
// wording as the timeline (nextBackupText).
func fmtNext(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return nextBackupText(t, time.Now())
}

// fmtLastScheduled is "today 03:10 ✓" for the last due time the scheduler
// handled, with ✗ when the backup failed or could not start.
func fmtLastScheduled(r *app.ScheduledRun) template.HTML {
	if r == nil {
		return "-"
	}
	when := template.HTMLEscapeString(whenText(r.DueAt, time.Now()))
	switch r.Status {
	case "ok":
		return template.HTML(when + ` <span class="pill ok" title="Run #` + itoa(r.RunID) + `">✓</span>`)
	case "running":
		return template.HTML(when + ` <span class="pill warn">running</span>`)
	case "skipped":
		note := r.Note
		if note == "" {
			note = "not started"
		}
		return template.HTML(when + ` <span class="pill bad" title="` + template.HTMLEscapeString(note) + `">✗ skipped</span>`)
	}
	return template.HTML(when + ` <span class="pill bad" title="Run #` + itoa(r.RunID) + `">✗</span>`)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// actorLabel is how the Runs page names who started a run: the scheduler
// shows as "schedule".
func actorLabel(actor string) string {
	if actor == app.SchedulerActor {
		return "schedule"
	}
	return actor
}

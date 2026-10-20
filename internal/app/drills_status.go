package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/store"
)

// DrillBrief is the last drill of a server in a status.
type DrillBrief struct {
	ID         int64     `json:"id"`
	OK         bool      `json:"ok"`
	BackupID   string    `json:"backup_id"`
	FinishedAt time.Time `json:"finished_at"`
	StepFailed string    `json:"step_failed,omitempty"`
	HasReport  bool      `json:"has_report"`
	Summary    string    `json:"summary"`
	AgeSeconds int64     `json:"age_seconds"`
}

// DrillStatus is what the pages and /metrics show about the drills of a
// server.
type DrillStatus struct {
	Scheduled        bool          `json:"scheduled"` // drills.enabled and a drills.schedule for this server
	Schedule         string        `json:"schedule,omitempty"`
	Next             time.Time     `json:"next,omitzero"`
	Last             *DrillBrief   `json:"last,omitempty"`
	LastOKAt         time.Time     `json:"last_ok_at,omitzero"`
	LastOKAgeSeconds *int64        `json:"last_ok_age_seconds,omitempty"`
	LastScheduled    *ScheduledRun `json:"last_scheduled,omitempty"`
	Running          bool          `json:"running"`
	// Stale: the schedule is set and the last drill failed, or none succeeded
	// for drills.max_age. The server turns orange.
	Stale bool   `json:"stale"`
	Text  string `json:"text"`          // "Last drill: 3 d ago ✔"
	Why   string `json:"why,omitempty"` // the sentence that goes with Stale
}

// shortAgo is "12 min", "5 h", "3 d".
func shortAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d d ago", int(d.Hours()/24))
}

// DrillStatusOf computes the drill status of a server at `now`. It reads the
// database only (never Barman nor PostgreSQL).
func (a *App) DrillStatusOf(name string, now time.Time) *DrillStatus {
	cfg := a.Config()
	ds := &DrillStatus{}
	if cs := findCfg(cfg, name); cs != nil {
		if spec := specOf(cfg, kindDrill, *cs); spec != "" {
			ds.Scheduled, ds.Schedule = true, spec
			ds.Next, _ = NextDue(configScheduleOf(spec), now)
		}
	}
	a.runMu.Lock()
	_, ds.Running = a.running[drillRunKey+name]
	a.runMu.Unlock()
	if rec, ok := a.lastScheduledKind(kindDrill, name); ok && ds.Scheduled {
		a.deriveStatus(&rec)
		ds.LastScheduled = &rec
	}
	list, err := a.Store.ListDrills(name, 100)
	if err != nil {
		return ds
	}
	var lastOK *store.Drill
	for i := range list {
		if list[i].OK {
			lastOK = &list[i]
			break
		}
	}
	if len(list) > 0 {
		d := list[0]
		v := drillView(d)
		ds.Last = &DrillBrief{ID: d.ID, OK: d.OK, BackupID: d.BackupID, FinishedAt: d.FinishedAt, StepFailed: d.StepFailed,
			HasReport: d.ReportPath != "", Summary: v.Details.Summary, AgeSeconds: int64(now.Sub(d.FinishedAt).Seconds())}
	}
	if lastOK != nil {
		ds.LastOKAt = lastOK.FinishedAt
		age := int64(now.Sub(lastOK.FinishedAt).Seconds())
		ds.LastOKAgeSeconds = &age
	}
	switch {
	case ds.Last == nil:
		ds.Text = "No drill yet"
	case ds.Last.OK:
		ds.Text = "Last drill: " + shortAgo(time.Duration(ds.Last.AgeSeconds)*time.Second) + " ✔"
	default:
		ds.Text = "Last drill: " + shortAgo(time.Duration(ds.Last.AgeSeconds)*time.Second) + " ✘ failed at \"" + DrillStepLabel(ds.Last.StepFailed) + "\""
	}
	if ds.Scheduled && ds.Last != nil {
		switch {
		case !ds.Last.OK:
			ds.Stale = true
			ds.Why = "The last restore drill failed at the step \"" + DrillStepLabel(ds.Last.StepFailed) + "\": the backups of this server are not proven restorable."
		case now.Sub(ds.LastOKAt) > cfg.Drills.MaxAge:
			ds.Stale = true
			ds.Why = fmt.Sprintf("No restore drill succeeded for %s (drills.max_age is %s).", HumanDuration(now.Sub(ds.LastOKAt)), HumanDuration(cfg.Drills.MaxAge))
		}
	}
	return ds
}

// applyDrill adds the drill status to a state read from the cache and turns
// the server orange when the drills are overdue or failing. The cached
// state itself is left alone.
func (a *App) applyDrill(st *ServerState, now time.Time) {
	ds := a.DrillStatusOf(st.Name, now)
	st.Drill = ds
	if ds.Stale && ds.Why != "" && !strings.Contains(strings.Join(st.Problems, "\n"), ds.Why) {
		st.Problems = append(append([]string(nil), st.Problems...), ds.Why)
		if st.Level == LevelOK {
			st.Level = LevelWarn
		}
	}
}

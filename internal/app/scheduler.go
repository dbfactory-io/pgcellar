package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stranix79/pgcellar/internal/config"
)

// SchedulerActor is the actor of the backups pgCellar starts by itself. The
// Runs page shows it as "schedule".
const SchedulerActor = "scheduler"

// DefaultSchedulerInterval is how often the scheduler looks at the clock.
const DefaultSchedulerInterval = 30 * time.Second

const (
	settingSchedulerTick      = "scheduler.tick"
	settingSchedulerLast      = "scheduler.last."
	settingSchedulerDrillLast = "scheduler.drill.last."
	maxDuesPerTick            = 2000
)

// A schedule kind is something the scheduler starts at due times: the backups
// (schedule.backup) and the restore drills (drills.schedule).
const (
	kindBackup = iota
	kindDrill
)

// entryKey is the key of a server's entry for a kind in the scheduler memory.
func entryKey(kind int, server string) string {
	if kind == kindDrill {
		return drillRunKey + server
	}
	return server
}

func lastKey(kind int, server string) string {
	if kind == kindDrill {
		return settingSchedulerDrillLast + server
	}
	return settingSchedulerLast + server
}

// specOf returns the schedule text of a server for a kind, "" when it has none.
func specOf(cfg *config.Config, kind int, srv config.Server) string {
	if kind == kindDrill {
		if !cfg.Drills.IsEnabled() {
			return ""
		}
		return strings.TrimSpace(srv.Drills.Schedule)
	}
	if !srv.Schedule.IsEnabled() {
		return ""
	}
	return srv.Schedule.Backup
}

// ScheduledRun is what the scheduler did with the last due time it handled
// for a server. It is kept in the settings table, so a restart does not run
// the same due time twice.
type ScheduledRun struct {
	DueAt time.Time `json:"due_at"`
	RunID int64     `json:"run_id"`
	Note  string    `json:"note,omitempty"` // why nothing was started
	// Status is derived when read: ok, failed, running or skipped.
	Status string `json:"status,omitempty"`
}

// schedEntry is what the scheduler remembers of one server while running.
type schedEntry struct {
	spec  string    // the schedule text it was computed from
	cron  string    // its 5-field form
	after time.Time // due times strictly after this one are still to be handled
	dead  bool      // the expression never matches: do not search again
}

type schedState struct {
	mu      sync.Mutex
	entries map[string]*schedEntry
	started bool // the first tick of this process has run
}

// NextDue returns the first due time of a schedule strictly after `after`.
// It is the single source of truth for "next backup": the scheduler, the
// timeline and the status pages all use it.
func NextDue(s config.Schedule, after time.Time) (time.Time, bool) {
	if !s.IsEnabled() || strings.TrimSpace(s.Backup) == "" {
		return time.Time{}, false
	}
	cron, _, err := config.ParseSchedule(s.Backup)
	if err != nil {
		return time.Time{}, false
	}
	return nextCron(cron, after)
}

// configScheduleOf wraps a schedule text for NextDue.
func configScheduleOf(spec string) config.Schedule {
	return config.Schedule{Backup: spec, Enabled: config.Ptr(true)}
}

// lastDueIn returns the latest due time of a cron expression in (after, now].
func lastDueIn(cron string, after, now time.Time) (time.Time, bool, bool) {
	var last time.Time
	t := after
	for i := 0; i < maxDuesPerTick; i++ {
		n, ok := nextCron(cron, t)
		if !ok {
			return last, !last.IsZero(), i == 0 // never matches
		}
		if n.After(now) {
			break
		}
		last, t = n, n
	}
	return last, !last.IsZero(), false
}

// SchedulerRestart forgets what the scheduler holds in memory, as a new
// process would. Tests use it to check that a restart is safe.
func (a *App) SchedulerRestart() {
	a.sched.mu.Lock()
	a.sched.entries = nil
	a.sched.started = false
	a.sched.mu.Unlock()
}

func (a *App) schedulerLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-a.firstCron: // the WAL receivers are up
	}
	iv := a.SchedulerInterval
	if iv <= 0 {
		iv = DefaultSchedulerInterval
	}
	a.SchedulerTick(ctx, a.now())
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.SchedulerTick(ctx, a.now())
		}
	}
}

func (a *App) lastScheduled(server string) (ScheduledRun, bool) {
	return a.lastScheduledKind(kindBackup, server)
}

func (a *App) lastScheduledKind(kind int, server string) (ScheduledRun, bool) {
	v, ok, err := a.Store.GetSetting(lastKey(kind, server))
	if err != nil || !ok {
		return ScheduledRun{}, false
	}
	var r ScheduledRun
	if json.Unmarshal([]byte(v), &r) != nil {
		return ScheduledRun{}, false
	}
	return r, true
}

// initialAfter decides from which moment the due times of a server count.
// A server met after the process started, or whose schedule just changed,
// counts from now. At startup the due times the process missed while it was
// stopped count too, but not older than catch_up and not those already
// handled.
func (a *App) initialAfter(kind int, server string, now time.Time, noCatchUp bool, catchUp time.Duration) time.Time {
	after := now
	if noCatchUp || catchUp <= 0 {
		return after
	}
	if v, ok, err := a.Store.GetSetting(settingSchedulerTick); err == nil && ok {
		if tick, err := time.Parse(time.RFC3339Nano, v); err == nil && tick.Before(now) {
			after = tick
			if floor := now.Add(-catchUp); after.Before(floor) {
				after = floor
			}
		}
	}
	if rec, ok := a.lastScheduledKind(kind, server); ok && rec.DueAt.After(after) && !rec.DueAt.After(now) {
		after = rec.DueAt
	}
	return after
}

// SchedulerTick looks at the schedules once, with `now` as the clock, and
// starts the backups that are due. At most one backup is started per server
// and per due time; when several due times were missed only the latest runs.
func (a *App) SchedulerTick(ctx context.Context, now time.Time) {
	cfg := a.Config()
	sc := cfg.Scheduler
	if !sc.IsEnabled() || a.Store == nil {
		return
	}
	parallel := sc.Parallel
	if parallel < 1 {
		parallel = 1
	}
	s := &a.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]*schedEntry{}
	}
	started := s.started

	type due struct {
		kind   int
		server string
		at     time.Time
		e      *schedEntry
	}
	var pending []due
	seen := map[string]bool{}
	for _, kind := range []int{kindBackup, kindDrill} {
		for _, srv := range cfg.Servers {
			spec := specOf(cfg, kind, srv)
			if spec == "" {
				continue
			}
			cron, _, err := config.ParseSchedule(spec)
			if err != nil {
				continue
			}
			key := entryKey(kind, srv.Name)
			seen[key] = true
			e := s.entries[key]
			if e == nil || e.spec != spec {
				e = &schedEntry{spec: spec, cron: cron, after: a.initialAfter(kind, srv.Name, now, started || e != nil, sc.EffectiveCatchUp())}
				s.entries[key] = e
			}
			if e.dead {
				continue
			}
			at, found, never := lastDueIn(e.cron, e.after, now)
			if never {
				e.dead = true
				continue
			}
			if found {
				pending = append(pending, due{kind, srv.Name, at, e})
			} else {
				e.after = now
			}
		}
	}
	for name := range s.entries { // servers that left the schedule start afresh if they come back
		if !seen[name] {
			delete(s.entries, name)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].at.Equal(pending[j].at) {
			return pending[i].at.Before(pending[j].at)
		}
		if pending[i].kind != pending[j].kind {
			return pending[i].kind < pending[j].kind // a backup before a drill
		}
		return pending[i].server < pending[j].server
	})
	for _, p := range pending {
		if a.runningCount() >= parallel {
			break // the others wait for the next tick
		}
		rec := ScheduledRun{DueAt: p.at}
		var id int64
		var err error
		what := "backup"
		if p.kind == kindDrill {
			what = "drill"
			id, err = a.StartDrill(ctx, SchedulerActor, p.server, DrillInput{})
		} else {
			id, err = a.StartBackup(ctx, SchedulerActor, p.server)
		}
		var conflict *ConflictError
		switch {
		case err == nil:
			rec.RunID = id
		case errors.As(err, &conflict) && conflict.Retry:
			continue // busy: the due time stays pending and is tried again at the next tick
		case errors.As(err, &conflict):
			rec.Note = "a " + what + " was already running"
			a.Logf("scheduler: %s %s due %s skipped: %v", what, p.server, p.at.Format(time.RFC3339), err)
		default:
			rec.Note = err.Error()
			a.Logf("scheduler: %s %s due %s not started: %v", what, p.server, p.at.Format(time.RFC3339), err)
		}
		if b, merr := json.Marshal(rec); merr == nil {
			if serr := a.Store.SetSetting(lastKey(p.kind, p.server), string(b)); serr != nil {
				a.Logf("scheduler: cannot record the %s of %s: %v", what, p.server, serr)
			}
		}
		p.e.after = now
	}
	// The tick is the point up to which every due time has been dealt with;
	// a due time still waiting for a free slot keeps it back.
	tick := now
	for _, p := range pending {
		if p.e.after.Before(tick) {
			tick = p.e.after
		}
	}
	s.started = true
	if err := a.Store.SetSetting(settingSchedulerTick, tick.Format(time.RFC3339Nano)); err != nil {
		a.Logf("scheduler: cannot record the tick: %v", err)
	}
}

// runningCount is the number of backups in progress, whoever started them.
func (a *App) runningCount() int {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return len(a.running)
}

// scheduleInfo fills the planning facts of a server for the pages: the next
// due time and what happened at the last one.
func (a *App) scheduleInfo(name string, now time.Time) (next time.Time, last *ScheduledRun) {
	if cs := findCfg(a.Config(), name); cs != nil {
		next, _ = NextDue(cs.Schedule, now)
	}
	rec, ok := a.lastScheduled(name)
	if !ok {
		return next, nil
	}
	a.deriveStatus(&rec)
	return next, &rec
}

// deriveStatus sets the Status of a scheduled run from its run record.
func (a *App) deriveStatus(rec *ScheduledRun) {
	rec.Status = "skipped"
	if rec.RunID > 0 {
		rec.Status = "failed"
		if r, err := a.Store.GetRun(rec.RunID); err == nil {
			switch {
			case r.FinishedAt == nil:
				rec.Status = "running"
			case r.ExitCode != nil && *r.ExitCode == 0:
				rec.Status = "ok"
			}
		}
	}
}

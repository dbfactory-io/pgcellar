package app_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
)

func at(day, h, m, s int) time.Time {
	return time.Date(2026, 10, day, h, m, s, 0, time.Local)
}

// setSchedule changes the schedule of a server through the service layer.
func setSchedule(t *testing.T, e *apptest.Env, name, backup string, enabled bool) {
	t.Helper()
	in, err := e.App.CurrentInput(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	in.Schedule = config.Schedule{Backup: backup, Enabled: enabled}
	if _, err := e.App.UpdateServer("tester", name, in); err != nil {
		t.Fatalf("update %s: %v", name, err)
	}
}

// scheduledRuns counts the runs the scheduler started for a server.
func scheduledRuns(t *testing.T, a *app.App, server string) int {
	t.Helper()
	runs, err := a.Runs(server, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range runs {
		if r.Actor == app.SchedulerActor {
			n++
		}
	}
	return n
}

func waitIdle(t *testing.T, a *app.App, server string) {
	t.Helper()
	runs, _ := a.Runs(server, 100)
	for _, r := range runs {
		apptest.WaitRun(t, a, r.ID)
	}
}

func TestNextDue(t *testing.T) {
	from := at(20, 2, 30, 0)
	cases := []struct {
		spec string
		want time.Time
	}{
		{"daily 03:10", at(20, 3, 10, 0)},
		{"daily 02:00", at(21, 2, 0, 0)},
		{"weekly sun 03:00", at(25, 3, 0, 0)}, // 20 October 2026 is a Tuesday
		{"every 6 hours", at(20, 6, 0, 0)},
		{"hourly", at(20, 3, 0, 0)},
		{"15 4 * * 1-5", at(20, 4, 15, 0)},
		{"*/20 * * * *", at(20, 2, 40, 0)},
	}
	for _, c := range cases {
		got, ok := app.NextDue(config.Schedule{Backup: c.spec, Enabled: true}, from)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q: got %v (%v), want %v", c.spec, got, ok, c.want)
		}
	}
	if _, ok := app.NextDue(config.Schedule{Backup: "daily 03:10"}, from); ok {
		t.Error("a disabled schedule has no next due time")
	}
	if _, ok := app.NextDue(config.Schedule{Backup: "whenever", Enabled: true}, from); ok {
		t.Error("an invalid schedule has no next due time")
	}
}

func TestSchedulerRunsOncePerDueTime(t *testing.T) {
	e := apptest.New(t)
	a := e.App
	ctx := context.Background()
	setSchedule(t, e, "pg-main", "daily 03:10", true)

	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	a.SchedulerTick(ctx, at(20, 3, 9, 59))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("nothing is due yet, got %d run(s)", n)
	}
	a.SchedulerTick(ctx, at(20, 3, 10, 5))
	waitIdle(t, a, "pg-main")
	for _, m := range []int{11, 12, 40} {
		a.SchedulerTick(ctx, at(20, 3, m, 0))
	}
	if n := scheduledRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("one due time, one run: got %d", n)
	}
	// A restart a few minutes later does not run it again.
	a.SchedulerRestart()
	a.SchedulerTick(ctx, at(20, 3, 45, 0))
	a.SchedulerTick(ctx, at(20, 3, 46, 0))
	if n := scheduledRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("a restart must not repeat a due time: got %d runs", n)
	}
	// The next day it runs again.
	a.SchedulerTick(ctx, at(21, 3, 10, 30))
	waitIdle(t, a, "pg-main")
	if n := scheduledRuns(t, a, "pg-main"); n != 2 {
		t.Fatalf("next day: got %d runs, want 2", n)
	}

	d, err := a.GetServer(ctx, "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if d.LastScheduled == nil || d.LastScheduled.Status != "ok" || !d.LastScheduled.DueAt.Equal(at(21, 3, 10, 0)) {
		t.Fatalf("last scheduled run: %+v", d.LastScheduled)
	}
	if want := at(22, 3, 10, 0); !d.NextBackup.Equal(want) && d.NextBackup.Before(a.Now()) {
		t.Fatalf("next backup %v", d.NextBackup)
	}
}

func TestSchedulerCatchUp(t *testing.T) {
	ctx := context.Background()

	t.Run("within the window", func(t *testing.T) {
		e := apptest.New(t)
		a := e.App
		setSchedule(t, e, "pg-main", "daily 03:10", true)
		a.SchedulerTick(ctx, at(20, 2, 30, 0)) // last tick before the stop
		a.SchedulerRestart()
		a.SchedulerTick(ctx, at(20, 5, 0, 0)) // back 1h50 after the due time
		waitIdle(t, a, "pg-main")
		if n := scheduledRuns(t, a, "pg-main"); n != 1 {
			t.Fatalf("the missed backup must run: got %d", n)
		}
		a.SchedulerTick(ctx, at(20, 5, 1, 0))
		if n := scheduledRuns(t, a, "pg-main"); n != 1 {
			t.Fatalf("caught up once only: got %d", n)
		}
	})

	t.Run("older than catch_up", func(t *testing.T) {
		e := apptest.New(t)
		a := e.App
		setSchedule(t, e, "pg-main", "daily 03:10", true)
		a.SchedulerTick(ctx, at(20, 2, 30, 0))
		a.SchedulerRestart()
		a.SchedulerTick(ctx, at(20, 10, 0, 0)) // due time was 6h50 ago
		if n := scheduledRuns(t, a, "pg-main"); n != 0 {
			t.Fatalf("too late to catch up: got %d run(s)", n)
		}
	})

	t.Run("catch_up 0", func(t *testing.T) {
		e := apptest.New(t)
		a := e.App
		setSchedule(t, e, "pg-main", "daily 03:10", true)
		a.Config().Scheduler.CatchUp = 0
		a.SchedulerTick(ctx, at(20, 2, 30, 0))
		a.SchedulerRestart()
		a.SchedulerTick(ctx, at(20, 3, 20, 0))
		if n := scheduledRuns(t, a, "pg-main"); n != 0 {
			t.Fatalf("catch_up 0 never catches up: got %d run(s)", n)
		}
	})

	t.Run("first start ever", func(t *testing.T) {
		e := apptest.New(t)
		a := e.App
		setSchedule(t, e, "pg-main", "daily 03:10", true)
		a.SchedulerTick(ctx, at(20, 5, 0, 0)) // no tick on record: nothing was missed
		if n := scheduledRuns(t, a, "pg-main"); n != 0 {
			t.Fatalf("a first start has nothing to catch up: got %d run(s)", n)
		}
	})
}

func TestSchedulerParallel(t *testing.T) {
	ctx := context.Background()
	t.Setenv("FAKE_BARMAN_BACKUP_DELAY", "1")

	for _, parallel := range []int{1, 2} {
		e := apptest.New(t)
		a := e.App
		setSchedule(t, e, "pg-main", "daily 03:10", true)
		setSchedule(t, e, "pg-legacy", "daily 03:10", true)
		a.Config().Scheduler.Parallel = parallel
		a.SchedulerTick(ctx, at(20, 2, 30, 0))
		a.SchedulerTick(ctx, at(20, 3, 10, 5))
		total := scheduledRuns(t, a, "pg-main") + scheduledRuns(t, a, "pg-legacy")
		if total != parallel {
			t.Fatalf("parallel %d: %d backup(s) started at once", parallel, total)
		}
		// The one that waited starts once a slot is free, still for the same due time.
		waitIdle(t, a, "pg-main")
		waitIdle(t, a, "pg-legacy")
		a.SchedulerTick(ctx, at(20, 3, 10, 40))
		waitIdle(t, a, "pg-main")
		waitIdle(t, a, "pg-legacy")
		if n := scheduledRuns(t, a, "pg-main"); n != 1 {
			t.Fatalf("parallel %d: pg-main ran %d times", parallel, n)
		}
		if n := scheduledRuns(t, a, "pg-legacy"); n != 1 {
			t.Fatalf("parallel %d: pg-legacy ran %d times", parallel, n)
		}
	}
}

func TestSchedulerRespectsRunningBackup(t *testing.T) {
	e := apptest.New(t)
	a := e.App
	ctx := context.Background()
	t.Setenv("FAKE_BARMAN_BACKUP_DELAY", "1")
	setSchedule(t, e, "pg-main", "daily 03:10", true)
	a.Config().Scheduler.Parallel = 2 // a free slot, but the server itself is busy
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	id, err := a.StartBackup(ctx, "someone", "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	a.SchedulerTick(ctx, at(20, 3, 10, 5))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("no second backup on the same server: got %d", n)
	}
	apptest.WaitRun(t, a, id)
	a.SchedulerTick(ctx, at(20, 3, 11, 0))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("the skipped due time is not retried: got %d", n)
	}
	d, _ := a.GetServer(ctx, "pg-main")
	if d.LastScheduled == nil || d.LastScheduled.Status != "skipped" {
		t.Fatalf("last scheduled run: %+v", d.LastScheduled)
	}
}

func TestSchedulerDisabled(t *testing.T) {
	ctx := context.Background()

	e := apptest.New(t)
	a := e.App
	setSchedule(t, e, "pg-main", "daily 03:10", true)
	a.Config().Scheduler.Enabled = false
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	a.SchedulerTick(ctx, at(20, 3, 20, 0))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("scheduler.enabled false: got %d run(s)", n)
	}

	e = apptest.New(t)
	a = e.App
	setSchedule(t, e, "pg-main", "daily 03:10", false)
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	a.SchedulerTick(ctx, at(20, 3, 20, 0))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("schedule.enabled false: got %d run(s)", n)
	}
}

func TestSchedulerFollowsScheduleChange(t *testing.T) {
	e := apptest.New(t)
	a := e.App
	ctx := context.Background()
	setSchedule(t, e, "pg-main", "daily 03:10", true)
	a.SchedulerTick(ctx, at(20, 2, 30, 0))
	setSchedule(t, e, "pg-main", "daily 04:00", true)
	a.SchedulerTick(ctx, at(20, 3, 10, 30))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("the old time no longer counts: got %d run(s)", n)
	}
	a.SchedulerTick(ctx, at(20, 4, 0, 30))
	waitIdle(t, a, "pg-main")
	if n := scheduledRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("new time: got %d run(s)", n)
	}
}

// A hand edit of pgcellar.yaml is picked up by the same Config() reload as
// everything else.
func TestSchedulerFollowsYAMLReload(t *testing.T) {
	e := apptest.New(t)
	a := e.App
	ctx := context.Background()
	setSchedule(t, e, "pg-main", "daily 03:10", true)
	a.SchedulerTick(ctx, at(20, 2, 30, 0))

	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Servers[0].Schedule.Backup = "daily 05:00"
	if err := config.Save(a.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(a.ConfigPath, future, future); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // Config() looks at the file once a second
	a.SchedulerTick(ctx, at(20, 3, 10, 30))
	if n := scheduledRuns(t, a, "pg-main"); n != 0 {
		t.Fatalf("old time after reload: got %d run(s)", n)
	}
	a.SchedulerTick(ctx, at(20, 5, 0, 30))
	waitIdle(t, a, "pg-main")
	if n := scheduledRuns(t, a, "pg-main"); n != 1 {
		t.Fatalf("new time after reload: got %d run(s)", n)
	}
}

package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/alerts"
	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/store"
)

// sink is a webhook that remembers what it was sent.
type sink struct {
	mu   sync.Mutex
	msgs []alerts.Payload
	srv  *httptest.Server
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p alerts.Payload
		if json.Unmarshal(b, &p) == nil {
			s.mu.Lock()
			s.msgs = append(s.msgs, p)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// take returns the messages received since the last call, for the server
// ("" = all).
func (s *sink) take(server string) []alerts.Payload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out, keep []alerts.Payload
	for _, m := range s.msgs {
		if server == "" || m.Server == server {
			out = append(out, m)
		} else {
			keep = append(keep, m)
		}
	}
	s.msgs = keep
	return out
}

func alertEnv(t *testing.T, mutate func(*config.Config)) (*apptest.Env, *sink) {
	t.Helper()
	sk := newSink(t)
	e := apptest.NewWith(t, func(c *config.Config) {
		c.Alerts.Webhook = &config.WebhookAlert{URL: sk.srv.URL}
		c.BaseURL = "https://cellar.example.com"
		if mutate != nil {
			mutate(c)
		}
	})
	return e, sk
}

func setClock(e *apptest.Env, t time.Time) {
	e.App.Now = func() time.Time { return t }
	e.App.RefreshStatuses(context.Background())
}

var day0 = time.Date(2026, 10, 20, 2, 30, 0, 0, time.Local)

func TestAlertsLevelChangeRepeatAndResolved(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	a := e.App
	st, _ := a.State("pg-main")
	if st.Level != app.LevelOK {
		t.Fatalf("pg-main should start healthy: %+v", st)
	}
	a.AlertsTick(ctx, day0) // first look: baseline, healthy servers say nothing
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("a healthy server must be silent: %+v", got)
	}
	a.AlertsTick(ctx, day0.Add(time.Minute))
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("nothing changed, nothing said: %+v", got)
	}

	// three days later with no new backup: red
	later := day0.Add(72 * time.Hour)
	setClock(e, later)
	a.AlertsTick(ctx, later)
	got := sk.take("pg-main")
	if len(got) != 1 {
		t.Fatalf("one message expected, got %+v", got)
	}
	m := got[0]
	if m.Kind != alerts.KindLevel || m.Level != "fail" || m.Title != "pg-main is failing" || m.URL != "https://cellar.example.com/servers/pg-main" || m.Action == "" || len(m.Lines) == 0 {
		t.Errorf("message: %+v", m)
	}
	if !strings.Contains(m.Text, "❌ pg-main is failing") || !strings.Contains(m.Text, "What to do:") {
		t.Errorf("text: %s", m.Text)
	}

	// the same condition is not repeated before repeat_every (24h)...
	a.AlertsTick(ctx, later.Add(time.Hour))
	a.AlertsTick(ctx, later.Add(23*time.Hour))
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("no repetition before repeat_every: %+v", got)
	}
	// ...and is reminded after it
	setClock(e, later.Add(25*time.Hour))
	a.AlertsTick(ctx, later.Add(25*time.Hour))
	got = sk.take("pg-main")
	if len(got) != 1 || got[0].Kind != alerts.KindReminder || !strings.Contains(got[0].Title, "still failing") {
		t.Fatalf("reminder: %+v", got)
	}

	// back to normal: one "resolved" message
	setClock(e, day0)
	a.AlertsTick(ctx, day0.Add(26*time.Hour))
	got = sk.take("pg-main")
	if len(got) != 1 || got[0].Kind != alerts.KindResolved || got[0].Level != "ok" || got[0].Title != "pg-main is healthy again" || !strings.Contains(got[0].Text, "✅") {
		t.Fatalf("resolved: %+v", got)
	}
	a.AlertsTick(ctx, day0.Add(27*time.Hour))
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("resolved is said once: %+v", got)
	}

	// every send is in the journal
	j, err := a.AlertJournal(50)
	if err != nil || len(j) < 3 {
		t.Fatalf("journal: %+v %v", j, err)
	}
	if j[0].Channel != "webhook" || !j[0].OK {
		t.Errorf("journal row: %+v", j[0])
	}
}

func TestAlertsStateSurvivesRestart(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	e.App.AlertsTick(ctx, day0)
	later := day0.Add(72 * time.Hour)
	setClock(e, later)
	e.App.AlertsTick(ctx, later)
	if got := sk.take("pg-main"); len(got) != 1 {
		t.Fatalf("first alert: %+v", got)
	}
	// A new process starts with the same database: it must not announce the same thing again.
	e.App.AlertsTick(ctx, later.Add(time.Hour))
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("persisted state must prevent a repeat: %+v", got)
	}
}

func TestAlertsQuietAfterStartAndWhenStale(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	e.App.AlertsTick(ctx, day0) // baseline
	later := day0.Add(72 * time.Hour)
	setClock(e, later)
	start := later.Add(-30 * time.Second)
	e.App.AlertsSince = start
	e.App.AlertsTick(ctx, later) // 30 s after start: quiet
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("quiet during the first two minutes: %+v", got)
	}
	e.App.AlertsTick(ctx, start.Add(app.AlertStartGrace+time.Second))
	if got := sk.take("pg-main"); len(got) != 1 {
		t.Fatalf("after the grace period the problem is announced: %+v", got)
	}
}

func TestAlertsBackupFailure(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	a := e.App
	// an old failure is history, not news
	old, _ := a.Store.StartRun("pg-main", store.KindBackup, "gilles")
	_ = a.Store.FinishRun(old, 1, "", "old failure")
	a.AlertsTick(ctx, day0)
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("history is silent: %+v", got)
	}
	// a run still going does not block the ones after it
	running, _ := a.Store.StartRun("pg-main", store.KindBackup, "gilles")
	bad, _ := a.Store.StartRun("pg-main", store.KindBackup, app.SchedulerActor)
	_ = a.Store.FinishRun(bad, 1, "", "ERROR: connection refused\nlast line: could not connect")
	fine, _ := a.Store.StartRun("pg-main", store.KindBackup, "gilles")
	_ = a.Store.FinishRun(fine, 0, "ok", "")
	intr, _ := a.Store.StartRun("pg-main", store.KindBackup, "gilles")
	_ = a.Store.FinishRun(intr, -1, "", "interrupted: pgCellar was restarted while this run was in progress")
	a.AlertsTick(ctx, day0.Add(time.Minute))
	got := sk.take("pg-main")
	if len(got) != 1 || got[0].Kind != alerts.KindBackup {
		t.Fatalf("one backup failure expected: %+v", got)
	}
	m := got[0]
	if m.Title != "Backup of pg-main failed" || !strings.Contains(strings.Join(m.Lines, "\n"), "scheduled") || !strings.Contains(m.Text, "could not connect") || !strings.HasSuffix(m.URL, "/runs/"+itoa(bad)) {
		t.Errorf("backup message: %+v", m)
	}
	a.AlertsTick(ctx, day0.Add(2*time.Minute))
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("said once: %+v", got)
	}
	// the run that was going fails later: still announced
	_ = a.Store.FinishRun(running, 2, "", "late failure")
	a.AlertsTick(ctx, day0.Add(3*time.Minute))
	got = sk.take("pg-main")
	if len(got) != 1 || !strings.Contains(got[0].Text, "late failure") {
		t.Fatalf("late failure: %+v", got)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestAlertsDrill(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	a := e.App
	a.AlertsTick(ctx, day0)
	add := func(ok bool, step string) {
		_, err := a.Store.AddDrill(store.Drill{Server: "pg-main", BackupID: "20261019T020001", StartedAt: day0, FinishedAt: day0, OK: ok, StepFailed: step,
			Details: `{"summary":"2 databases restored","error":"pg_ctl: could not start"}`})
		if err != nil {
			t.Fatal(err)
		}
	}
	add(true, "")
	a.AlertsTick(ctx, day0.Add(time.Minute))
	for _, m := range sk.take("pg-main") {
		if m.Kind == alerts.KindDrillOK {
			t.Fatalf("a passing drill is silent by default: %+v", m)
		}
	}
	add(false, "start")
	a.AlertsTick(ctx, day0.Add(2*time.Minute))
	var failed []alerts.Payload
	for _, m := range sk.take("pg-main") {
		if m.Kind == alerts.KindDrill {
			failed = append(failed, m)
		}
	}
	if len(failed) != 1 || failed[0].Title != "Restore drill of pg-main failed" || !strings.Contains(failed[0].Text, "Start PostgreSQL") || !strings.Contains(failed[0].Text, "could not start") {
		t.Fatalf("drill failure: %+v", failed)
	}

	// with on_drill_success
	e2, sk2 := alertEnv(t, func(c *config.Config) { c.Alerts.OnDrillSuccess = true })
	e2.App.AlertsTick(ctx, day0)
	_, _ = e2.App.Store.AddDrill(store.Drill{Server: "pg-main", StartedAt: day0, FinishedAt: day0, OK: true, Details: `{"summary":"2 databases restored"}`})
	e2.App.AlertsTick(ctx, day0.Add(time.Minute))
	var okMsgs []alerts.Payload
	for _, m := range sk2.take("pg-main") {
		if m.Kind == alerts.KindDrillOK {
			okMsgs = append(okMsgs, m)
		}
	}
	if len(okMsgs) != 1 || okMsgs[0].Level != "ok" || !strings.HasPrefix(okMsgs[0].Text, "✅ Restore drill of pg-main passed") {
		t.Fatalf("drill success: %+v", okMsgs)
	}
}

func TestAlertsSkippedSchedule(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	a := e.App
	a.AlertsTick(ctx, day0)
	due := day0.Add(-30 * time.Minute)
	b, _ := json.Marshal(app.ScheduledRun{DueAt: due, Note: "a backup was already running"})
	if err := a.Store.SetSetting("scheduler.last.pg-main", string(b)); err != nil {
		t.Fatal(err)
	}
	a.AlertsTick(ctx, day0.Add(time.Minute))
	var skipped []alerts.Payload
	for _, m := range sk.take("pg-main") {
		if m.Kind == alerts.KindSkipped {
			skipped = append(skipped, m)
		}
	}
	if len(skipped) != 1 || skipped[0].Level != "warn" || !strings.Contains(skipped[0].Text, "already running") || !strings.HasPrefix(skipped[0].Title, "Scheduled backup of pg-main did not run") {
		t.Fatalf("skipped: %+v", skipped)
	}
	a.AlertsTick(ctx, day0.Add(2*time.Minute))
	for _, m := range sk.take("pg-main") {
		if m.Kind == alerts.KindSkipped {
			t.Fatalf("a skipped due time is announced once: %+v", m)
		}
	}
}

func TestAlertsDailySummary(t *testing.T) {
	e, sk := alertEnv(t, func(c *config.Config) { c.Alerts.DailySummary = "08:30" })
	ctx := context.Background()
	a := e.App
	// the clock is the injected one
	morning := time.Date(2026, 10, 20, 8, 29, 0, 0, time.Local)
	setClock(e, morning)
	a.AlertsTick(ctx, morning)
	if got := sk.take(""); hasKind(got, alerts.KindSummary) {
		t.Fatalf("not yet 08:30: %+v", got)
	}
	at := time.Date(2026, 10, 20, 8, 31, 0, 0, time.Local)
	setClock(e, at)
	a.AlertsTick(ctx, at)
	var sums []alerts.Payload
	for _, m := range sk.take("") {
		if m.Kind == alerts.KindSummary {
			sums = append(sums, m)
		}
	}
	if len(sums) != 1 {
		t.Fatalf("one summary expected: %+v", sums)
	}
	states, _, _ := a.States()
	want := app.Summarize(states, false, nil)
	if sums[0].Title != want.Text {
		t.Errorf("the summary uses the same sentence as the interface: %q vs %q", sums[0].Title, want.Text)
	}
	body := strings.Join(sums[0].Lines, "\n")
	if !strings.Contains(body, "pg-main") || !strings.Contains(body, "last backup") || !strings.Contains(body, "streaming live") {
		t.Errorf("one line per server with its facts:\n%s", sums[0].Text)
	}
	if sums[0].URL != "https://cellar.example.com/status" {
		t.Errorf("url %q", sums[0].URL)
	}
	a.AlertsTick(ctx, at.Add(time.Hour))
	if got := sk.take(""); hasKind(got, alerts.KindSummary) {
		t.Fatalf("one summary a day: %+v", got)
	}
	next := at.Add(24 * time.Hour)
	setClock(e, next)
	a.AlertsTick(ctx, next)
	if got := sk.take(""); !hasKind(got, alerts.KindSummary) {
		t.Fatal("the next day gets its own summary")
	}
}

func hasKind(ms []alerts.Payload, kind string) bool {
	for _, m := range ms {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

func TestSendTestAlert(t *testing.T) {
	e, sk := alertEnv(t, nil)
	chans := e.App.AlertChannels()
	if len(chans) != 1 || chans[0].Name != "webhook" || !chans[0].Ready {
		t.Fatalf("channels: %+v", chans)
	}
	res, err := e.App.SendTestAlert(context.Background(), "admin-user", "webhook")
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("test: %+v %v", res, err)
	}
	if got := sk.take(""); len(got) != 1 || got[0].Kind != alerts.KindTest {
		t.Errorf("received %+v", got)
	}
	if _, err := e.App.SendTestAlert(context.Background(), "admin-user", "telegram"); err == nil {
		t.Error("an unconfigured channel is refused")
	}
	audit, _ := e.App.Audit(10)
	if len(audit) == 0 || audit[0].Action != "alerts.test" {
		t.Errorf("the test must be audited: %+v", audit)
	}
	counts := e.App.AlertCounts()
	if len(counts) != 1 || counts[0].Channel != "webhook" || !counts[0].OK || counts[0].N != 1 {
		t.Errorf("counts %+v", counts)
	}
}

func TestAlertsSilentWithoutChannel(t *testing.T) {
	e := apptest.New(t)
	e.App.AlertsTick(context.Background(), day0)
	if j, _ := e.App.AlertJournal(10); len(j) != 0 {
		t.Errorf("no channel, no send: %+v", j)
	}
	if _, err := e.App.SendTestAlert(context.Background(), "x", ""); err == nil {
		t.Error("no channel configured must be said")
	}
}

// A state reloaded from the database after a restart is not news.
func TestAlertsIgnoreStaleCache(t *testing.T) {
	e, sk := alertEnv(t, nil)
	ctx := context.Background()
	e.App.AlertsTick(ctx, day0)
	later := day0.Add(72 * time.Hour)
	setClock(e, later) // the red state is now in the database
	a2, err := app.Load(e.App.ConfigPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a2.Close() }()
	a2.Now = func() time.Time { return later }
	if !a2.CacheStale() {
		t.Fatal("a freshly started App must hold a stale cache")
	}
	a2.AlertsTick(ctx, later)
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("a stale cache must not trigger anything: %+v", got)
	}
}

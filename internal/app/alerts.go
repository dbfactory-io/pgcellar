package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stranix79/pgcellar/internal/alerts"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/store"
)

// AlertStartGrace is how long after startup pgCellar stays quiet: the status
// cache is being rebuilt and a server can look red for a minute for no reason.
const AlertStartGrace = 2 * time.Minute

// DefaultAlertsInterval is how often the alert loop looks at the state.
const DefaultAlertsInterval = 30 * time.Second

const (
	settingAlertState = "alerts.state"
	alertJournalKeep  = 2000
	maxAlertLines     = 5
)

// alertServer is what pgCellar remembers of the last announcement about a
// server: the level it announced, since when, and when it last said so.
type alertServer struct {
	Level  string    `json:"level"`
	Since  time.Time `json:"since"`
	SentAt time.Time `json:"sent_at"`
}

// alertState is persisted in the settings table, so a restart neither repeats
// nor forgets what was announced.
type alertState struct {
	Init        bool                    `json:"init"`
	Servers     map[string]*alertServer `json:"servers"`
	LastRunID   int64                   `json:"last_run_id"`
	Alerted     []int64                 `json:"alerted"` // finished runs above LastRunID already announced
	LastDrillID int64                   `json:"last_drill_id"`
	Skipped     map[string]string       `json:"skipped"` // "backup:srv" -> due time announced
	DailyDate   string                  `json:"daily_date"`
}

type alertEngine struct {
	mu sync.Mutex
}

// AlertChannelInfo describes a configured channel for the interface: never a
// secret, only whether the environment variables it needs are set.
type AlertChannelInfo struct {
	Name   string             `json:"name"`
	Target string             `json:"target"`
	Env    []alerts.EnvStatus `json:"env"`
	Ready  bool               `json:"ready"` // every variable it needs is set
}

// AlertSentView is one line of the journal.
type AlertSentView struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Channel string    `json:"channel"`
	Kind    string    `json:"kind"`
	Server  string    `json:"server,omitempty"`
	Level   string    `json:"level"`
	Text    string    `json:"text"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
}

func (a *App) alertChannels() []alerts.Channel {
	return alerts.Channels(a.Config().Alerts, a.AlertOptions)
}

// AlertChannels lists the configured channels.
func (a *App) AlertChannels() []AlertChannelInfo {
	out := []AlertChannelInfo{}
	for _, c := range a.alertChannels() {
		info := AlertChannelInfo{Name: c.Name(), Target: c.Target(), Env: c.Env(), Ready: true}
		if info.Env == nil {
			info.Env = []alerts.EnvStatus{}
		}
		for _, e := range info.Env {
			if !e.Set {
				info.Ready = false
			}
		}
		out = append(out, info)
	}
	return out
}

// AlertJournal returns the latest sends, newest first.
func (a *App) AlertJournal(limit int) ([]AlertSentView, error) {
	rows, err := a.Store.ListAlerts(limit)
	if err != nil {
		return nil, err
	}
	out := make([]AlertSentView, 0, len(rows))
	for _, r := range rows {
		out = append(out, AlertSentView{ID: r.ID, At: r.At, Channel: r.Channel, Kind: r.Kind, Server: r.Server, Level: r.Level, Text: r.Text, OK: r.OK, Error: r.Error})
	}
	return out, nil
}

// AlertCounts is the number of sends by channel and outcome (for /metrics).
func (a *App) AlertCounts() []store.AlertCount {
	n, _ := a.Store.CountAlerts()
	return n
}

// deliver sends a message and records every attempt in the journal. A failure
// is logged and returned, never fatal.
func (a *App) deliver(ctx context.Context, chans []alerts.Channel, only string, m alerts.Message) []alerts.Result {
	if m.At.IsZero() {
		m.At = a.now()
	}
	res := alerts.Send(ctx, chans, only, m)
	for _, r := range res {
		if !r.OK {
			a.Logf("alert via %s failed: %s", r.Channel, r.Error)
		}
		if err := a.Store.AddAlert(store.AlertSent{At: m.At, Channel: r.Channel, Kind: m.Kind, Server: m.Server, Level: m.Level, Text: m.Subject(), OK: r.OK, Error: r.Error}); err != nil {
			a.Logf("cannot record the alert: %v", err)
		}
	}
	_ = a.Store.PruneAlerts(alertJournalKeep)
	return res
}

// SendTestAlert sends a test message to one channel ("" = all) and audits it.
func (a *App) SendTestAlert(ctx context.Context, actor, channel string) ([]alerts.Result, error) {
	chans := a.alertChannels()
	known := false
	for _, c := range chans {
		if channel == "" || c.Name() == channel {
			known = true
		}
	}
	if !known {
		if channel == "" {
			return nil, &ConflictError{Msg: "No alert channel is configured: add telegram, mail or webhook under alerts: in pgcellar.yaml."}
		}
		return nil, fmt.Errorf("%w: no alert channel %q is configured", ErrNotFound, channel)
	}
	m := alerts.Message{
		Kind: alerts.KindTest, Level: alerts.LevelOK, Title: "Test message from pgCellar",
		Lines: []string{"If you can read this, this channel works."},
		URL:   a.alertURL("/alerts"), At: a.now(),
	}
	res := a.deliver(ctx, chans, channel, m)
	var parts []string
	for _, r := range res {
		if r.OK {
			parts = append(parts, r.Channel+" ok")
		} else {
			parts = append(parts, r.Channel+" failed")
		}
	}
	_ = a.Store.AddAudit(actor, "alerts.test", channel, strings.Join(parts, ", "))
	return res, nil
}

func (a *App) alertURL(path string) string {
	base := strings.TrimRight(strings.TrimSpace(a.Config().BaseURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

func (a *App) loadAlertState() alertState {
	var st alertState
	if v, ok, err := a.Store.GetSetting(settingAlertState); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &st)
	}
	if st.Servers == nil {
		st.Servers = map[string]*alertServer{}
	}
	if st.Skipped == nil {
		st.Skipped = map[string]string{}
	}
	return st
}

func (a *App) saveAlertState(st alertState) {
	b, err := json.Marshal(st)
	if err == nil {
		err = a.Store.SetSetting(settingAlertState, string(b))
	}
	if err != nil {
		a.Logf("cannot save the alert state: %v", err)
	}
}

func (a *App) alertsLoop(ctx context.Context) {
	iv := a.AlertsInterval
	if iv <= 0 {
		iv = DefaultAlertsInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.AlertsTick(ctx, a.now())
		}
	}
}

// AlertsTick looks at everything once with `now` as the clock and sends what
// deserves a message. Nothing is sent during the first AlertStartGrace after
// AlertsSince, and without a configured channel nothing happens at all.
func (a *App) AlertsTick(ctx context.Context, now time.Time) {
	if a.Store == nil {
		return
	}
	a.alerts.mu.Lock()
	if !a.AlertsSince.IsZero() && now.Sub(a.AlertsSince) < AlertStartGrace {
		a.alerts.mu.Unlock()
		return
	}
	chans := a.alertChannels()
	if len(chans) == 0 {
		a.alerts.mu.Unlock()
		return
	}
	cfg := a.Config()
	st := a.loadAlertState()
	var msgs []alerts.Message
	msgs = append(msgs, a.alertRuns(&st)...)
	msgs = append(msgs, a.alertSkipped(&st, cfg)...)
	if !a.CacheStale() { // a state reloaded from the database says nothing
		states, _, _ := a.States()
		msgs = append(msgs, a.alertLevels(&st, cfg, states, now)...)
		if m := a.alertDaily(&st, cfg, states, now); m != nil {
			msgs = append(msgs, *m)
		}
	}
	st.Init = true
	a.saveAlertState(st) // before sending: at most once, even if pgCellar stops in the middle
	a.alerts.mu.Unlock()
	for _, m := range msgs {
		a.deliver(ctx, chans, "", m)
	}
}

// ---- runs and drills ----

func (a *App) alertRuns(st *alertState) []alerts.Message {
	runs, err := a.Store.ListRuns("", 300)
	if err != nil {
		return nil
	}
	drills, _ := a.Store.ListDrills("", 100)
	if !st.Init { // first look: what happened before is not news
		for _, r := range runs {
			st.LastRunID = max(st.LastRunID, r.ID)
		}
		for _, d := range drills {
			st.LastDrillID = max(st.LastDrillID, d.ID)
		}
		return nil
	}
	var out []alerts.Message
	alerted := map[int64]bool{}
	for _, id := range st.Alerted {
		alerted[id] = true
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	blocked := false // a run still going holds LastRunID back, the ones after it are remembered in Alerted
	for _, r := range runs {
		if r.ID <= st.LastRunID {
			continue
		}
		if r.FinishedAt == nil {
			blocked = true
			continue
		}
		if !alerted[r.ID] {
			alerted[r.ID] = true
			if m := a.backupFailedMessage(r); m != nil {
				out = append(out, *m)
			}
		}
		if !blocked {
			st.LastRunID = r.ID
		}
	}
	st.Alerted = st.Alerted[:0]
	for id := range alerted {
		if id > st.LastRunID {
			st.Alerted = append(st.Alerted, id)
		}
	}
	sort.Slice(st.Alerted, func(i, j int) bool { return st.Alerted[i] < st.Alerted[j] })

	sort.Slice(drills, func(i, j int) bool { return drills[i].ID < drills[j].ID })
	for _, d := range drills {
		if d.ID <= st.LastDrillID {
			continue
		}
		st.LastDrillID = d.ID
		if m := a.drillMessage(d); m != nil {
			out = append(out, *m)
		}
	}
	return out
}

func (a *App) backupFailedMessage(r store.Run) *alerts.Message {
	if r.Kind != store.KindBackup || r.ExitCode == nil || *r.ExitCode == 0 {
		return nil
	}
	if *r.ExitCode == -1 && strings.HasPrefix(r.StderrTail, "interrupted:") {
		return nil // pgCellar was restarted during the run: not a failure of the backup
	}
	how := "started by " + r.Actor
	if r.Actor == SchedulerActor {
		how = "scheduled"
	}
	m := &alerts.Message{
		Kind: alerts.KindBackup, Server: r.Server, Level: alerts.LevelFail,
		Title:  fmt.Sprintf("Backup of %s failed", r.Server),
		Lines:  []string{fmt.Sprintf("The backup (%s, run #%d) ended with exit code %d.", how, r.ID, *r.ExitCode)},
		Action: "Open the run, read what Barman said, fix the cause (the server page shows the failing checks), then start a backup with \"Backup now\".",
		URL:    a.alertURL(fmt.Sprintf("/runs/%d", r.ID)),
		At:     a.now(),
	}
	if why := lastLine(r.StderrTail); why != "" {
		m.Lines = append(m.Lines, "Barman said: "+why)
	}
	return m
}

func (a *App) drillMessage(d store.Drill) *alerts.Message {
	v := drillView(d)
	if d.OK {
		if !a.Config().Alerts.OnDrillSuccess {
			return nil
		}
		m := &alerts.Message{
			Kind: alerts.KindDrillOK, Server: d.Server, Level: alerts.LevelOK,
			Title: fmt.Sprintf("Restore drill of %s passed", d.Server),
			URL:   a.alertURL("/servers/" + d.Server), At: a.now(),
		}
		if v.Details.Summary != "" {
			m.Lines = []string{v.Details.Summary}
		}
		return m
	}
	m := &alerts.Message{
		Kind: alerts.KindDrill, Server: d.Server, Level: alerts.LevelFail,
		Title:  fmt.Sprintf("Restore drill of %s failed", d.Server),
		Lines:  []string{fmt.Sprintf("It failed at the step \"%s\": the backups of this server are not proven restorable.", DrillStepLabel(d.StepFailed))},
		Action: "Open the drill on the server page, read the failing step and its log, fix it, then run the drill again.",
		URL:    a.alertURL("/servers/" + d.Server), At: a.now(),
	}
	if v.Details.Error != "" {
		m.Lines = append(m.Lines, shorten(v.Details.Error, 300))
	}
	return m
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return shorten(strings.TrimSpace(lines[len(lines)-1]), 300)
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---- skipped schedules ----

func (a *App) alertSkipped(st *alertState, cfg *config.Config) []alerts.Message {
	var out []alerts.Message
	for _, kind := range []int{kindBackup, kindDrill} {
		for _, srv := range cfg.Servers {
			if specOf(cfg, kind, srv) == "" {
				continue
			}
			rec, ok := a.lastScheduledKind(kind, srv.Name)
			if !ok || rec.RunID != 0 || rec.Note == "" {
				continue
			}
			key := "backup:" + srv.Name
			what := "backup"
			if kind == kindDrill {
				key, what = "drill:"+srv.Name, "restore drill"
			}
			due := rec.DueAt.UTC().Format(time.RFC3339)
			if st.Skipped[key] == due {
				continue
			}
			st.Skipped[key] = due
			if !st.Init {
				continue
			}
			out = append(out, alerts.Message{
				Kind: alerts.KindSkipped, Server: srv.Name, Level: alerts.LevelWarn,
				Title:  fmt.Sprintf("Scheduled %s of %s did not run", what, srv.Name),
				Lines:  []string{fmt.Sprintf("It was due at %s and was not started: %s", rec.DueAt.Local().Format("2006-01-02 15:04"), strings.TrimSuffix(rec.Note, "."))},
				Action: "Open the server page to see what is running, and start it by hand when it is free.",
				URL:    a.alertURL("/servers/" + srv.Name), At: a.now(),
			})
		}
	}
	return out
}

// ---- levels ----

func (a *App) alertLevels(st *alertState, cfg *config.Config, states []ServerState, now time.Time) []alerts.Message {
	var out []alerts.Message
	repeat := cfg.Alerts.RepeatEvery
	seen := map[string]bool{}
	for _, s := range states {
		seen[s.Name] = true
		prev := st.Servers[s.Name]
		switch {
		case prev == nil && s.Level == LevelOK:
			st.Servers[s.Name] = &alertServer{Level: LevelOK, Since: now, SentAt: now}
		case prev == nil:
			st.Servers[s.Name] = &alertServer{Level: s.Level, Since: now, SentAt: now}
			out = append(out, a.levelMessage(s, alerts.KindLevel, time.Time{}, now))
		case prev.Level == s.Level && s.Level == LevelOK:
			// still fine
		case prev.Level == s.Level:
			if repeat > 0 && now.Sub(prev.SentAt) >= repeat {
				prev.SentAt = now
				out = append(out, a.levelMessage(s, alerts.KindReminder, prev.Since, now))
			}
		case s.Level == LevelOK:
			out = append(out, a.levelMessage(s, alerts.KindResolved, prev.Since, now))
			st.Servers[s.Name] = &alertServer{Level: LevelOK, Since: now, SentAt: now}
		default: // ok -> bad, or warn <-> fail
			since := prev.Since
			if prev.Level == LevelOK {
				since = now
			}
			st.Servers[s.Name] = &alertServer{Level: s.Level, Since: since, SentAt: now}
			out = append(out, a.levelMessage(s, alerts.KindLevel, time.Time{}, now))
		}
	}
	for name := range st.Servers {
		if !seen[name] {
			delete(st.Servers, name)
		}
	}
	return out
}

// levelMessage words the announcement about one server.
func (a *App) levelMessage(s ServerState, kind string, since, now time.Time) alerts.Message {
	m := alerts.Message{Kind: kind, Server: s.Name, Level: s.Level, URL: a.alertURL("/servers/" + s.Name), At: now}
	long := ""
	if !since.IsZero() {
		long = " (for " + HumanDuration(now.Sub(since)) + ")"
	}
	switch {
	case kind == alerts.KindResolved:
		m.Level = alerts.LevelOK
		m.Title = fmt.Sprintf("%s is healthy again", s.Name)
		if !since.IsZero() {
			m.Lines = []string{"The problem lasted " + HumanDuration(now.Sub(since)) + "."}
		}
		return m
	case s.Level == LevelFail && kind == alerts.KindReminder:
		m.Title = fmt.Sprintf("%s is still failing%s", s.Name, long)
	case s.Level == LevelFail:
		m.Title = fmt.Sprintf("%s is failing", s.Name)
	case kind == alerts.KindReminder:
		m.Title = fmt.Sprintf("%s still needs attention%s", s.Name, long)
	default:
		m.Title = fmt.Sprintf("%s needs attention", s.Name)
	}
	seen := map[string]bool{}
	add := func(l string) {
		if l != "" && !seen[l] && len(m.Lines) < maxAlertLines {
			seen[l] = true
			m.Lines = append(m.Lines, l)
		}
	}
	add(s.Error)
	for _, p := range s.Problems {
		add(p)
	}
	if len(m.Lines) == 0 { // the problems say it better; checks only when there are none
		for _, c := range s.Checks {
			if !c.OK {
				add(c.Display + " failed. " + firstSentence(c.What))
			}
		}
	}
	for i, l := range m.Lines {
		m.Lines[i] = shorten(l, 280)
	}
	if len(m.Lines) == 0 && s.LastBackupID == "" {
		add("No backup has been taken yet.")
	}
	m.Action = alertAction(s)
	return m
}

func alertAction(s ServerState) string {
	switch {
	case s.Error != "":
		return fmt.Sprintf("Check that Barman works on this host (run \"barman check %s\" as the barman user), then open the server page.", s.Name)
	}
	for _, c := range s.Checks {
		if !c.OK && c.Fix != "" {
			return shorten(c.Display+": "+c.Fix, 300)
		}
	}
	if s.Drill != nil && s.Drill.Stale {
		return "Open the server page, read the last restore drill and fix its failing step, then run the drill again."
	}
	return "Open the server page, read the problems listed there and start with the first one."
}

// ---- daily summary ----

func (a *App) alertDaily(st *alertState, cfg *config.Config, states []ServerState, now time.Time) *alerts.Message {
	h, mi, ok := config.ParseClock(cfg.Alerts.DailySummary)
	if !ok || len(states) == 0 {
		return nil
	}
	today := now.Format("2006-01-02")
	at := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
	if now.Before(at) || now.Sub(at) > 12*time.Hour || st.DailyDate == today {
		return nil
	}
	st.DailyDate = today
	m := a.SummaryMessage(states, now)
	return &m
}

// SummaryMessage is the daily message: the headline of the Status page and
// one line per server.
func (a *App) SummaryMessage(states []ServerState, now time.Time) alerts.Message {
	sm := Summarize(states, false, nil)
	level := sm.Level
	if level != LevelWarn && level != LevelFail {
		level = LevelOK
	}
	m := alerts.Message{Kind: alerts.KindSummary, Level: level, Title: sm.Text, URL: a.alertURL("/status"), At: now}
	if sm.Sub != "" {
		m.Lines = append(m.Lines, sm.Sub)
	}
	for _, s := range states {
		m.Lines = append(m.Lines, summaryLine(s, now))
	}
	if level != LevelOK {
		m.Action = "Open pgCellar and start with the servers marked ❌, then the ones marked ⚠️."
	}
	return m
}

func summaryLine(s ServerState, now time.Time) string {
	parts := []string{s.Name + " " + alerts.Emoji(s.Level)}
	if s.LastBackupEnd.IsZero() {
		parts = append(parts, "no backup yet")
	} else {
		parts = append(parts, "last backup "+shortAgo(now.Sub(s.LastBackupEnd)))
	}
	if s.StreamingMonitored {
		if s.StreamingLive {
			parts = append(parts, "streaming live")
		} else {
			parts = append(parts, "streaming stopped")
		}
	}
	if s.Drill != nil && (s.Drill.Scheduled || s.Drill.Last != nil) {
		parts = append(parts, strings.ToLower(s.Drill.Text[:1])+s.Drill.Text[1:])
	}
	if s.Storage != nil && s.Storage.FS != nil {
		parts = append(parts, fmt.Sprintf("%s free (%d%% used)", FormatSize(s.Storage.FS.FreeBytes), s.Storage.FS.UsedPct()))
	}
	return strings.Join(parts, " · ")
}

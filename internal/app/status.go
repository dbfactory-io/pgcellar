package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/store"
)

// Thresholds used when alerts.max_backup_age / max_wal_age are not set.
const (
	defaultMaxBackupAge = 26 * time.Hour
	defaultMaxWALAge    = 24 * time.Hour
)

func (a *App) statusLoop(ctx context.Context) {
	interval := a.StatusInterval
	if interval <= 0 {
		interval = time.Minute
	}
	if a.firstCron != nil {
		select {
		case <-ctx.Done():
			return
		case <-a.firstCron:
		}
	}
	a.RefreshStatuses(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.RefreshStatuses(ctx)
		}
	}
}

// serverNames lists the servers Barman knows. When Barman cannot say, the
// names found in barman.d are used and the error is returned too.
func (a *App) serverNames(ctx context.Context) ([]string, error) {
	names, err := a.Engine.ListServers(barman.WithActor(ctx, systemActor))
	if err == nil && len(names) > 0 {
		return names, nil
	}
	files, _ := barman.LoadServers(a.barmanConfigDir())
	var fromFiles []string
	for _, s := range files {
		fromFiles = append(fromFiles, s.Name)
	}
	sort.Strings(fromFiles)
	if err == nil && len(fromFiles) > 0 {
		err = fmt.Errorf("barman lists no server although %d are configured in barman.d", len(fromFiles))
	}
	if err != nil {
		names = fromFiles
	}
	return names, err
}

// RefreshStatuses recomputes the status of every server and replaces the
// cache. It is called every StatusInterval by the background loop.
func (a *App) RefreshStatuses(ctx context.Context) {
	a.refreshOnce.Lock()
	defer a.refreshOnce.Unlock()

	names, listErr := a.serverNames(ctx)
	states := make([]*ServerState, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			states[i] = a.computeState(ctx, n)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	a.cache = map[string]*ServerState{}
	a.cacheOrder = names
	for _, s := range states {
		a.cache[s.Name] = s
	}
	// Real time on purpose: the dev clock is frozen for the fixtures, but
	// "last refresh" must say how long ago the refresh really happened.
	a.cacheTime = time.Now()
	a.cacheStale = false
	a.cacheErr = ""
	if listErr != nil {
		a.cacheErr = fmt.Sprintf("Barman could not list its servers: %v", listErr)
	}
	a.persistStatuses(states)
}

// persistedState is a ServerState with the parts that are not part of its
// public JSON (the backup list).
type persistedState struct {
	ServerState
	Backups []BackupView `json:"backups"`
}

func (a *App) statusRow(st *ServerState) (store.StatusRow, error) {
	c := *st
	c.Stale = false
	b, err := json.Marshal(persistedState{ServerState: c, Backups: st.Backups})
	return store.StatusRow{Server: st.Name, JSON: string(b), UpdatedAt: st.UpdatedAt}, err
}

// persistStatuses keeps the last states in the database so that pages are
// full right after a restart. A failure is logged, never fatal.
func (a *App) persistStatuses(states []*ServerState) {
	rows := make([]store.StatusRow, 0, len(states))
	for _, st := range states {
		if r, err := a.statusRow(st); err == nil {
			rows = append(rows, r)
		}
	}
	if err := a.Store.SaveStatuses(rows); err != nil {
		a.Logf("cannot save the status cache: %v", err)
	}
}

func (a *App) persistStatus(st *ServerState) {
	r, err := a.statusRow(st)
	if err == nil {
		err = a.Store.SaveStatus(r)
	}
	if err != nil {
		a.Logf("cannot save the status of %s: %v", st.Name, err)
	}
}

// loadStatusCache fills the cache from the database, marked stale until the
// first refresh completes. Called once, from Load.
func (a *App) loadStatusCache() {
	rows, err := a.Store.LoadStatuses()
	if err != nil {
		a.Logf("cannot read the saved status cache: %v", err)
		return
	}
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	for _, r := range rows {
		var p persistedState
		if json.Unmarshal([]byte(r.JSON), &p) != nil || p.Name == "" {
			continue
		}
		st := p.ServerState
		st.Backups = p.Backups
		if a.cache == nil {
			a.cache = map[string]*ServerState{}
		}
		a.cache[st.Name] = &st
		a.cacheOrder = append(a.cacheOrder, st.Name)
		if st.UpdatedAt.After(a.cacheTime) {
			a.cacheTime = st.UpdatedAt
		}
	}
	a.cacheStale = len(a.cache) > 0
}

// CacheStale reports whether the cached states come from before the last
// restart and no refresh has completed yet.
func (a *App) CacheStale() bool {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	return a.cacheStale
}

// refreshServer updates one entry of the cache (after a backup, for example).
func (a *App) refreshServer(ctx context.Context, name string) {
	st := a.computeState(ctx, name)
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if _, ok := a.cache[name]; !ok {
		a.cacheOrder = append(a.cacheOrder, name)
	}
	a.cache[name] = st
	a.persistStatus(st)
}

func (a *App) computeState(ctx context.Context, name string) *ServerState {
	bs, err := a.Engine.Status(barman.WithActor(ctx, systemActor), name)
	if err != nil {
		st := &ServerState{Name: name, UpdatedAt: a.now(), Checks: []CheckView{}, Problems: []string{}, Notes: []string{}}
		st.Error = fmt.Sprintf("Barman could not report on %s: %v", name, err)
		st.Level = LevelFail
		return st
	}
	st := a.stateFrom(name, bs)
	a.applySetup(ctx, st, a.cachedState(name), false)
	return st
}

// cachedState returns the cache entry of a server, or nil.
func (a *App) cachedState(name string) *ServerState {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	return a.cache[name]
}

// stateFrom turns a Barman digest into the cached state, with the alert rules.
func (a *App) stateFrom(name string, bs *barman.ServerStatus) *ServerState {
	st := &ServerState{Name: name, UpdatedAt: a.now(), Checks: []CheckView{}, Problems: []string{}, Notes: []string{}, raw: bs}
	if b := bs.LastBackup; b != nil {
		st.LastBackupID = b.ID
		st.LastBackupEnd = b.EndTime
		st.LastBackupSize = b.Size
	}
	st.LastFilledWAL, st.LastFilledWALTime, st.LastWAL = bs.LastWAL, bs.LastWALTime, bs.LastWALTime
	st.StreamingMonitored = barman.HasStreamingCheck(bs.Checks)
	st.StreamingLive = bs.StreamingLive
	st.LastDataReceived = bs.LastDataReceived
	st.BackupCount = bs.BackupCount
	st.Backups = backupViews(bs.Backups)
	st.Checks = checkViews(bs.Checks)
	st.CheckOK = len(bs.Checks) > 0
	for _, c := range bs.Checks {
		if !c.OK {
			st.CheckOK = false
		}
	}
	st.Problems = append(st.Problems, bs.Problems...)

	cfg := a.Config()
	maxBackup, maxWAL := cfg.Alerts.MaxBackupAge, cfg.Alerts.MaxWALAge
	if maxBackup <= 0 {
		maxBackup = defaultMaxBackupAge
	}
	if maxWAL <= 0 {
		maxWAL = defaultMaxWALAge
	}
	now := a.now()
	level := LevelOK
	if st.LastBackupID == "" || len(bs.Checks) == 0 || !st.CheckOK {
		level = LevelFail
	}
	if !st.LastBackupEnd.IsZero() {
		age := now.Sub(st.LastBackupEnd)
		if age > maxBackup {
			st.Problems = append(st.Problems, fmt.Sprintf("The last backup is %s old, more than the %s allowed.", HumanDuration(age), HumanDuration(maxBackup)))
			if age > 2*maxBackup {
				level = LevelFail
			}
		}
	}
	// WAL alerts. On a quiet database a segment closes rarely, so while the
	// receiver is alive the age of the last closed segment is only a note;
	// what counts is when data last arrived.
	segmentAge := time.Duration(-1)
	if !st.LastFilledWALTime.IsZero() {
		segmentAge = now.Sub(st.LastFilledWALTime)
	}
	switch {
	case st.StreamingMonitored && st.StreamingLive:
		if !st.LastDataReceived.IsZero() {
			if age := now.Sub(st.LastDataReceived); age > maxWAL {
				st.Problems = append(st.Problems, fmt.Sprintf("No WAL data has arrived for %s, more than the %s allowed.", HumanDuration(age), HumanDuration(maxWAL)))
			}
		}
		if segmentAge > maxWAL {
			st.Notes = append(st.Notes, fmt.Sprintf("The last filled WAL file is %s old. %s", HumanDuration(segmentAge), archiveTimeoutAdvice))
		}
	default:
		if segmentAge > maxWAL {
			st.Problems = append(st.Problems, fmt.Sprintf("The last filled WAL file is %s old, more than the %s allowed.", HumanDuration(segmentAge), HumanDuration(maxWAL)))
		}
	}
	if level == LevelOK && len(st.Problems) > 0 {
		level = LevelWarn
	}
	st.Level = level
	return st
}

// archiveTimeoutAdvice is shown when a quiet server has not closed a segment
// for a long time although streaming is alive.
const archiveTimeoutAdvice = "For a bounded recovery point on a quiet server, set archive_timeout on PostgreSQL (for example 1h)."

// backupViews converts a list-backups result (oldest first) to views, newest first.
func backupViews(list []barman.Backup) []BackupView {
	out := make([]BackupView, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		b := list[i]
		out = append(out, BackupView{ID: b.ID, Status: b.Status, BeginTime: b.BeginTime, EndTime: b.EndTime, SizeBytes: b.Size, WALSizeBytes: b.WALSize, PGVersion: b.PGVersion})
	}
	return out
}

// applyCheck puts the result of an on-demand check into the cache of one
// server, so that the pages show it without waiting for the next refresh.
func (a *App) applyCheck(ctx context.Context, name string, checks []barman.CheckResult) {
	a.cacheMu.Lock()
	old, ok := a.cache[name]
	a.cacheMu.Unlock()
	if !ok || old.raw == nil {
		return
	}
	st := a.stateFrom(name, old.raw.WithChecks(checks))
	a.applySetup(ctx, st, old, true)
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if cur, ok := a.cache[name]; ok && cur == old {
		a.cache[name] = st
		a.persistStatus(st)
	}
}

func checkViews(checks []barman.CheckResult) []CheckView {
	out := make([]CheckView, 0, len(checks))
	for _, c := range checks {
		what, fix := barman.Explain(c)
		v := CheckView{Name: c.Name, Display: barman.DisplayName(c.Name), OK: c.OK, What: what}
		if !c.OK {
			v.Fix = fix
		}
		out = append(out, v)
	}
	return out
}

// HumanDuration renders a duration for people: "3 days", "5 h 12 min".
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// States returns the cached state of every server, in Barman's order, with
// ages computed at the time of the call, and when the cache was built.
func (a *App) States() ([]ServerState, time.Time, string) {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	now := a.now()
	out := make([]ServerState, 0, len(a.cacheOrder))
	for _, n := range a.cacheOrder {
		if s, ok := a.cache[n]; ok {
			v := withAges(*s, now)
			v.Stale = a.cacheStale
			out = append(out, v)
		}
	}
	return out, a.cacheTime, a.cacheErr
}

// State returns the cached state of one server.
func (a *App) State(name string) (ServerState, bool) {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	s, ok := a.cache[name]
	if !ok {
		return ServerState{}, false
	}
	v := withAges(*s, a.now())
	v.Stale = a.cacheStale
	return v, true
}

func withAges(s ServerState, now time.Time) ServerState {
	if !s.LastDataReceived.IsZero() {
		v := int64(now.Sub(s.LastDataReceived).Seconds())
		s.LastDataReceivedAgeSeconds = &v
	}
	if !s.LastBackupEnd.IsZero() {
		v := int64(now.Sub(s.LastBackupEnd).Seconds())
		s.LastBackupAgeSeconds = &v
	}
	if !s.LastFilledWALTime.IsZero() {
		v := int64(now.Sub(s.LastFilledWALTime).Seconds())
		s.LastFilledWALAgeSeconds = &v
		s.LastWALAgeSeconds = &v
	}
	return s
}

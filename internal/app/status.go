package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
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
	if err == nil {
		return names, nil
	}
	files, _ := barman.LoadServers(a.barmanConfigDir())
	for _, s := range files {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names, err
}

// RefreshStatuses recomputes the status of every server and replaces the
// cache. It is called every minute by the background loop.
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
	a.cacheErr = ""
	if listErr != nil {
		a.cacheErr = fmt.Sprintf("Barman could not list its servers: %v", listErr)
	}
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
}

func (a *App) computeState(ctx context.Context, name string) *ServerState {
	st := &ServerState{Name: name, UpdatedAt: a.now(), Checks: []CheckView{}, Problems: []string{}}
	bs, err := a.Engine.Status(barman.WithActor(ctx, systemActor), name)
	if err != nil {
		st.Error = fmt.Sprintf("Barman could not report on %s: %v", name, err)
		st.Level = LevelFail
		return st
	}
	if b := bs.LastBackup; b != nil {
		st.LastBackupID = b.ID
		st.LastBackupEnd = b.EndTime
		st.LastBackupSize = b.Size
	}
	st.LastWAL = bs.LastWALTime
	st.BackupCount = bs.BackupCount
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
	if !st.LastWAL.IsZero() {
		if age := now.Sub(st.LastWAL); age > maxWAL {
			st.Problems = append(st.Problems, fmt.Sprintf("The last archived WAL file is %s old, more than the %s allowed.", HumanDuration(age), HumanDuration(maxWAL)))
		}
	}
	if level == LevelOK && len(st.Problems) > 0 {
		level = LevelWarn
	}
	st.Level = level
	return st
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
			out = append(out, withAges(*s, now))
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
	return withAges(*s, a.now()), true
}

func withAges(s ServerState, now time.Time) ServerState {
	if !s.LastBackupEnd.IsZero() {
		v := int64(now.Sub(s.LastBackupEnd).Seconds())
		s.LastBackupAgeSeconds = &v
	}
	if !s.LastWAL.IsZero() {
		v := int64(now.Sub(s.LastWAL).Seconds())
		s.LastWALAgeSeconds = &v
	}
	return s
}

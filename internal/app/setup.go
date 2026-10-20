package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
)

const (
	// setupRetry is how soon a failed read of the PostgreSQL settings is tried
	// again, whatever setup_interval says: a server that was unreachable should
	// not stay "unreadable" for half an hour after it came back.
	setupRetry = 5 * time.Minute
	// setupTimeout bounds the two short commands of one read.
	setupTimeout = 30 * time.Second
)

// SetupView is the answer of the setup endpoint.
type SetupView struct {
	Server string              `json:"server"`
	Level  string              `json:"level"` // ok | warning | error | unknown
	At     time.Time           `json:"at,omitzero"`
	Checks []barman.SetupCheck `json:"checks"`
}

// Setup returns what pgCellar last read in the PostgreSQL settings of a
// server. It never runs anything: the cache is refreshed every setup_interval.
func (a *App) Setup(name string) (SetupView, error) {
	st, ok := a.State(name)
	if !ok {
		files, _ := barman.LoadServers(a.barmanConfigDir())
		for _, f := range files {
			if f.Name == name {
				return SetupView{Server: name, Level: "unknown", Checks: []barman.SetupCheck{}}, nil
			}
		}
		return SetupView{}, ErrNotFound
	}
	v := SetupView{Server: name, Level: st.SetupLevel, At: st.SetupAt, Checks: st.Setup}
	if v.Level == "" {
		v.Level = "unknown"
	}
	if v.Checks == nil {
		v.Checks = []barman.SetupCheck{}
	}
	return v, nil
}

func unreadableOnly(list []barman.SetupCheck) bool {
	return len(list) == 1 && list[0].Key == barman.SetupKeyUnreadable
}

// applySetup fills the Server setup part of a freshly built state. The
// PostgreSQL settings are read when the previous result is older than
// setup_interval (or force is set); in between, the previous lines are kept
// and only the three lines that come from `barman check` are recomputed.
// A setup warning never changes the health level; a setup error does, because
// it makes Barman fail anyway. Not being able to read the settings does not:
// Barman's own checks say whether the server is reachable.
func (a *App) applySetup(ctx context.Context, st, prev *ServerState, force bool) {
	if a.Tools == nil {
		return
	}
	files, _ := barman.LoadServers(a.barmanConfigDir())
	var srv *barman.Server
	for i := range files {
		if files[i].Name == st.Name {
			srv = &files[i]
			break
		}
	}
	if srv == nil || srv.ConnInfo == "" {
		return
	}

	now := a.now()
	interval := a.SetupInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	var rows []barman.SetupCheck
	at := now
	reuse := !force && prev != nil && len(prev.Setup) > 0 && !prev.SetupAt.IsZero()
	if reuse {
		age := now.Sub(prev.SetupAt)
		limit := interval
		if unreadableOnly(prev.Setup) && setupRetry < limit {
			limit = setupRetry
		}
		reuse = age >= 0 && age < limit
	}
	if reuse {
		at = prev.SetupAt
		for _, c := range prev.Setup {
			if !c.IsDerived() {
				rows = append(rows, c)
			}
		}
	} else {
		if barman.ActorFrom(ctx) == "" {
			ctx = barman.WithActor(ctx, systemActor)
		}
		rctx, cancel := context.WithTimeout(ctx, setupTimeout)
		rows = barman.ReadSetup(rctx, a.Tools, a.SetupTools, *srv)
		cancel()
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			if prev != nil {
				st.Setup, st.SetupLevel, st.SetupIssues, st.SetupAt = prev.Setup, prev.SetupLevel, prev.SetupIssues, prev.SetupAt
			}
			return
		}
	}
	if !unreadableOnly(rows) && st.raw != nil {
		rows = append(rows, barman.DerivedSetup(st.raw.Checks, *srv)...)
	}
	st.Setup, st.SetupAt = rows, at
	st.SetupLevel = barman.WorstSetup(rows)
	errs := 0
	for _, c := range rows {
		switch c.Level {
		case barman.SetupWarning:
			st.SetupIssues++
		case barman.SetupError:
			st.SetupIssues++
			if c.Key != barman.SetupKeyUnreadable {
				errs++
			}
		}
	}
	if errs > 0 {
		st.Level = LevelFail
		st.Problems = append(st.Problems, fmt.Sprintf("Server setup: %d setting(s) to change on the PostgreSQL server, see the Server setup section.", errs))
	}
}

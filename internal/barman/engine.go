package barman

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// CLIEngine implements Engine on top of a Runner.
type CLIEngine struct {
	Runner Runner
}

// NewEngine returns an Engine that drives Barman through r.
func NewEngine(r Runner) *CLIEngine { return &CLIEngine{Runner: r} }

var _ Engine = (*CLIEngine)(nil)

// runJSON executes `barman --format json <args>`. A non-zero exit is not fatal
// when Barman still printed a JSON object (check does that); the error is
// returned together with the output so callers decide.
func (e *CLIEngine) runJSON(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--format", "json"}, args...)
	out, errOut, err := e.Runner.Run(ctx, full...)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if msg := OutputError(out); msg != "" {
			return out, fmt.Errorf("barman: %s", msg)
		}
		if len(errOut) > 0 && !strings.Contains(err.Error(), strings.TrimSpace(string(errOut))) {
			return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(errOut)))
		}
	}
	return out, err
}

func (e *CLIEngine) ListServers(ctx context.Context) ([]string, error) {
	out, err := e.runJSON(ctx, "list-servers", "--minimal")
	if err != nil {
		return nil, err
	}
	return ParseListServers(out)
}

// Check runs `barman check`. A failing check makes Barman exit with status 1
// but the JSON is complete, so that case returns the results and no error.
func (e *CLIEngine) Check(ctx context.Context, server string) ([]CheckResult, error) {
	out, err := e.runJSON(ctx, "check", server)
	res, perr := ParseCheck(out, server)
	if perr == nil {
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, perr
}

func (e *CLIEngine) ListBackups(ctx context.Context, server string) ([]Backup, error) {
	out, err := e.runJSON(ctx, "list-backups", server)
	if err != nil {
		return nil, err
	}
	list, err := ParseListBackups(out, server)
	if err != nil {
		return nil, err
	}
	// Oldest first, by id (ids are timestamps).
	sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

func (e *CLIEngine) ShowBackup(ctx context.Context, server, backupID string) (*Backup, error) {
	out, err := e.runJSON(ctx, "show-backup", server, backupID)
	if err != nil {
		return nil, err
	}
	return ParseShowBackup(out, server)
}

// Status builds the digest of a server from `status`, `list-backups` and
// `check`. A sub-command that fails does not abort the digest: it becomes a
// problem, because "Barman could not tell" is itself something to show. Only a
// cancelled context is returned as an error.
func (e *CLIEngine) Status(ctx context.Context, server string) (*ServerStatus, error) {
	st := &ServerStatus{Server: server}

	if out, err := e.runJSON(ctx, "status", server); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		st.Problems = append(st.Problems, fmt.Sprintf("Barman could not report the status of %s: %v.", server, err))
	} else if info, perr := ParseStatus(out, server); perr != nil {
		st.Problems = append(st.Problems, fmt.Sprintf("The status of %s could not be read: %v.", server, perr))
	} else {
		st.LastWALTime = info.LastWALTime
		if !info.Active {
			st.Problems = append(st.Problems, fmt.Sprintf("Server %s is inactive or disabled in Barman, so nothing is backed up.", server))
		}
	}

	backups, err := e.ListBackups(ctx, server)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		st.Problems = append(st.Problems, fmt.Sprintf("Barman could not list the backups of %s: %v.", server, err))
	} else {
		st.BackupCount = len(backups)
		var last *Backup
		var lastBad *Backup
		for i := range backups {
			b := &backups[i]
			switch b.Status {
			case "DONE":
				last = b
			case "FAILED":
				lastBad = b
			}
		}
		if last != nil {
			if full, err := e.ShowBackup(ctx, server, last.ID); err == nil {
				if full.Size == 0 {
					full.Size = last.Size
				}
				if full.EndTime.IsZero() {
					full.EndTime = last.EndTime
				}
				st.LastBackup = full
			} else {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				st.LastBackup = last
			}
		}
		switch {
		case len(backups) == 0:
			st.Problems = append(st.Problems, fmt.Sprintf("No backup has been taken yet for %s: run the first one.", server))
		case last == nil:
			st.Problems = append(st.Problems, fmt.Sprintf("No completed backup exists for %s yet.", server))
		}
		if lastBad != nil && (last == nil || lastBad.ID > last.ID) {
			st.Problems = append(st.Problems, fmt.Sprintf("The most recent backup attempt (%s) failed: look at the backup log and take a new one.", lastBad.ID))
		}
	}

	checks, err := e.Check(ctx, server)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		st.Problems = append(st.Problems, fmt.Sprintf("Barman could not run its checks on %s: %v.", server, err))
	} else {
		st.Checks = checks
		for _, c := range checks {
			if c.OK {
				continue
			}
			what, fix := Explain(c)
			st.Problems = append(st.Problems, fmt.Sprintf("%s failed. %s %s", DisplayName(c.Name), what, fix))
		}
	}

	st.Healthy = len(st.Problems) == 0
	return st, nil
}

// runPlain runs a command whose output is only shown to the user, not parsed.
func (e *CLIEngine) runPlain(ctx context.Context, args ...string) error {
	_, errOut, err := e.Runner.Run(ctx, args...)
	if err != nil && len(errOut) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(errOut)))
	}
	return err
}

func (e *CLIEngine) Backup(ctx context.Context, server string) error {
	return e.runPlain(ctx, "backup", server)
}

func (e *CLIEngine) SwitchWAL(ctx context.Context, server string) error {
	return e.runPlain(ctx, "switch-wal", "--force", "--archive", server)
}

func (e *CLIEngine) Cron(ctx context.Context) error {
	return e.runPlain(ctx, "cron")
}

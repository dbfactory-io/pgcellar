package barman

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// CLIEngine implements Engine on top of a Runner.
type CLIEngine struct {
	Runner Runner
	// BarmanHome is barman_home of barman.conf; the WAL catalogue of a server
	// is read from <BarmanHome>/<server>/wals. Empty means /var/lib/barman.
	BarmanHome string

	mu       sync.Mutex
	lastFull map[string]*Backup // server -> last `show-backup` result, reused while the last backup id stays the same
}

func (e *CLIEngine) barmanHome() string {
	if e.BarmanHome == "" {
		return DefaultBarmanHome
	}
	return e.BarmanHome
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
	out, err := e.runJSON(ctx, "list-servers")
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
		st.LastWAL, st.LastWALTime = info.LastWAL, info.LastWALTime
		// Barman 3.20 no longer prints last_archived_wal in `status`: read
		// the WAL catalogue file instead (instant, unlike `diagnose`).
		if info.Active && info.LastWALTime.IsZero() {
			path, name, when, werr := ReadLastWAL(e.barmanHome(), server)
			switch {
			case werr != nil:
				st.Problems = append(st.Problems, fmt.Sprintf("Last WAL unknown: cannot read %s: %v.", path, werr))
			default:
				st.LastWAL, st.LastWALTime = name, when
			}
		}
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
		st.Backups = backups
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
			if full := e.cachedFull(server, last.ID); full != nil {
				st.LastBackup = full
			} else if full, err := e.ShowBackup(ctx, server, last.ID); err == nil {
				if full.Size == 0 {
					full.Size = last.Size
				}
				if full.EndTime.IsZero() {
					full.EndTime = last.EndTime
				}
				e.storeFull(server, full)
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

	st.BaseProblems = append([]string(nil), st.Problems...)
	st.LastDataReceived = ReadLastReceived(e.barmanHome(), server)

	checks, err := e.Check(ctx, server)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		st.Problems = append(st.Problems, fmt.Sprintf("Barman could not run its checks on %s: %v.", server, err))
	} else {
		st.Checks = checks
		st.StreamingLive = StreamingLiveFrom(checks)
		st.Problems = append(st.Problems, CheckProblems(checks)...)
	}

	st.Healthy = len(st.Problems) == 0
	return st, nil
}

// cachedFull returns a copy of the last show-backup result of server when it
// was taken for backup id, nil otherwise. A finished backup does not change,
// so Status runs show-backup again only when a new backup appears.
func (e *CLIEngine) cachedFull(server, id string) *Backup {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b := e.lastFull[server]; b != nil && b.ID == id {
		c := *b
		return &c
	}
	return nil
}

func (e *CLIEngine) storeFull(server string, b *Backup) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastFull == nil {
		e.lastFull = map[string]*Backup{}
	}
	c := *b
	e.lastFull[server] = &c
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

// RebuildXlogDB runs `barman rebuild-xlogdb <server>`: the WAL catalogue is
// written again from the WAL files present (used on a copy of a server's
// directory, whose catalogue may lag behind its files).
func (e *CLIEngine) RebuildXlogDB(ctx context.Context, server string) (string, error) {
	return e.runText(ctx, "rebuild-xlogdb", server)
}

func (e *CLIEngine) Cron(ctx context.Context) error {
	return e.runPlain(ctx, "cron")
}

// BackupIDPattern is the shape of a Barman backup id (a timestamp).
var BackupIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`)

// runText runs a command whose output is shown to the user. On failure the
// error carries Barman's own message (stderr, else stdout) and nothing else.
func (e *CLIEngine) runText(ctx context.Context, args ...string) (string, error) {
	out, errOut, err := e.Runner.Run(ctx, args...)
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return text, ctx.Err()
		}
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = text
		}
		if msg != "" {
			return text, &BarmanError{Msg: msg, Err: err}
		}
	}
	return text, err
}

// BarmanError is a failed Barman command whose message is Barman's own.
type BarmanError struct {
	Msg string
	Err error
}

func (e *BarmanError) Error() string { return e.Msg }
func (e *BarmanError) Unwrap() error { return e.Err }

// DeleteBackup runs `barman delete <server> <backup_id>`.
func (e *CLIEngine) DeleteBackup(ctx context.Context, server, backupID string) (string, error) {
	if !BackupIDPattern.MatchString(backupID) {
		return "", fmt.Errorf("%q is not a backup id", backupID)
	}
	return e.runText(ctx, "delete", server, backupID)
}

// Recover runs `barman recover [--target-time T | --target-immediate] <server> <backup_id> <dest>`.
// The recovery is local (no --remote-ssh-command): dest is a directory of the
// host where Barman runs.
func (e *CLIEngine) Recover(ctx context.Context, server, backupID, dest string, opts RecoverOptions) (string, error) {
	if !BackupIDPattern.MatchString(backupID) {
		return "", fmt.Errorf("%q is not a backup id", backupID)
	}
	args := []string{"recover"}
	if opts.TargetTime != "" {
		args = append(args, "--target-time", opts.TargetTime)
	}
	if opts.TargetImmediate {
		args = append(args, "--target-immediate")
	}
	args = append(args, server, backupID, dest)
	return e.runText(ctx, args...)
}

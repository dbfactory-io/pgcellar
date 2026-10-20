package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/store"
)

// A restore drill proves that a backup can be restored and that the result
// starts: pgCellar restores it into recoveries.dir/<server>/drill-<ts>, starts
// the copy with the PostgreSQL binaries of its major version on a private port,
// waits for the end of the recovery, looks at what is inside, stops it and
// writes a dated PDF report. Everything that runs goes through the audited
// runner.

// Drill steps, in order. The key is what the database and the API carry.
const (
	StepRecover   = "recover"
	StepVersion   = "version"
	StepConfigure = "configure"
	StepStart     = "start"
	StepRecovery  = "recovery"
	StepChecks    = "checks"
	StepStop      = "stop"
	StepChecksums = "checksums"
	StepCleanup   = "cleanup"
)

var drillStepLabels = map[string]string{
	StepRecover:   "Restore the backup (barman recover)",
	StepVersion:   "Find the PostgreSQL version",
	StepConfigure: "Prepare the throwaway copy",
	StepStart:     "Start PostgreSQL",
	StepRecovery:  "Wait for the end of the recovery",
	StepChecks:    "Look inside the databases",
	StepStop:      "Stop PostgreSQL",
	StepChecksums: "Verify the data checksums",
	StepCleanup:   "Clean up",
	"":            "",
}

// DrillStepLabel is the sentence for a step key.
func DrillStepLabel(key string) string {
	if l, ok := drillStepLabels[key]; ok {
		return l
	}
	return key
}

// Step results.
const (
	StepOK      = "ok"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Ports a drill may use for the throwaway copy.
const (
	drillPortMin = 54400
	drillPortMax = 54499
)

const drillRunKey = "drill:"

// DrillTools names the programs of a drill; empty means the version-specific
// binary under drills.bin_dir (pg_ctl, pg_isready, pg_checksums). Tests and
// --dev point them to the fakes, like SetupTools does for psql (the psql of
// SetupTools is used for the queries).
type DrillTools struct {
	PgCtl       string
	PgIsReady   string
	PgChecksums string
}

// DrillInput is what a drill needs. Everything is optional.
type DrillInput struct {
	BackupID   string `json:"backup_id"`             // default: the latest backup that is DONE
	Target     string `json:"target"`                // latest (default) | backup_end | time
	TargetTime string `json:"target_time,omitempty"` // with target "time"
}

// DrillStep is one line of the report.
type DrillStep struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // ok | failed | skipped
	Millis int64  `json:"millis"`
	Note   string `json:"note,omitempty"`
}

// DrillDatabase is one database of the restored copy.
type DrillDatabase struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Tables    int    `json:"tables"`
	Rows      *int64 `json:"rows,omitempty"` // total of count(*), with drills.verify_tables
}

// DrillDetails is the JSON kept in the drills table.
type DrillDetails struct {
	Target        string          `json:"target"`
	TargetTime    string          `json:"target_time,omitempty"`
	PGMajor       string          `json:"pg_major,omitempty"`
	PGVersion     string          `json:"pg_version,omitempty"`
	LSN           string          `json:"lsn,omitempty"`
	Checksums     string          `json:"checksums,omitempty"` // verified | not enabled | FAILED...
	SizeBytes     int64           `json:"size_bytes"`
	Port          int             `json:"port,omitempty"`
	DurationMs    int64           `json:"duration_ms"`
	Steps         []DrillStep     `json:"steps"`
	Databases     []DrillDatabase `json:"databases"`
	VerifyTables  bool            `json:"verify_tables"`
	Error         string          `json:"error,omitempty"`
	LogTail       string          `json:"log_tail,omitempty"`
	Summary       string          `json:"summary"`
	Fingerprint   string          `json:"fingerprint,omitempty"`
	DataDir       string          `json:"data_dir,omitempty"` // present while the restored copy is kept
	Actor         string          `json:"actor,omitempty"`
	PGUser        string          `json:"pg_user,omitempty"`
	RestoreCmd    string          `json:"restore_command,omitempty"`
	DatabasesN    int             `json:"databases_count"`
	TablesN       int             `json:"tables_count"`
	ReportWritten bool            `json:"report_written"`
}

// DrillView is a drill as the pages and the API show it.
type DrillView struct {
	ID         int64        `json:"id"`
	Server     string       `json:"server"`
	BackupID   string       `json:"backup_id"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	OK         bool         `json:"ok"`
	StepFailed string       `json:"step_failed,omitempty"`
	RunID      int64        `json:"run_id"`
	HasReport  bool         `json:"has_report"`
	Details    DrillDetails `json:"details"`
}

// Duration is the time the drill took.
func (v DrillView) Duration() time.Duration {
	return time.Duration(v.Details.DurationMs) * time.Millisecond
}

func drillView(d store.Drill) DrillView {
	v := DrillView{ID: d.ID, Server: d.Server, BackupID: d.BackupID, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		OK: d.OK, StepFailed: d.StepFailed, RunID: d.RunID, HasReport: d.ReportPath != ""}
	_ = json.Unmarshal([]byte(d.Details), &v.Details)
	if v.Details.Steps == nil {
		v.Details.Steps = []DrillStep{}
	}
	if v.Details.Databases == nil {
		v.Details.Databases = []DrillDatabase{}
	}
	return v
}

// ---- start ----

// StartDrill validates the request and starts the drill in the background. It
// returns the id of the run to follow. Only one drill runs at a time, and none
// while a backup of the same server is in progress.
func (a *App) StartDrill(ctx context.Context, actor, server string, in DrillInput) (int64, error) {
	if err := a.knownServer(ctx, server); err != nil {
		return 0, err
	}
	in.BackupID = strings.TrimSpace(in.BackupID)
	in.Target = strings.ToLower(strings.TrimSpace(in.Target))
	if in.Target == "" {
		in.Target = TargetLatest
	}
	var opts barman.RecoverOptions
	targetTime := ""
	var problems []string
	switch in.Target {
	case TargetLatest:
	case TargetBackupEnd:
		opts.TargetImmediate = true
	case TargetTime:
		t, err := parseTargetTime(in.TargetTime)
		if err != nil {
			problems = append(problems, "Point in time: "+err.Error()+".")
		} else {
			targetTime = barmanTime(t)
			opts.TargetTime = targetTime
		}
	default:
		problems = append(problems, fmt.Sprintf("Target %q is unknown: use latest, backup_end or time.", in.Target))
	}
	if len(problems) == 0 {
		st, ok := a.State(server)
		if !ok {
			return 0, ErrRefreshPending
		}
		if in.BackupID == "" {
			for _, b := range st.Backups { // newest first
				if b.Status == "DONE" {
					in.BackupID = b.ID
					break
				}
			}
			if in.BackupID == "" {
				problems = append(problems, fmt.Sprintf("%s has no complete backup (DONE) to restore yet.", server))
			}
		} else {
			found := false
			for _, b := range st.Backups {
				if b.ID == in.BackupID {
					found = true
					if b.Status != "DONE" {
						problems = append(problems, fmt.Sprintf("Backup %s is %s: only a complete backup (DONE) can be restored.", in.BackupID, b.Status))
					}
				}
			}
			if !found {
				problems = append(problems, fmt.Sprintf("Backup %s is not in the list of %s.", in.BackupID, server))
			}
		}
	}
	if len(problems) > 0 {
		return 0, &ProblemsError{Problems: problems}
	}

	key := drillRunKey + server
	a.runMu.Lock()
	for k, id := range a.running {
		if strings.HasPrefix(k, drillRunKey) {
			a.runMu.Unlock()
			return id, &ConflictError{Msg: fmt.Sprintf("A restore drill is already running (run #%d): drills run one at a time.", id), Retry: true}
		}
	}
	if id, busy := a.running[server]; busy {
		a.runMu.Unlock()
		return id, &ConflictError{Msg: fmt.Sprintf("A backup of %s is running (run #%d): a drill never runs at the same time.", server, id), Retry: true}
	}
	id, err := a.Store.StartRun(server, store.KindDrill, actor)
	if err != nil {
		a.runMu.Unlock()
		return 0, err
	}
	a.running[key] = id
	a.runMu.Unlock()
	_ = a.Store.AddAudit(actor, "drill.start", server+"/"+in.BackupID, fmt.Sprintf("run #%d target=%s %s", id, in.Target, targetTime))

	cfg := a.Config()
	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		defer func() {
			a.runMu.Lock()
			delete(a.running, key)
			a.runMu.Unlock()
		}()
		a.runDrill(id, actor, server, in, opts, cfg)
	}()
	return id, nil
}

// ---- the drill itself ----

// drillRun is the state of one drill while it runs.
type drillRun struct {
	a      *App
	cfg    *config.Config
	ctx    context.Context
	actor  string
	server string
	in     DrillInput
	opts   barman.RecoverOptions

	journal strings.Builder
	t0      time.Time
	startAt time.Time // the clock of the application at the start (a.now)
	steps   []DrillStep
	det     DrillDetails

	root, dest, sock string
	port             int
	bin              string // directory of the PostgreSQL binaries
	pgUser           string
	started          bool // postgres was started and may still run
	extraDirs        []string
	failedKey        string
	inFinish         bool
}

func (r *drillRun) logf(format string, args ...any) {
	fmt.Fprintf(&r.journal, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// clip shortens command output for the journal.
func clip(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + " [...]"
	}
	return strings.ReplaceAll(s, "\n", "\n          ")
}

// exec runs a program through the audited runner and writes the command, its
// exit code and its output to the journal.
func (r *drillRun) exec(ctx context.Context, binary string, args ...string) ([]byte, error) {
	r.logf("$ %s %s", binary, strings.Join(args, " "))
	out, errOut, err := r.a.Tools.RunBinary(barman.WithTarget(ctx, r.server), binary, args...)
	if s := clip(out, 600); s != "" {
		r.logf("  %s", s)
	}
	if s := clip(errOut, 600); s != "" {
		r.logf("  stderr: %s", s)
	}
	if err != nil {
		r.logf("  -> failed: exit %d", exitCodeOf(err))
	}
	return out, err
}

// step runs fn as a named step: time it, journal it, record the verdict. It
// returns false when fn failed.
func (r *drillRun) step(key string, fn func() (string, error)) bool {
	st := time.Now()
	r.logf("== %s", DrillStepLabel(key))
	note, err := fn()
	s := DrillStep{Key: key, Label: DrillStepLabel(key), Status: StepOK, Millis: time.Since(st).Milliseconds(), Note: note}
	if err != nil {
		s.Status = StepFailed
		s.Note = errText(err)
		r.det.Error = errText(err)
		if !r.inFinish && r.ctx.Err() != nil && errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
			r.det.Error = fmt.Sprintf("the drill ran out of time (drills.timeout %s) during this step", r.cfg.Drills.Timeout)
			s.Note = r.det.Error
		}
		r.logf("   FAILED after %s: %s", time.Since(st).Round(time.Millisecond), s.Note)
	} else {
		r.logf("   ok in %s %s", time.Since(st).Round(time.Millisecond), note)
	}
	r.steps = append(r.steps, s)
	return err == nil
}

func (r *drillRun) skip(key, why string) {
	r.steps = append(r.steps, DrillStep{Key: key, Label: DrillStepLabel(key), Status: StepSkipped, Note: why})
	r.logf("== %s: skipped (%s)", DrillStepLabel(key), why)
}

func errText(err error) string {
	var re *barman.RunError
	if errors.As(err, &re) {
		if m := strings.TrimSpace(re.Stderr); m != "" {
			if i := strings.IndexByte(m, '\n'); i > 0 && len(m) > 300 {
				m = m[:i]
			}
			return fmt.Sprintf("%s (exit %d)", m, re.ExitCode)
		}
	}
	return err.Error()
}

func (a *App) runDrill(id int64, actor, server string, in DrillInput, opts barman.RecoverOptions, cfg *config.Config) {
	r := &drillRun{a: a, cfg: cfg, actor: actor, server: server, in: in, opts: opts, t0: time.Now(), startAt: a.now()}
	ctx, cancel := context.WithTimeout(barman.WithActor(context.Background(), actor), cfg.Drills.Timeout)
	defer cancel()
	r.ctx = ctx
	r.det.Target, r.det.TargetTime, r.det.Actor = in.Target, opts.TargetTime, actor
	r.det.VerifyTables = cfg.Drills.VerifyTables
	r.logf("Restore drill of %s, backup %s, target %s%s (run #%d, started by %s)", server, in.BackupID, in.Target,
		map[bool]string{true: " " + opts.TargetTime}[opts.TargetTime != ""], id, actor)

	failed := r.execute()

	// Whatever happened, stop the copy and clean up, with a fresh clock when
	// the drill ran out of time.
	cctx, ccancel := context.WithTimeout(barman.WithActor(context.Background(), actor), 3*time.Minute)
	defer ccancel()
	r.finish(cctx, failed)

	d := r.record(id)
	code := 0
	if !d.OK {
		code = 1
	}
	stderr := ""
	if !d.OK {
		stderr = "Drill failed at step " + DrillStepLabel(d.StepFailed) + ": " + r.det.Error
	}
	if err := a.Store.FinishRun(id, code, tail([]byte(r.journal.String())), tail([]byte(stderr))); err != nil {
		a.Logf("cannot record the end of run #%d: %v", id, err)
	}
	_ = a.Store.AddAudit(actor, "drill.end", server+"/"+in.BackupID, fmt.Sprintf("run #%d ok=%t %s", id, d.OK, r.det.Summary))
	a.Logf("drill of %s: %s", server, r.det.Summary)
}

// execute runs the steps up to the first failure and returns its key.
func (r *drillRun) execute() (failed string) {
	a, cfg := r.a, r.cfg
	fail := func(key string) string { return key }

	// a. restore
	if !r.step(StepRecover, func() (string, error) {
		r.root = cfg.Recoveries.Dir
		dest, err := drillDest(r.root, r.server, time.Now())
		if err != nil {
			return "", err
		}
		r.dest = dest
		if err := prepareRecoveryDir(dest, cfg); err != nil {
			return "", err
		}
		r.det.DataDir = dest
		eng := barman.NewEngine(&journalRunner{r: r, inner: a.Runner})
		if _, err := eng.Recover(r.ctx, r.server, r.in.BackupID, dest, r.opts); err != nil {
			return "", err
		}
		r.det.SizeBytes = dirSize(dest)
		return fmt.Sprintf("restored %s into %s", humanBytes(r.det.SizeBytes), filepath.Base(dest)), nil
	}) {
		return fail(StepRecover)
	}

	// b. which binaries
	if !r.step(StepVersion, func() (string, error) {
		raw, err := os.ReadFile(filepath.Join(r.dest, "PG_VERSION"))
		if err != nil {
			return "", fmt.Errorf("the restored directory has no PG_VERSION: %w", err)
		}
		major := strings.TrimSpace(string(raw))
		if !regexp.MustCompile(`^[0-9]{1,2}(\.[0-9])?$`).MatchString(major) {
			return "", fmt.Errorf("PG_VERSION holds %q, which is not a PostgreSQL version", major)
		}
		r.det.PGMajor = major
		r.bin = filepath.Join(cfg.Drills.BinDir, major, "bin")
		if a.DrillTools.PgCtl == "" {
			if fi, err := os.Stat(filepath.Join(r.bin, "pg_ctl")); err != nil || fi.IsDir() {
				return "", fmt.Errorf("PostgreSQL %s binaries are not in this image (no %s): rebuild the image with PG_MAJORS including %s", major, filepath.Join(r.bin, "pg_ctl"), major)
			}
		}
		return "PostgreSQL " + major, nil
	}) {
		return fail(StepVersion)
	}

	// c. settings of the copy
	if !r.step(StepConfigure, r.configure) {
		return fail(StepConfigure)
	}

	// d. start
	if !r.step(StepStart, func() (string, error) {
		r.started = true // even a failed start may leave a postmaster behind
		_, err := r.exec(r.ctx, r.tool("pg_ctl"), "-D", r.dest, "-o", fmt.Sprintf("-p %d", r.port), "-w", "-t", "300", "-l", filepath.Join(r.dest, "drill.log"), "start")
		if err != nil {
			r.det.LogTail = r.logTail()
			if l := r.logLines(40); l != "" {
				r.logf("  last lines of drill.log:\n          %s", strings.ReplaceAll(l, "\n", "\n          "))
			}
			if msg := localeProblem(r.logLines(200)); msg != "" {
				err = errors.New(msg)
			}
		}
		return fmt.Sprintf("on port %d", r.port), err
	}) {
		return fail(StepStart)
	}

	// d bis. wait for the end of the recovery
	if !r.step(StepRecovery, r.waitRecovery) {
		r.det.LogTail = r.logTail()
		return fail(StepRecovery)
	}

	// e. look inside
	if !r.step(StepChecks, r.check) {
		return fail(StepChecks)
	}
	return ""
}

// finish stops the copy, verifies the checksums of the stopped copy, removes
// what must go, and turns the steps into the verdict.
func (r *drillRun) finish(ctx context.Context, failed string) {
	a, cfg := r.a, r.cfg
	r.inFinish = true
	stopOK := true
	running := r.started
	if running && failed == StepStart { // a failed start usually leaves nothing running
		_, serr := r.execQuiet(ctx, r.tool("pg_ctl"), "-D", r.dest, "status")
		running = serr == nil
	}
	if running {
		stopOK = r.step(StepStop, func() (string, error) {
			_, err := r.exec(ctx, r.tool("pg_ctl"), "-D", r.dest, "-w", "-t", "120", "-m", "fast", "stop")
			if err != nil {
				_, err = r.exec(ctx, r.tool("pg_ctl"), "-D", r.dest, "-w", "-t", "60", "-m", "immediate", "stop")
				if err == nil {
					return "stopped (immediate mode: the fast stop did not work)", nil
				}
			}
			return "", err
		})
		if !stopOK && failed == "" {
			failed = StepStop
		}
	}
	// Data checksums are verified on the stopped copy: pg_checksums refuses a
	// running cluster, and the state recorded in a base backup is not "shut
	// down" until PostgreSQL has stopped cleanly once.
	switch {
	case failed != "":
	case r.det.Checksums == "enabled" && stopOK:
		if !r.step(StepChecksums, func() (string, error) {
			out, err := r.exec(ctx, r.tool("pg_checksums"), "--check", "-D", r.dest)
			if err != nil {
				r.det.Checksums = "FAILED"
				return "", err
			}
			r.det.Checksums = "verified"
			return "no bad checksum (" + firstLineWith(out, "Blocks scanned") + ")", nil
		}) {
			failed = StepChecksums
		}
	default:
		r.skip(StepChecksums, "data checksums are not enabled on this server")
		if r.det.Checksums == "" {
			r.det.Checksums = "not enabled"
		}
	}
	if r.det.Checksums == "enabled" {
		r.det.Checksums = "not verified"
	}

	r.step(StepCleanup, func() (string, error) {
		keep := failed == "" && cfg.Drills.KeepsLast() && r.dest != ""
		for _, d := range r.extraDirs {
			_ = os.RemoveAll(d)
		}
		if r.dest == "" {
			return "nothing to remove", nil
		}
		if keep {
			a.dropOlderDrillCopies(r.server, r.dest)
			return "kept the restored copy (drills.keep_last), the previous copy was removed", nil
		}
		r.det.DataDir = ""
		if err := removeTree(r.root, r.dest); err != nil {
			return "", err
		}
		return "removed the restored copy", nil
	})
	if failed == "" {
		for _, s := range r.steps {
			if s.Status == StepFailed {
				failed = s.Key
				break
			}
		}
	}
	r.det.Steps = r.steps
	r.det.DurationMs = time.Since(r.t0).Milliseconds()
	r.failedKey = failed
}

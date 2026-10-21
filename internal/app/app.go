// Package app wires pgCellar together (configuration, store, Barman engine)
// and holds the service layer used by both presentations: the HTML interface
// (internal/web) and the JSON API (internal/api). Neither of them contains
// business logic of its own.
package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

// Roles, from the least to the most privileged.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

var roleRank = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// RoleAtLeast reports whether role grants everything min grants.
func RoleAtLeast(role, min string) bool {
	return roleRank[role] > 0 && roleRank[role] >= roleRank[min]
}

// App is the running pgCellar: configuration, database and Barman engine.
type App struct {
	Version string
	Dev     bool
	// Demo is the public demonstration instance (pgcellar serve --demo): the
	// history is fictional and rebuilt from the clock, nothing can be changed,
	// no alert is ever sent. See internal/demo.
	Demo bool
	// FSStat replaces the file system measurement (statfs) of the storage
	// figures. The demo shows a 4 TiB volume whatever the container has.
	FSStat func(path string) (total, free, used int64, dev uint64, err error)
	// DisplayHome, when set, is shown in place of barman_home in the storage
	// figures (the demo keeps its files in a temporary directory).
	DisplayHome string
	// CronInterval is how often `barman cron` runs (barman.cron_interval in
	// pgcellar.yaml, unless the caller overrides it). Zero disables it.
	CronInterval time.Duration
	// StatusInterval is how often the status cache is refreshed.
	StatusInterval time.Duration
	// SchedulerInterval is how often the scheduler looks at the clock.
	SchedulerInterval time.Duration
	// SetupInterval is how often the PostgreSQL settings are read again.
	SetupInterval time.Duration
	// SSHHome is the home directory whose ~/.ssh holds the SSH key pair; empty
	// = the home of the Barman user. Tests point it to a temporary directory.
	SSHHome string
	// Tools runs psql and pg_basebackup for the Server setup section, through
	// the same audited path as Barman. SetupTools names the programs.
	Tools      barman.BinaryRunner
	SetupTools barman.SetupTools
	// DrillTools names pg_ctl, pg_isready and pg_checksums for the restore
	// drills (empty = the binaries of the PostgreSQL major version of the
	// restored copy). DrillPollInterval is the pause between two looks at a
	// copy that is recovering (0 = 2 s).
	DrillTools        DrillTools
	DrillPollInterval time.Duration
	// AlertOptions lets tests aim the alert channels at local servers.
	AlertOptions alerts.Options
	// AlertsInterval is how often the alert loop looks at the state (0 = 30 s).
	// AlertsSince is the start of the quiet period after startup; zero = none.
	AlertsInterval time.Duration
	AlertsSince    time.Time
	// LicensePublicKey replaces the embedded verification key (tests).
	LicensePublicKey ed25519.PublicKey

	ConfigPath string // absolute path of pgcellar.yaml
	ConfigDir  string // its directory, a git repository
	Store      *store.Store
	Engine     barman.Engine
	// BarmanHome is barman_home of barman.conf (empty = Barman's default).
	BarmanHome string
	Runner     barman.Runner
	Logger     *log.Logger
	// Now can be replaced in tests.
	Now func() time.Time

	// Generated in --dev mode; removed by Close.
	TempDir string

	mu         sync.RWMutex
	cfg        *config.Config
	cfgModTime time.Time
	cfgChecked time.Time

	sched   schedState
	alerts  alertEngine
	lic     licenseCache
	runMu   sync.Mutex
	running map[string]int64 // server -> run id of the backup in progress

	auditMu   sync.Mutex
	auditSeen map[string]time.Time // command -> when its last failure was logged

	cacheMu     sync.RWMutex
	cache       map[string]*ServerState
	cacheOrder  []string
	cacheTime   time.Time
	cacheErr    string
	cacheStale  bool // loaded from the database, no refresh has completed since
	refreshOnce sync.Mutex

	firstCron chan struct{} // closed once the first `barman cron` has run (or is not wanted)

	wg   sync.WaitGroup // background loops
	jobs sync.WaitGroup // backups in progress
}

// Load reads and validates the configuration, opens the database and builds
// the Barman engine. In dev mode it works on a temporary copy of everything
// and uses the fake Barman, see dev.go.
func Load(configPath string, dev bool) (*App, error) { return load(configPath, dev, false, "") }

// LoadDemo is Load for the public demo: the configuration may hold no
// administrator, only one read-only user.
//
// onlyUnder is the directory of the fake programs: nothing else is ever run.
func LoadDemo(configPath, onlyUnder string) (*App, error) {
	return load(configPath, false, true, onlyUnder)
}

func load(configPath string, dev, demo bool, onlyUnder string) (*App, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}
	a := &App{
		Dev:            dev,
		Demo:           demo,
		StatusInterval: time.Minute,
		SetupInterval:  config.DefaultSetupInterval,
		Logger:         log.New(os.Stderr, "pgcellar: ", log.LstdFlags),
		Now:            time.Now,
		running:        map[string]int64{},
		cache:          map[string]*ServerState{},
	}
	if dev {
		abs, err = a.setupDev(abs, cfg)
		if err != nil {
			return nil, fmt.Errorf("dev mode: %w", err)
		}
	}
	if problems := config.ValidateWith(cfg, demo); len(problems) > 0 {
		a.cleanup()
		return nil, fmt.Errorf("invalid configuration %s:\n  - %s", configPath, strings.Join(problems, "\n  - "))
	}
	a.ConfigPath = abs
	a.ConfigDir = filepath.Dir(abs)
	a.cfg = cfg
	for _, w := range config.Warnings(cfg) {
		a.Logf("warning: %s", w)
	}
	a.CronInterval = cfg.Barman.CronInterval
	if cfg.StatusInterval > 0 {
		a.StatusInterval = cfg.StatusInterval
	}
	if cfg.SetupInterval > 0 {
		a.SetupInterval = cfg.SetupInterval
	}
	a.noteModTime()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		a.cleanup()
		return nil, fmt.Errorf("cannot create data_dir %s: %w", cfg.DataDir, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "pgcellar.db"))
	if err != nil {
		a.cleanup()
		return nil, fmt.Errorf("cannot open the database: %w", err)
	}
	if err := st.Migrate(); err != nil {
		_ = st.Close()
		a.cleanup()
		return nil, fmt.Errorf("cannot prepare the database: %w", err)
	}
	a.Store = st

	runner := barman.CLIRunner{
		Binary: cfg.Barman.Binary,
		User:   cfg.Barman.User,
		Audit:  a.audit,

		OnlyUnder: onlyUnder,
	}
	a.Runner = runner
	a.Tools = runner
	eng := barman.NewEngine(runner)
	if g, gerr := barman.LoadGlobal(cfg.Barman.ConfigFile); gerr == nil {
		eng.BarmanHome = g.BarmanHome
		a.BarmanHome = g.BarmanHome
	}
	a.Engine = eng

	if err := config.EnsureRepo(a.ConfigDir); err != nil {
		a.Logger.Printf("warning: no git history for %s: %v", a.ConfigDir, err)
	} else if _, err := config.ImportExisting(a.ConfigDir); err != nil {
		a.Logger.Printf("warning: cannot record the existing configuration of %s: %v", a.ConfigDir, err)
	}
	if d := a.barmanDirIfSeparate(); d != "" {
		if _, err := os.Stat(d); err == nil {
			if err := config.EnsureRepo(d); err != nil {
				a.Logger.Printf("warning: no git history for %s: %v", d, err)
			} else if _, err := config.ImportExisting(d); err != nil {
				a.Logger.Printf("warning: cannot record the existing configuration of %s: %v", d, err)
			}
		}
	}
	a.closeStaleRuns()
	a.loadStatusCache()
	return a, nil
}

// Background commands (the barman cron ticker and the status refresh) run as
// this actor. They are audited only when they fail, and an identical failure
// is written at most once per auditRepeat, so a broken Barman does not bury
// the audit log under one line per minute.
const (
	systemActor = "system"
	auditRepeat = time.Hour
)

// audit records one executed Barman command. Everything started by a user or
// an API key is recorded; background commands only on failure.
func (a *App) audit(e barman.AuditEntry) {
	actor := e.Actor
	if actor == "" {
		actor = systemActor
	}
	if actor == systemActor && !a.backgroundFailureDue(e) {
		return
	}
	target := e.Target
	if n := len(e.Args); target == "" && n > 0 && filepath.Base(e.Binary) != "psql" && !strings.HasPrefix(e.Args[n-1], "-") && e.Args[n-1] != "cron" {
		target = e.Args[n-1]
	}
	details := fmt.Sprintf("exit=%d duration=%s", e.ExitCode, e.Duration.Round(time.Millisecond))
	if err := a.Store.AddAudit(actor, e.Command, target, details); err != nil {
		a.Logger.Printf("audit write failed: %v", err)
	}
}

// backgroundFailureDue reports whether a background command deserves an audit
// line: it failed, and the same command did not fail and get logged within
// the last auditRepeat.
func (a *App) backgroundFailureDue(e barman.AuditEntry) bool {
	if e.ExitCode == 0 {
		return false
	}
	now := time.Now()
	a.auditMu.Lock()
	defer a.auditMu.Unlock()
	if a.auditSeen == nil {
		a.auditSeen = map[string]time.Time{}
	}
	if last, ok := a.auditSeen[e.Command]; ok && now.Sub(last) < auditRepeat {
		return false
	}
	a.auditSeen[e.Command] = now
	return true
}

// closeStaleRuns marks runs left open by a previous process as interrupted.
func (a *App) closeStaleRuns() {
	runs, err := a.Store.ListRuns("", 500)
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.FinishedAt == nil {
			_ = a.Store.FinishRun(r.ID, -1, r.StdoutTail, "interrupted: pgCellar was restarted while this run was in progress")
		}
	}
}

func (a *App) now() time.Time { return a.Now() }

func (a *App) barmanHome() string {
	if a.BarmanHome == "" {
		return barman.DefaultBarmanHome
	}
	return a.BarmanHome
}

// Logf writes to the application log.
func (a *App) Logf(format string, args ...any) { a.Logger.Printf(format, args...) }

// Start launches the background loops (status cache, barman cron, session
// cleanup). They stop when ctx ends.
func (a *App) Start(ctx context.Context) {
	// `barman cron` starts the WAL receivers; the first status refresh waits
	// for it, otherwise every server shows "receive-wal running" as failed.
	a.firstCron = make(chan struct{})
	if a.CronInterval > 0 {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.cronLoop(ctx)
		}()
	} else {
		close(a.firstCron)
	}
	if a.Config().Scheduler.IsEnabled() {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.schedulerLoop(ctx)
		}()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.statusLoop(ctx)
	}()
	if !a.Dev && !a.Demo {
		a.AlertsSince = a.now()
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.alertsLoop(ctx)
		}()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		lastAuditPurge := time.Time{}
		for {
			_, _ = a.Store.PurgeExpiredSessions()
			if time.Since(lastAuditPurge) >= 24*time.Hour {
				a.PurgeAudit()
				a.PurgeRecoveries()
				lastAuditPurge = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// PurgeAudit deletes the audit lines older than audit_retention_days (0 keeps
// everything) and returns how many were removed. The background loop calls it
// once a day, and once at startup.
func (a *App) PurgeAudit() int64 {
	days := a.Config().EffectiveAuditRetentionDays()
	if days <= 0 {
		return 0
	}
	n, err := a.Store.PruneAudit(time.Now().AddDate(0, 0, -days))
	if err != nil {
		a.Logf("audit purge failed: %v", err)
		return 0
	}
	if n > 0 {
		a.Logf("audit purge: removed %d line(s) older than %d days", n, days)
	}
	return n
}

func (a *App) cronLoop(ctx context.Context) {
	a.runCron(ctx)
	close(a.firstCron)
	t := time.NewTicker(a.CronInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runCron(ctx)
		}
	}
}

func (a *App) runCron(ctx context.Context) {
	cctx, cancel := context.WithTimeout(barman.WithActor(ctx, systemActor), 5*time.Minute)
	defer cancel()
	if err := a.Engine.Cron(cctx); err != nil && ctx.Err() == nil {
		a.Logf("barman cron failed: %v", err)
	}
}

// Close waits for the background loops, then closes the database. Call it
// after cancelling the context given to Start.
func (a *App) Close() error {
	a.wg.Wait()
	// Give running backups a moment to record their end; a backup that
	// takes longer is left to Barman and shows up as interrupted on restart.
	done := make(chan struct{})
	go func() { a.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
	var err error
	if a.Store != nil {
		err = a.Store.Close()
	}
	a.cleanup()
	return err
}

func (a *App) cleanup() {
	if a.TempDir != "" {
		_ = os.RemoveAll(a.TempDir)
	}
}

// ---- configuration access ----

func (a *App) noteModTime() {
	if fi, err := os.Stat(a.ConfigPath); err == nil {
		a.cfgModTime = fi.ModTime()
	}
	a.cfgChecked = time.Now()
}

// Config returns the current configuration. The file is the truth: when it
// changed on disk (hand edit, git pull) and still validates, it is reloaded.
// The returned value must be treated as read-only.
func (a *App) Config() *config.Config {
	a.mu.RLock()
	cfg, checked := a.cfg, a.cfgChecked
	a.mu.RUnlock()
	if time.Since(checked) < time.Second {
		return cfg
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.cfgChecked) < time.Second {
		return a.cfg
	}
	a.cfgChecked = time.Now()
	fi, err := os.Stat(a.ConfigPath)
	if err != nil || !fi.ModTime().After(a.cfgModTime) {
		return a.cfg
	}
	fresh, err := config.Load(a.ConfigPath)
	if err != nil || len(config.ValidateWith(fresh, a.Demo)) > 0 {
		a.Logf("pgcellar.yaml changed on disk but is not valid, keeping the previous configuration")
		a.cfgModTime = fi.ModTime()
		return a.cfg
	}
	a.cfg, a.cfgModTime = fresh, fi.ModTime()
	return a.cfg
}

// barmanConfigDir is where the server files live.
func (a *App) barmanConfigDir() string { return a.Config().Barman.ConfigDir }

// barmanDirIfSeparate returns the Barman config directory when it is not
// inside the pgcellar.yaml repository (then it gets its own repository).
func (a *App) barmanDirIfSeparate() string {
	d := a.cfg.Barman.ConfigDir
	if d == "" {
		return ""
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return ""
	}
	if abs == a.ConfigDir || strings.HasPrefix(abs, a.ConfigDir+string(filepath.Separator)) {
		return ""
	}
	return abs
}

// mutateConfig reloads pgcellar.yaml from disk, applies fn, validates and
// saves. The caller holds no lock.
func (a *App) mutateConfig(fn func(cfg *config.Config) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	if problems := config.ValidateWith(cfg, a.Demo); len(problems) > 0 {
		return &ProblemsError{Problems: problems}
	}
	if err := config.Save(a.ConfigPath, cfg); err != nil {
		return err
	}
	a.cfg = cfg
	a.noteModTime()
	return nil
}

// Commit records pending changes in the git history of the configuration
// directory (and of the Barman directory when it is elsewhere). It returns
// the short hash of the first repository that changed.
func (a *App) Commit(message, author string) (string, error) {
	var first string
	var errs []string
	dirs := []string{a.ConfigDir}
	if d := a.barmanDirIfSeparate(); d != "" {
		dirs = append(dirs, d)
	}
	for _, d := range dirs {
		h, err := config.Commit(d, message, author, author+"@pgcellar")
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if first == "" {
			first = h
		}
	}
	if len(errs) > 0 {
		return first, errors.New(strings.Join(errs, "; "))
	}
	return first, nil
}

// Errors returned by the service layer. Presentations map them to messages
// and status codes.
var (
	ErrNotFound = errors.New("not found")
	ErrNoFile   = errors.New("this server has no configuration file in the Barman directory")
)

// ProblemsError carries validation problems, one sentence each.
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string { return strings.Join(e.Problems, " ") }

// ConflictError means the request is valid but cannot be done now.
type ConflictError struct {
	Msg string
	// Retry: the request is fine and only has to wait (a drill while a backup
	// runs); the scheduler keeps it pending instead of dropping it.
	Retry bool
}

func (e *ConflictError) Error() string { return e.Msg }

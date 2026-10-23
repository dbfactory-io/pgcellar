package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// The Vault is the off-site copy of the backups: after every successful
// backup, and every vault.wal_interval for the WAL files, pgCellar runs
// `restic backup` of <barman_home>/<server> and of the server's barman.d file
// into one restic repository per server on S3. restic encrypts on this host
// with a password pgCellar generates and never sends anywhere: whoever holds
// the bucket cannot read it. Copies run one at a time, in the background,
// through the audited runner; a copy never blocks a backup.

// Triggers of a copy.
const (
	VaultTriggerBackup = "backup" // after a successful backup
	VaultTriggerWAL    = "wal"    // the periodic pass for the WAL files
	VaultTriggerManual = "manual" // "Copy now"
)

const (
	settingVaultInfo  = "vault.info"
	settingVaultPrune = "vault.prune."
	// DefaultVaultInterval is how often the vault loop looks for work.
	DefaultVaultInterval = 30 * time.Second
	vaultCopyTimeout     = 12 * time.Hour
	vaultPruneEvery      = 24 * time.Hour
	vaultUsageEvery      = 6 * time.Hour
	vaultLockEvery       = 24 * time.Hour
	vaultHistoryDays     = 30
	// vaultFailStreak failed copies in a row turn the server orange.
	vaultFailStreak = 3
	journalMax      = 12 * 1024
)

// VaultTools names the restic program (tests and --dev use the fake).
type VaultTools struct {
	Restic string
}

func (a *App) resticBin() string {
	if a.VaultTools.Restic != "" {
		return a.VaultTools.Restic
	}
	return "restic"
}

// vaultJob is a copy waiting for its turn.
type vaultJob struct {
	trigger string
	actor   string
}

// vaultHolder is the in-memory side of the vault: the queue and what runs.
type vaultHolder struct {
	mu      sync.Mutex
	pending map[string]vaultJob
	order   []string
	busy    map[string]string // server -> "copy" or "drill": restic works on its repository
	current int64             // id of the copy in progress, 0 when none
	wake    chan struct{}

	infoMu sync.Mutex // read-modify-write of the vault.info setting
}

func (h *vaultHolder) init() {
	if h.pending == nil {
		h.pending = map[string]vaultJob{}
		h.busy = map[string]string{}
		h.wake = make(chan struct{}, 1)
	}
}

// vaultInfo is what pgCellar learnt about the bucket, kept in the settings
// table (rebuildable: a test connection or the next copy fills it again).
type vaultInfo struct {
	// Target is endpoint/bucket/prefix the facts below were read on; they are
	// forgotten when it changes.
	Target          string                `json:"target"`
	Lock            vault.Lock            `json:"lock"`
	LockCheckedAt   time.Time             `json:"lock_checked_at,omitzero"`
	LockError       string                `json:"lock_error,omitempty"`
	Test            *VaultTestResult      `json:"test,omitempty"`
	Usage           map[string]VaultUsage `json:"usage,omitempty"`
	KeyCreatedAt    time.Time             `json:"key_created_at,omitzero"`
	KeyDownloadedAt time.Time             `json:"key_downloaded_at,omitzero"`
	KeyDownloadedBy string                `json:"key_downloaded_by,omitempty"`
}

// VaultUsage is what the copies of one server take in the bucket.
type VaultUsage struct {
	vault.Usage
	MeasuredAt time.Time `json:"measured_at"`
	Method     string    `json:"method"` // s3 (object listing) or restic (restic stats)
	Error      string    `json:"error,omitempty"`
}

func vaultTarget(v config.Vault) string {
	return strings.TrimRight(v.Endpoint, "/") + "/" + v.Bucket + "/" + v.EffectivePrefix()
}

func (a *App) loadVaultInfo() vaultInfo {
	var vi vaultInfo
	if v, ok, err := a.Store.GetSetting(settingVaultInfo); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &vi)
	}
	if vi.Usage == nil {
		vi.Usage = map[string]VaultUsage{}
	}
	return vi
}

// updateVaultInfo changes the stored facts under a lock. Facts measured on
// another bucket are dropped first, the key dates are kept.
func (a *App) updateVaultInfo(fn func(*vaultInfo)) {
	a.vault.infoMu.Lock()
	defer a.vault.infoMu.Unlock()
	vi := a.loadVaultInfo()
	if t := vaultTarget(a.Config().Vault); vi.Target != t {
		vi = vaultInfo{Target: t, Usage: map[string]VaultUsage{}, KeyCreatedAt: vi.KeyCreatedAt, KeyDownloadedAt: vi.KeyDownloadedAt, KeyDownloadedBy: vi.KeyDownloadedBy}
	}
	fn(&vi)
	if b, err := json.Marshal(vi); err == nil {
		if err := a.Store.SetSetting(settingVaultInfo, string(b)); err != nil {
			a.Logf("vault: cannot record what was measured: %v", err)
		}
	}
}

// currentVaultInfo is the stored facts, empty when they were measured on
// another bucket.
func (a *App) currentVaultInfo() vaultInfo {
	vi := a.loadVaultInfo()
	if vi.Target != vaultTarget(a.Config().Vault) {
		return vaultInfo{Usage: map[string]VaultUsage{}, KeyCreatedAt: vi.KeyCreatedAt, KeyDownloadedAt: vi.KeyDownloadedAt, KeyDownloadedBy: vi.KeyDownloadedBy}
	}
	return vi
}

// ---- secrets ----

// ErrVaultOff is returned when the vault is not enabled.
var ErrVaultOff = errors.New("the off-site copy is not enabled (vault.enabled in pgcellar.yaml, or the Vault page)")

// vaultSecret reads the S3 secret key from its file.
func vaultSecret(cfg *config.Config) (string, error) {
	p := cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("the S3 secret key file %s does not exist: paste the secret key on the Vault page, or write it into that file (mode 0600)", p)
	}
	if err != nil {
		return "", fmt.Errorf("cannot read the S3 secret key: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("the S3 secret key file %s is empty", p)
	}
	return s, nil
}

// vaultEnv is the environment restic needs: the S3 credentials and a quiet
// progress output. It never goes to a log.
func vaultEnv(cfg *config.Config) ([]string, error) {
	secret, err := vaultSecret(cfg)
	if err != nil {
		return nil, err
	}
	return []string{
		"AWS_ACCESS_KEY_ID=" + cfg.Vault.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + secret,
		"AWS_SESSION_TOKEN=",
		"AWS_DEFAULT_REGION=" + cfg.Vault.EffectiveRegion(),
		"RESTIC_PROGRESS_FPS=0.0166", // one progress line a minute in the JSON output
	}, nil
}

// writeSecretFile writes a secret into path (0600, directory 0700), through a
// temporary file so a reader never sees half of it.
func writeSecretFile(cfg *config.Config, path, value string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	chownBarman(cfg, path)
	return nil
}

// ensureVaultPassword returns the restic password file, generated the first
// time. An existing file is never replaced: it is the only way to read the
// copies already made.
func (a *App) ensureVaultPassword(cfg *config.Config) (string, error) {
	p := cfg.Vault.EffectivePasswordFile(cfg.DataDir)
	b, err := os.ReadFile(p)
	switch {
	case err == nil && strings.TrimSpace(string(b)) == "":
		return "", fmt.Errorf("the restic password file %s is empty: put the recovery key back into it", p)
	case err == nil:
		return p, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("cannot read the restic password file: %w", err)
	}
	pw, err := vault.NewPassword()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return p, nil // written by another call a moment ago
	}
	if err != nil {
		return "", fmt.Errorf("cannot create the restic password file: %w", err)
	}
	if _, err := f.WriteString(pw + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	chownBarman(cfg, p, filepath.Dir(p))
	a.updateVaultInfo(func(vi *vaultInfo) { vi.KeyCreatedAt = a.now() })
	_ = a.Store.AddAudit(systemActor, "vault.key.create", "", "a new restic password was generated in "+p)
	a.Logf("vault: generated the restic password in %s; download the recovery key from the Vault page", p)
	return p, nil
}

// ---- restic ----

// resticGlobal are the options of every restic command on a server's
// repository.
func resticGlobal(cfg *config.Config, server string) []string {
	v := cfg.Vault
	return []string{
		"-r", vault.Repo(v.Endpoint, v.Bucket, v.EffectivePrefix(), server),
		"--password-file", v.EffectivePasswordFile(cfg.DataDir),
		"--cache-dir", filepath.Join(cfg.DataDir, "vault", "cache"),
		"-o", "s3.region=" + v.EffectiveRegion(),
	}
}

// resticCall runs restic commands on one repository and keeps a journal.
type resticCall struct {
	a         *App
	cfg       *config.Config
	server    string
	env       []string
	journal   *strings.Builder
	unlimited bool // the journal of a drill has no size limit of its own
}

func (c *resticCall) logf(format string, args ...any) {
	if c.journal != nil && (c.unlimited || c.journal.Len() < journalMax) {
		fmt.Fprintf(c.journal, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
}

// run executes `restic <global options> <args>` through the audited runner.
func (c *resticCall) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r, ok := c.a.Tools.(barman.EnvRunner)
	if !ok {
		return nil, nil, errors.New("the command runner cannot hand the S3 credentials to restic")
	}
	full := append(resticGlobal(c.cfg, c.server), args...)
	c.logf("$ %s %s", c.a.resticBin(), strings.Join(full, " "))
	out, errOut, err := r.RunBinaryEnv(barman.WithTarget(ctx, c.server), c.env, c.a.resticBin(), full...)
	if s := clip(errOut, 800); s != "" {
		c.logf("  stderr: %s", s)
	}
	if err != nil {
		c.logf("  -> exit %d", exitCodeOf(err))
	}
	return out, errOut, err
}

// resticError turns a failed restic command into one sentence.
func resticError(err error, stderr []byte) error {
	code := exitCodeOf(err)
	why := vault.Explain(code, string(stderr))
	last := lastLine(string(stderr))
	switch {
	case why != "" && last != "":
		return fmt.Errorf("%s (restic said: %s)", why, last)
	case why != "":
		return errors.New(why)
	case last != "":
		return fmt.Errorf("restic failed (exit %d): %s", code, last)
	}
	return fmt.Errorf("restic failed: %w", err)
}

// vaultPaths are what is copied for a server: its Barman directory and its
// barman.d file.
func (a *App) vaultPaths(server string) ([]string, error) {
	if !barman.ValidName(server) {
		return nil, fmt.Errorf("%q is not a server name", server)
	}
	dir := filepath.Join(a.barmanHome(), server)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("the Barman directory %s does not exist yet: there is nothing to copy before the first backup", dir)
	}
	paths := []string{dir}
	if files, err := barman.LoadServers(a.barmanConfigDir()); err == nil {
		if f := findFile(files, server); f != nil && f.File != "" {
			paths = append(paths, f.File)
		}
	}
	return paths, nil
}

// ---- queue ----

// enqueueVault puts a copy of server in the queue. A copy already waiting
// takes the stronger trigger (a click or a backup before a WAL pass). It
// returns the position in the queue (1 = next).
func (a *App) enqueueVault(server, trigger, actor string) int {
	h := &a.vault
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	if j, ok := h.pending[server]; ok {
		if j.trigger == VaultTriggerWAL && trigger != VaultTriggerWAL {
			h.pending[server] = vaultJob{trigger: trigger, actor: actor}
		}
	} else {
		h.pending[server] = vaultJob{trigger: trigger, actor: actor}
		h.order = append(h.order, server)
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
	for i, s := range h.order {
		if s == server {
			return i + 1
		}
	}
	return len(h.order)
}

// vaultBlocked reports why a copy of server must wait: a backup, a restore or
// a drill of that server is running.
func (a *App) vaultBlocked(server string) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	for _, k := range []string{server, drillRunKey + server, recoveryRunKey + server} {
		if _, busy := a.running[k]; busy {
			return true
		}
	}
	return false
}

// nextVaultJob takes the first queued copy that may run now and marks its
// server busy.
func (a *App) nextVaultJob() (string, vaultJob, bool) {
	h := &a.vault
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	for i, s := range h.order {
		if h.busy[s] != "" || a.vaultBlocked(s) {
			continue
		}
		j := h.pending[s]
		delete(h.pending, s)
		h.order = append(h.order[:i:i], h.order[i+1:]...)
		h.busy[s] = "copy"
		return s, j, true
	}
	return "", vaultJob{}, false
}

func (a *App) vaultRelease(server string) {
	h := &a.vault
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	delete(h.busy, server)
}

// vaultActivity is what the vault does now: the server being copied (with the
// id of its record) and the queue.
func (a *App) vaultActivity() (running map[string]string, current int64, queue []string) {
	h := &a.vault
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	running = map[string]string{}
	for k, v := range h.busy {
		running[k] = v
	}
	return running, h.current, append([]string(nil), h.order...)
}

// ---- loop ----

func (a *App) vaultLoop(ctx context.Context) {
	if a.firstCron != nil {
		select {
		case <-ctx.Done():
			return
		case <-a.firstCron:
		}
	}
	iv := a.VaultInterval
	if iv <= 0 {
		iv = DefaultVaultInterval
	}
	a.vault.mu.Lock()
	a.vault.init()
	wake := a.vault.wake
	a.vault.mu.Unlock()
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		a.VaultTick(ctx, a.now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// VaultTick queues the WAL passes that are due, reads the Object Lock rule
// once a day, then runs the queued copies one after the other. The loop
// calls it every vault interval and whenever a copy is queued.
func (a *App) VaultTick(ctx context.Context, now time.Time) {
	cfg := a.Config()
	if !cfg.Vault.Enabled || a.Store == nil {
		return
	}
	a.queueWALPasses(cfg, now)
	a.maybeCheckLock(ctx, cfg, now)
	for ctx.Err() == nil {
		server, job, ok := a.nextVaultJob()
		if !ok {
			return
		}
		a.RunVaultCopy(ctx, server, job.trigger, job.actor)
	}
}

// queueWALPasses queues a copy of every server whose last attempt is older
// than vault.wal_interval. A server without a Barman directory yet is left
// alone: there is nothing to copy.
func (a *App) queueWALPasses(cfg *config.Config, now time.Time) {
	iv := cfg.Vault.EffectiveWALInterval()
	if iv <= 0 {
		return
	}
	for _, srv := range cfg.Servers {
		if !cfg.VaultFor(srv) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(a.barmanHome(), srv.Name)); err != nil || !fi.IsDir() {
			continue
		}
		last, err := a.Store.ListVaultCopies(srv.Name, 1)
		if err != nil {
			continue
		}
		if len(last) == 0 || now.Sub(last[0].StartedAt) >= iv {
			a.enqueueVault(srv.Name, VaultTriggerWAL, systemActor)
		}
	}
}

// vaultAfterBackup queues the copy that follows a successful backup.
func (a *App) vaultAfterBackup(server, actor string) {
	cfg := a.Config()
	if cs := findCfg(cfg, server); cs != nil && cfg.VaultFor(*cs) {
		a.enqueueVault(server, VaultTriggerBackup, actor)
	}
}

// VaultQueued is the answer to "Copy now".
type VaultQueued struct {
	Server   string `json:"server"`
	Position int    `json:"position"` // 1 = next
	Running  bool   `json:"running"`  // a copy of this server runs already; this one follows it
}

// VaultCopyNow queues a copy of a server, ahead of nothing: copies run one
// at a time, in the order they were asked.
func (a *App) VaultCopyNow(ctx context.Context, actor, server string) (*VaultQueued, error) {
	cfg := a.Config()
	if !cfg.Vault.Enabled {
		return nil, &ConflictError{Msg: ErrVaultOff.Error()}
	}
	if err := a.knownServer(ctx, server); err != nil {
		return nil, err
	}
	cs := findCfg(cfg, server)
	if cs == nil || !cfg.VaultFor(*cs) {
		return nil, &ConflictError{Msg: fmt.Sprintf("%s is not copied to the vault (vault: false in its entry of pgcellar.yaml).", server)}
	}
	running, _, _ := a.vaultActivity()
	pos := a.enqueueVault(server, VaultTriggerManual, actor)
	_ = a.Store.AddAudit(actor, "vault.copy", server, fmt.Sprintf("queued, position %d", pos))
	return &VaultQueued{Server: server, Position: pos, Running: running[server] != ""}, nil
}

// ---- one copy ----

// vaultCopyDetails is the JSON kept with a copy.
type vaultCopyDetails struct {
	Warnings    []string `json:"warnings,omitempty"`
	Initialised bool     `json:"initialised,omitempty"` // the repository was created by this copy
	Unchanged   bool     `json:"unchanged,omitempty"`   // nothing changed since the previous copy
	Pruned      bool     `json:"pruned,omitempty"`
	PruneError  string   `json:"prune_error,omitempty"`
	Journal     string   `json:"journal,omitempty"`
}

// RunVaultCopy copies one server now, in the calling goroutine, and records
// the outcome. The vault loop calls it for the queued copies; the server must
// have been marked busy (nextVaultJob does it).
func (a *App) RunVaultCopy(ctx context.Context, server, trigger, actor string) int64 {
	cfg := a.Config()
	t0 := time.Now()
	id, err := a.Store.StartVaultCopy(server, trigger, actor, a.now())
	if err != nil {
		a.Logf("vault: cannot record the copy of %s: %v", server, err)
		a.vaultRelease(server)
		return 0
	}
	a.vault.mu.Lock()
	a.vault.current = id
	a.vault.mu.Unlock()
	defer func() {
		a.vault.mu.Lock()
		a.vault.current = 0
		a.vault.mu.Unlock()
		a.vaultRelease(server)
	}()

	cctx, cancel := context.WithTimeout(barman.WithActor(ctx, actor), vaultCopyTimeout)
	defer cancel()
	var journal strings.Builder
	var det vaultCopyDetails
	rec := store.VaultCopy{}
	sum, err := a.copyToVault(cctx, cfg, server, &journal, &det)
	if err == nil {
		rec.OK = true
		rec.SnapshotID, rec.BytesAdded, rec.BytesTotal, rec.FilesNew = sum.SnapshotID, sum.Uploaded(), sum.TotalBytesProcessed, sum.FilesNew
		det.Unchanged = sum.SnapshotID == ""
		a.afterVaultCopy(cctx, cfg, server, trigger, &journal, &det)
	} else {
		rec.Error = err.Error()
		if ctx.Err() != nil {
			rec.Error = "interrupted: pgCellar was stopping"
		}
	}
	rec.DurationMs = time.Since(t0).Milliseconds()
	det.Journal = journal.String()
	if b, merr := json.Marshal(det); merr == nil {
		rec.Details = string(b)
	}
	if ferr := a.Store.FinishVaultCopy(id, rec, a.now()); ferr != nil {
		a.Logf("vault: cannot record the end of the copy of %s: %v", server, ferr)
	}
	if !rec.OK {
		a.Logf("vault: copy of %s failed: %s", server, rec.Error)
	} else if trigger != VaultTriggerWAL {
		a.Logf("vault: copy of %s done, %s sent in %s", server, FormatSize(rec.BytesAdded), time.Since(t0).Round(time.Second))
	}
	return id
}

// copyToVault runs restic backup, creating the repository on the first copy
// and clearing a lock left by a killed process.
func (a *App) copyToVault(ctx context.Context, cfg *config.Config, server string, journal *strings.Builder, det *vaultCopyDetails) (vault.BackupSummary, error) {
	env, err := vaultEnv(cfg)
	if err != nil {
		return vault.BackupSummary{}, err
	}
	if _, err := a.ensureVaultPassword(cfg); err != nil {
		return vault.BackupSummary{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "vault", "cache"), 0o700); err != nil {
		return vault.BackupSummary{}, err
	}
	paths, err := a.vaultPaths(server)
	if err != nil {
		return vault.BackupSummary{}, err
	}
	c := &resticCall{a: a, cfg: cfg, server: server, env: env, journal: journal}
	args := append([]string{"backup", "--json", "--host", vault.Host, "--tag", "pgcellar", "--tag", "server=" + server, "--skip-if-unchanged"}, paths...)
	var out, errOut []byte
	for attempt := 0; attempt < 3; attempt++ {
		out, errOut, err = c.run(ctx, args...)
		code := exitCodeOf(err)
		switch {
		case err == nil || code == vault.ExitPartial:
		case code == vault.ExitNoRepo || (code == 1 && strings.Contains(string(errOut), "Is there a repository at the following location")):
			if _, ierr, e := c.run(ctx, "init"); e != nil {
				return vault.BackupSummary{}, fmt.Errorf("cannot create the restic repository: %w", resticError(e, ierr))
			}
			det.Initialised = true
			c.logf("created the repository")
			continue
		case code == vault.ExitLocked && attempt == 0:
			// A copy killed by a restart leaves a lock; unlock only removes stale ones.
			_, _, _ = c.run(ctx, "unlock")
			continue
		default:
			return vault.BackupSummary{}, resticError(err, errOut)
		}
		break
	}
	if err != nil && exitCodeOf(err) != vault.ExitPartial {
		return vault.BackupSummary{}, resticError(err, errOut)
	}
	sum, warnings, perr := vault.ParseBackup(out)
	if perr != nil {
		return sum, fmt.Errorf("restic ended without a summary: %w", perr)
	}
	det.Warnings = warnings
	if exitCodeOf(err) == vault.ExitPartial {
		det.Warnings = append(det.Warnings, "some files changed or vanished during the copy (Barman moves WAL files while it works); they are taken by the next copy")
	}
	c.logf("snapshot %s: %d new files, %s added, %s covered", shortID(sum.SnapshotID), sum.FilesNew, FormatSize(sum.Uploaded()), FormatSize(sum.TotalBytesProcessed))
	return sum, nil
}

func shortID(id string) string {
	if id == "" {
		return "(unchanged)"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// afterVaultCopy forgets old snapshots once a day and measures what the
// server takes in the bucket. Neither failure fails the copy.
func (a *App) afterVaultCopy(ctx context.Context, cfg *config.Config, server, trigger string, journal *strings.Builder, det *vaultCopyDetails) {
	now := a.now()
	pruneDue := false
	if v, ok, err := a.Store.GetSetting(settingVaultPrune + server); err == nil && ok {
		if t, perr := time.Parse(time.RFC3339, v); perr != nil || now.Sub(t) >= vaultPruneEvery {
			pruneDue = true
		}
	} else if err == nil {
		// First copy: start the clock, there is nothing to forget yet.
		_ = a.Store.SetSetting(settingVaultPrune+server, now.Format(time.RFC3339))
	}
	if pruneDue {
		env, err := vaultEnv(cfg)
		if err == nil {
			c := &resticCall{a: a, cfg: cfg, server: server, env: env, journal: journal}
			args := append([]string{"forget", "--json", "--prune", "--host", vault.Host}, cfg.Vault.Keep.Args()...)
			_, errOut, rerr := c.run(ctx, args...)
			if rerr != nil {
				err = resticError(rerr, errOut)
			}
		}
		if err != nil {
			det.PruneError = err.Error()
		} else {
			det.Pruned = true
			_ = a.Store.SetSetting(settingVaultPrune+server, now.Format(time.RFC3339))
		}
	}
	measure := trigger != VaultTriggerWAL || det.Pruned
	if !measure {
		u, ok := a.currentVaultInfo().Usage[server]
		measure = !ok || now.Sub(u.MeasuredAt) >= vaultUsageEvery
	}
	if measure {
		a.measureVaultUsage(ctx, cfg, server)
	}
}

// measureVaultUsage lists the objects of the server's folder (versions
// included: on a bucket with Object Lock, what restic deleted is still there
// and billed), or asks restic when the listing is refused.
func (a *App) measureVaultUsage(ctx context.Context, cfg *config.Config, server string) {
	u := VaultUsage{MeasuredAt: a.now(), Method: "s3"}
	client, err := s3Client(cfg)
	if err == nil {
		u.Usage, err = client.Usage(ctx, vault.ServerPrefix(cfg.Vault.EffectivePrefix(), server))
	}
	if err != nil {
		u.Method = "restic"
		env, eerr := vaultEnv(cfg)
		if eerr == nil {
			c := &resticCall{a: a, cfg: cfg, server: server, env: env}
			out, errOut, rerr := c.run(ctx, "stats", "--json", "--mode", "raw-data")
			if rerr == nil {
				if st, perr := vault.ParseStats(out); perr == nil {
					u.Usage = vault.Usage{CurrentBytes: st.TotalSize}
					err = nil
				}
			} else {
				err = resticError(rerr, errOut)
			}
		} else {
			err = eerr
		}
	}
	if err != nil {
		u.Error = err.Error()
		a.updateVaultInfo(func(vi *vaultInfo) {
			if old, ok := vi.Usage[server]; ok { // keep the last good figure
				old.Error = u.Error
				vi.Usage[server] = old
				return
			}
			vi.Usage[server] = u
		})
		return
	}
	a.updateVaultInfo(func(vi *vaultInfo) { vi.Usage[server] = u })
}

// s3Client is the S3 client of the configured bucket.
func s3Client(cfg *config.Config) (*vault.S3, error) {
	secret, err := vaultSecret(cfg)
	if err != nil {
		return nil, err
	}
	v := cfg.Vault
	return &vault.S3{Endpoint: v.Endpoint, Region: v.EffectiveRegion(), Bucket: v.Bucket, AccessKey: v.AccessKey, SecretKey: secret}, nil
}

// maybeCheckLock reads the Object Lock rule of the bucket once a day.
func (a *App) maybeCheckLock(ctx context.Context, cfg *config.Config, now time.Time) {
	vi := a.currentVaultInfo()
	if !vi.LockCheckedAt.IsZero() && now.Sub(vi.LockCheckedAt) < vaultLockEvery {
		return
	}
	_, _ = a.checkVaultLock(ctx, cfg)
}

func (a *App) checkVaultLock(ctx context.Context, cfg *config.Config) (vault.Lock, error) {
	client, err := s3Client(cfg)
	var lock vault.Lock
	if err == nil {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		lock, err = client.ObjectLock(lctx)
		cancel()
	}
	a.updateVaultInfo(func(vi *vaultInfo) {
		vi.LockCheckedAt = a.now()
		vi.LockError = ""
		if err != nil {
			vi.LockError = err.Error()
			return
		}
		vi.Lock = lock
	})
	return lock, err
}

// PurgeVaultCopies removes the copy records older than 30 days (the newest
// and the newest good copy of each server stay).
func (a *App) PurgeVaultCopies() {
	n, err := a.Store.PruneVaultCopies(a.now().AddDate(0, 0, -vaultHistoryDays))
	if err != nil {
		a.Logf("vault: cannot purge the old copy records: %v", err)
	} else if n > 0 {
		a.Logf("vault: removed %d copy record(s) older than %d days", n, vaultHistoryDays)
	}
}

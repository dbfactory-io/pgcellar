package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/store"
)

// outputTail is the most that is kept of a run's output.
const outputTail = 16 * 1024

func tail(b []byte) string {
	if len(b) > outputTail {
		return "[...truncated...]\n" + string(b[len(b)-outputTail:])
	}
	return string(b)
}

// ---- servers ----

// walMethodOf reads the WAL method out of the two Barman switches.
func walMethodOf(s barman.Server) string {
	arch, stream := barman.IsOn(s.Archiver), barman.IsOn(s.StreamingArchiver)
	switch {
	case arch && stream:
		return WALBoth
	case arch:
		return WALArchiveCommand
	default:
		return WALStreaming
	}
}

func inputFrom(s barman.Server, cs *config.Server) ServerInput {
	in := ServerInput{
		Name:              s.Name,
		Description:       s.Description,
		BackupMethod:      s.BackupMethod,
		ConnInfo:          s.ConnInfo,
		StreamingConnInfo: s.StreamingConnInfo,
		Slot:              s.Slot,
		SSHCommand:        s.SSHCommand,
	}
	if in.BackupMethod == "" {
		in.BackupMethod = "rsync" // Barman's own default
	}
	in.WALMethod = walMethodOf(s)
	if r, err := config.BuildRetention(s.RetentionPolicy, s.MinimumRedundancy); err == nil {
		in.Retention = r
	}
	if cs != nil {
		in.Label = cs.Label
		in.Schedule = cs.Schedule
		if s.RetentionPolicy == "" && s.MinimumRedundancy == "" {
			in.Retention = cs.Retention
		}
	}
	return in
}

func findCfg(cfg *config.Config, name string) *config.Server {
	for i := range cfg.Servers {
		if cfg.Servers[i].Name == name {
			return &cfg.Servers[i]
		}
	}
	return nil
}

func findFile(files []barman.Server, name string) *barman.Server {
	for i := range files {
		if files[i].Name == name {
			return &files[i]
		}
	}
	return nil
}

func (a *App) detail(name string, file *barman.Server, cs *config.Server, inBarman bool) ServerDetail {
	d := ServerDetail{InBarman: inBarman, Problems: []string{}}
	if file != nil {
		d.ServerInput = inputFrom(*file, cs)
		d.HasFile = true
		d.File = file.File
		if p := barman.Validate(*file); p != nil {
			d.Problems = p
		}
	} else {
		d.ServerInput = ServerInput{Name: name}
		if cs != nil {
			d.Label = cs.Label
			d.Schedule = cs.Schedule
			d.Retention = cs.Retention
		}
	}
	if d.Schedule.Backup != "" {
		if _, human, err := config.ParseSchedule(d.Schedule.Backup); err == nil {
			d.ScheduleHuman = human
		}
	}
	if st, ok := a.State(name); ok {
		d.State = &st
	}
	d.NextBackup, d.LastScheduled = a.scheduleInfo(name, a.now())
	return d
}

// serverNameSet is every server pgCellar knows without asking Barman: the
// files of barman.d, pgcellar.yaml and the status cache.
func (a *App) serverNameSet(files []barman.Server, cfg *config.Config) map[string]bool {
	set := map[string]bool{}
	for _, f := range files {
		set[f.Name] = true
	}
	for _, s := range cfg.Servers {
		set[s.Name] = true
	}
	a.cacheMu.RLock()
	for n := range a.cache {
		set[n] = true
	}
	a.cacheMu.RUnlock()
	return set
}

// inBarman tells whether Barman listed the server at the last refresh. Before
// the first refresh nothing is known, so a server with a file is assumed to be
// listed.
func (a *App) inBarman(name string, hasFile bool) bool {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	if a.cacheTime.IsZero() {
		return hasFile
	}
	for _, n := range a.cacheOrder {
		if n == name {
			return true
		}
	}
	return false
}

// ListServers merges the servers of barman.d, pgcellar.yaml and the status
// cache. It never runs Barman.
func (a *App) ListServers(ctx context.Context) ([]ServerDetail, error) {
	files, err := barman.LoadServers(a.barmanConfigDir())
	if err != nil {
		return nil, fmt.Errorf("cannot read the Barman configuration directory: %w", err)
	}
	cfg := a.Config()
	set := a.serverNameSet(files, cfg)
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	byName := make(map[string]*barman.Server, len(files))
	for i := range files {
		byName[files[i].Name] = &files[i]
	}
	out := make([]ServerDetail, 0, len(names))
	for _, n := range names {
		f := byName[n]
		out = append(out, a.detail(n, f, findCfg(cfg, n), a.inBarman(n, f != nil)))
	}
	return out, nil
}

// GetServer returns one server, or ErrNotFound. It never runs Barman.
func (a *App) GetServer(ctx context.Context, name string) (*ServerDetail, error) {
	files, err := barman.LoadServers(a.barmanConfigDir())
	if err != nil {
		return nil, fmt.Errorf("cannot read the Barman configuration directory: %w", err)
	}
	cfg := a.Config()
	f := findFile(files, name)
	if f == nil && !a.serverNameSet(nil, cfg)[name] {
		return nil, ErrNotFound
	}
	d := a.detail(name, f, findCfg(cfg, name), a.inBarman(name, f != nil))
	return &d, nil
}

func normalize(in *ServerInput) {
	for _, p := range []*string{&in.Name, &in.Label, &in.Description, &in.BackupMethod, &in.ConnInfo, &in.StreamingConnInfo, &in.Slot, &in.SSHCommand, &in.Schedule.Backup} {
		*p = strings.TrimSpace(*p)
	}
	in.BackupMethod = strings.ToLower(in.BackupMethod)
	in.WALMethod = strings.ToLower(strings.TrimSpace(in.WALMethod))
}

// toBarman applies the input on top of an existing section (or a new one).
func toBarman(in ServerInput, existing *barman.Server) barman.Server {
	var s barman.Server
	isNew := existing == nil
	if existing != nil {
		s = *existing
		s.Extra = map[string]string{}
		for k, v := range existing.Extra {
			s.Extra[k] = v
		}
	} else {
		s = barman.Server{Name: in.Name, Extra: map[string]string{}}
	}
	methodChanged := isNew || strings.ToLower(s.BackupMethod) != in.BackupMethod
	s.Description = in.Description
	s.BackupMethod = in.BackupMethod
	s.ConnInfo = in.ConnInfo
	s.StreamingConnInfo = in.StreamingConnInfo
	s.Slot = in.Slot
	s.SSHCommand = in.SSHCommand
	wal := in.WALMethod
	if wal == "" && methodChanged {
		wal = WALStreaming
		if in.BackupMethod == "rsync" {
			wal = WALArchiveCommand
		}
	}
	switch wal {
	case WALStreaming:
		s.StreamingArchiver, s.Archiver = "on", "off"
	case WALArchiveCommand:
		s.StreamingArchiver, s.Archiver = "off", "on"
	case WALBoth:
		s.StreamingArchiver, s.Archiver = "on", "on"
	}
	if in.BackupMethod == "postgres" && barman.IsOn(s.StreamingArchiver) {
		if _, ok := s.Extra["create_slot"]; !ok {
			s.Extra["create_slot"] = "auto"
		}
	}
	r := config.Retention{RecoveryWindowDays: in.Retention.RecoveryWindowDays, Redundancy: in.Retention.Redundancy, MinimumRedundancy: in.Retention.MinimumRedundancy}
	s.RetentionPolicy = config.RetentionPolicy(r)
	s.MinimumRedundancy = config.MinimumRedundancy(r)
	return s
}

func upsertServer(cfg *config.Config, in ServerInput) {
	entry := config.Server{
		Name:  in.Name,
		Label: in.Label,
		Schedule: config.Schedule{
			Backup:  in.Schedule.Backup,
			Enabled: in.Schedule.Enabled,
		},
		Retention: config.Retention{
			RecoveryWindowDays: in.Retention.RecoveryWindowDays,
			Redundancy:         in.Retention.Redundancy,
			MinimumRedundancy:  in.Retention.MinimumRedundancy,
		},
	}
	servers := make([]config.Server, 0, len(cfg.Servers)+1)
	replaced := false
	for _, s := range cfg.Servers {
		if s.Name == in.Name {
			entry.Tags = s.Tags
			entry.Drills = s.Drills // set in pgcellar.yaml, not in the server form
			s = entry
			replaced = true
		}
		servers = append(servers, s)
	}
	if !replaced {
		servers = append(servers, entry)
	}
	cfg.Servers = servers
}

// problemsFor checks an input without touching any file.
func (a *App) problemsFor(in ServerInput, bs barman.Server) []string {
	var p []string
	if in.Schedule.Backup != "" {
		if _, _, err := config.ParseSchedule(in.Schedule.Backup); err != nil {
			p = append(p, "Schedule: "+err.Error()+".")
		}
	} else if in.Schedule.Enabled {
		p = append(p, "Schedule: give a schedule such as \"daily 02:00\" or untick \"enabled\".")
	}
	r := in.Retention
	switch {
	case r.RecoveryWindowDays < 0 || r.Redundancy < 0 || r.MinimumRedundancy < 0:
		p = append(p, "Retention: values cannot be negative.")
	case r.RecoveryWindowDays > 0 && r.Redundancy > 0:
		p = append(p, "Retention: set either a recovery window or a redundancy, not both.")
	case r.Redundancy > 0 && r.MinimumRedundancy > r.Redundancy:
		p = append(p, "Retention: the minimum redundancy cannot be higher than the redundancy.")
	}
	if in.Name != "" && !serverNameOK(in.Name) {
		p = append(p, fmt.Sprintf("The server name %q is not valid: use lowercase letters, digits, '-' and '_' only.", in.Name))
	}
	switch in.WALMethod {
	case "", WALStreaming, WALArchiveCommand, WALBoth:
	default:
		p = append(p, fmt.Sprintf("WAL method %q is not known: use streaming, archive_command or both.", in.WALMethod))
	}
	p = append(p, barman.Validate(bs)...)
	return p
}

func serverNameOK(n string) bool {
	if n == "" {
		return false
	}
	for _, c := range n {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return barman.ValidName(n)
}

// CreateServer writes a new barman.d file and the pgcellar.yaml entry, then
// commits both with the user as author.
func (a *App) CreateServer(actor string, in ServerInput) (*WriteResult, error) {
	normalize(&in)
	if in.BackupMethod == "" {
		in.BackupMethod = "postgres"
	}
	bs := toBarman(in, nil)
	if p := a.problemsFor(in, bs); len(p) > 0 {
		return nil, &ProblemsError{Problems: p}
	}
	dir := a.barmanConfigDir()
	files, err := barman.LoadServers(dir)
	if err != nil {
		return nil, err
	}
	if findFile(files, in.Name) != nil {
		return nil, &ConflictError{Msg: fmt.Sprintf("A server named %q already exists.", in.Name)}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", dir, err)
	}
	if err := barman.SaveNewServer(dir, bs); err != nil {
		return nil, err
	}
	if err := a.mutateConfig(func(cfg *config.Config) error { upsertServer(cfg, in); return nil }); err != nil {
		_ = os.Remove(filepath.Join(dir, in.Name+".conf"))
		return nil, err
	}
	_ = a.Store.AddAudit(actor, "server.create", in.Name, "")
	return a.finishWrite("add server "+in.Name, actor), nil
}

// UpdateServer rewrites the barman.d section and the pgcellar.yaml entry.
func (a *App) UpdateServer(actor, name string, in ServerInput) (*WriteResult, error) {
	in.Name = name
	normalize(&in)
	files, err := barman.LoadServers(a.barmanConfigDir())
	if err != nil {
		return nil, err
	}
	existing := findFile(files, name)
	if existing == nil {
		if a.knownInConfig(name) {
			return nil, ErrNoFile
		}
		return nil, ErrNotFound
	}
	bs := toBarman(in, existing)
	if p := a.problemsFor(in, bs); len(p) > 0 {
		return nil, &ProblemsError{Problems: p}
	}
	previous, readErr := os.ReadFile(existing.File)
	if err := barman.SaveServer(bs); err != nil {
		return nil, err
	}
	if err := a.mutateConfig(func(cfg *config.Config) error { upsertServer(cfg, in); return nil }); err != nil {
		if readErr == nil {
			_ = os.WriteFile(existing.File, previous, 0o640)
		}
		return nil, err
	}
	_ = a.Store.AddAudit(actor, "server.update", name, "")
	return a.finishWrite("update server "+name, actor), nil
}

func (a *App) knownInConfig(name string) bool { return findCfg(a.Config(), name) != nil }

func (a *App) finishWrite(message, actor string) *WriteResult {
	res := &WriteResult{}
	h, err := a.Commit(message, actor)
	res.Commit = h
	if err != nil {
		res.Warning = "The change was saved but could not be recorded in git: " + err.Error()
		a.Logf("%s: %s", message, res.Warning)
	}
	return res
}

// CurrentInput returns the current input of a server with the JSON-decoded
// overrides applied by the caller; used by PUT so that omitted fields keep
// their value.
func (a *App) CurrentInput(ctx context.Context, name string) (ServerInput, error) {
	d, err := a.GetServer(ctx, name)
	if err != nil {
		return ServerInput{}, err
	}
	if !d.HasFile {
		return ServerInput{}, ErrNoFile
	}
	return d.ServerInput, nil
}

// ---- Barman actions ----

func (a *App) knownServer(ctx context.Context, name string) error {
	if !barman.ValidName(name) {
		return ErrNotFound
	}
	if files, err := barman.LoadServers(a.barmanConfigDir()); err == nil && findFile(files, name) != nil {
		return nil
	}
	if names, err := a.Engine.ListServers(barman.WithActor(ctx, systemActor)); err == nil {
		for _, n := range names {
			if n == name {
				return nil
			}
		}
	}
	return ErrNotFound
}

// Check runs `barman check` live and explains every line.
func (a *App) Check(ctx context.Context, actor, name string) ([]CheckView, error) {
	if err := a.knownServer(ctx, name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(barman.WithActor(ctx, actor), 3*time.Minute)
	defer cancel()
	res, err := a.Engine.Check(ctx, name)
	if err != nil {
		return nil, err
	}
	a.applyCheck(ctx, name, res)
	return checkViews(res), nil
}

// ErrRefreshPending means the status cache has no entry for the server yet
// (pgCellar just started); the first refresh will fill it.
var ErrRefreshPending = errors.New("first refresh pending")

// Backups lists the backups of a server, newest first, from the status cache.
// It never runs Barman. The list is as old as the last refresh.
func (a *App) Backups(ctx context.Context, actor, name string) ([]BackupView, error) {
	if !barman.ValidName(name) {
		return nil, ErrNotFound
	}
	st, ok := a.State(name)
	if !ok {
		files, err := barman.LoadServers(a.barmanConfigDir())
		if err == nil && findFile(files, name) != nil || findCfg(a.Config(), name) != nil {
			return nil, ErrRefreshPending
		}
		return nil, ErrNotFound
	}
	if st.Backups == nil {
		return []BackupView{}, nil
	}
	return st.Backups, nil
}

// captureRunner keeps what Barman printed, for the run record.
type captureRunner struct {
	inner          barman.Runner
	stdout, stderr bytes.Buffer
}

func (c *captureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	out, errOut, err := c.inner.Run(ctx, args...)
	c.stdout.Write(out)
	c.stderr.Write(errOut)
	return out, errOut, err
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var re *barman.RunError
	if errors.As(err, &re) {
		return re.ExitCode
	}
	return -1
}

// StartBackup starts `barman backup` in the background and returns the id of
// the run to follow. Only one backup per server runs at a time.
func (a *App) StartBackup(ctx context.Context, actor, name string) (int64, error) {
	if err := a.knownServer(ctx, name); err != nil {
		return 0, err
	}
	a.runMu.Lock()
	if id, busy := a.running[name]; busy {
		a.runMu.Unlock()
		return id, &ConflictError{Msg: fmt.Sprintf("A backup of %s is already running (run #%d).", name, id)}
	}
	if id, busy := a.running[drillRunKey+name]; busy {
		a.runMu.Unlock()
		return id, &ConflictError{Msg: fmt.Sprintf("A restore drill of %s is running (run #%d): a backup never runs at the same time, try again when it ends.", name, id), Retry: true}
	}
	id, err := a.Store.StartRun(name, store.KindBackup, actor)
	if err != nil {
		a.runMu.Unlock()
		return 0, err
	}
	a.running[name] = id
	a.runMu.Unlock()
	_ = a.Store.AddAudit(actor, "backup.start", name, fmt.Sprintf("run #%d", id))

	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		bctx, cancel := context.WithTimeout(barman.WithActor(context.Background(), actor), 12*time.Hour)
		defer cancel()
		cr := &captureRunner{inner: a.Runner}
		eng := barman.NewEngine(cr)
		berr := eng.Backup(bctx, name)
		if berr == nil {
			// The backup's end LSN usually sits in the WAL segment still being
			// written; on a quiet server that segment would stay open for hours
			// and the backup would remain WAITING_FOR_WALS. Close it now.
			if serr := eng.SwitchWAL(bctx, name); serr != nil {
				a.Logf("switch-wal after the backup of %s failed: %v", name, serr)
			}
		}
		code := exitCodeOf(berr)
		stderr := cr.stderr.String()
		if berr != nil && !strings.Contains(stderr, berr.Error()) {
			stderr += "\n" + berr.Error()
		}
		if err := a.Store.FinishRun(id, code, tail(cr.stdout.Bytes()), tail([]byte(strings.TrimSpace(stderr)))); err != nil {
			a.Logf("cannot record the end of run #%d: %v", id, err)
		}
		a.runMu.Lock()
		delete(a.running, name)
		a.runMu.Unlock()
		a.refreshServer(bctx, name)
	}()
	return id, nil
}

// SwitchWAL closes the current WAL segment and archives it. It returns what
// Barman printed.
func (a *App) SwitchWAL(ctx context.Context, actor, name string) (string, error) {
	if err := a.knownServer(ctx, name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(barman.WithActor(ctx, actor), 5*time.Minute)
	defer cancel()
	cr := &captureRunner{inner: a.Runner}
	err := barman.NewEngine(cr).SwitchWAL(ctx, name)
	_ = a.Store.AddAudit(actor, "switch-wal", name, fmt.Sprintf("exit=%d", exitCodeOf(err)))
	out := strings.TrimSpace(cr.stdout.String())
	if err != nil {
		return out, err
	}
	return out, nil
}

// DeleteBackup removes one complete backup with `barman delete`. Barman
// refuses by itself to go below minimum_redundancy; its message is returned as
// a ConflictError. The cache of the server is refreshed afterwards.
func (a *App) DeleteBackup(ctx context.Context, actor, server, backupID string) error {
	if !barman.BackupIDPattern.MatchString(backupID) {
		return &ProblemsError{Problems: []string{fmt.Sprintf("%q is not a backup id (expected something like 20261020T124328).", backupID)}}
	}
	if err := a.knownServer(ctx, server); err != nil {
		return err
	}
	if st, ok := a.State(server); ok {
		found := false
		for _, b := range st.Backups {
			if b.ID == backupID {
				found = true
			}
		}
		if !found {
			return ErrNotFound
		}
	}
	// A delete can take minutes on a large backup: it must not stop because
	// the browser went away.
	dctx, cancel := context.WithTimeout(barman.WithActor(context.WithoutCancel(ctx), actor), time.Hour)
	defer cancel()
	_, err := a.Engine.DeleteBackup(dctx, server, backupID)
	details := "exit=0"
	if err != nil {
		details = "failed: " + err.Error()
	}
	_ = a.Store.AddAudit(actor, "backup.delete", server+"/"+backupID, details)
	a.refreshServer(dctx, server)
	if err != nil {
		var be *barman.BarmanError
		if errors.As(err, &be) {
			if strings.Contains(be.Msg, "Unknown backup") {
				return ErrNotFound
			}
			return &ConflictError{Msg: be.Msg}
		}
		return err
	}
	return nil
}

// ---- runs, history, audit ----

func toRunView(r store.Run, withOutput bool) RunView {
	v := RunView{ID: r.ID, Server: r.Server, Kind: r.Kind, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, ExitCode: r.ExitCode, Actor: r.Actor}
	switch {
	case r.FinishedAt == nil:
		v.Status = RunRunning
	case r.ExitCode != nil && *r.ExitCode == 0:
		v.Status = RunOK
	default:
		v.Status = RunFailed
	}
	if withOutput {
		v.Stdout, v.Stderr = r.StdoutTail, r.StderrTail
	}
	return v
}

// Runs lists recent runs, newest first. An empty server means all.
func (a *App) Runs(server string, limit int) ([]RunView, error) {
	runs, err := a.Store.ListRuns(server, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RunView, 0, len(runs))
	for _, r := range runs {
		out = append(out, toRunView(r, false))
	}
	return out, nil
}

// Run returns one run with its output, or ErrNotFound.
func (a *App) Run(id int64) (*RunView, error) {
	r, err := a.Store.GetRun(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := toRunView(*r, true)
	return &v, nil
}

// History returns the latest commits of the configuration repositories.
func (a *App) History(n int) ([]HistoryItem, error) {
	var out []HistoryItem
	add := func(repo, dir string) error {
		log, err := config.Log(dir, n)
		if err != nil {
			return err
		}
		for _, e := range log {
			out = append(out, HistoryItem{Repo: repo, Hash: e.Hash, Date: e.Date, Author: e.Author, Message: e.Message})
		}
		return nil
	}
	if err := add("config", a.ConfigDir); err != nil {
		return nil, err
	}
	if d := a.barmanDirIfSeparate(); d != "" {
		if err := add("barman", d); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// Diff returns the patch of one commit of "config" or "barman".
func (a *App) Diff(repo, hash string) (string, error) {
	dir := a.ConfigDir
	if repo == "barman" {
		if dir = a.barmanDirIfSeparate(); dir == "" {
			return "", ErrNotFound
		}
	}
	return config.Diff(dir, hash)
}

// Audit returns the latest audit entries.
func (a *App) Audit(limit int) ([]AuditItem, error) {
	list, err := a.Store.ListAudit(limit)
	if err != nil {
		return nil, err
	}
	out := make([]AuditItem, 0, len(list))
	for _, e := range list {
		out = append(out, AuditItem{At: e.At, Actor: e.Actor, Action: e.Action, Target: e.Target, Details: e.Details})
	}
	return out, nil
}

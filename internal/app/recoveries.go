package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

// Targets of a recovery.
const (
	TargetLatest    = "latest"     // as far as the archived WAL goes
	TargetBackupEnd = "backup_end" // the state at the end of the chosen backup
	TargetTime      = "time"       // a point in time
)

// RecoveryInput is what a restore needs.
type RecoveryInput struct {
	BackupID   string `json:"backup_id"`
	Target     string `json:"target"`                // latest | backup_end | time
	TargetTime string `json:"target_time,omitempty"` // with target "time": RFC 3339, or "YYYY-MM-DD HH:MM:SS+TZ"
	Archive    bool   `json:"archive"`               // also build a .tar.gz next to the directory
}

// RecoveryView is one finished restore.
type RecoveryView struct {
	ID           int64     `json:"id"`
	Server       string    `json:"server"`
	BackupID     string    `json:"backup_id"`
	Target       string    `json:"target"`
	TargetTime   string    `json:"target_time,omitempty"`
	Path         string    `json:"path"`
	ArchivePath  string    `json:"archive_path,omitempty"`
	SizeBytes    int64     `json:"size_bytes"`
	ArchiveBytes int64     `json:"archive_bytes"`
	Downloadable bool      `json:"downloadable"` // the archive exists and is within max_download_bytes
	RunID        int64     `json:"run_id"`
	CreatedAt    time.Time `json:"created_at"`
	Actor        string    `json:"actor"`
}

// ErrTooLarge means an archive exceeds recoveries.max_download_bytes.
var ErrTooLarge = errors.New("the archive is larger than recoveries.max_download_bytes: read it from the host path instead")

func (a *App) recoveryView(r store.Recovery, max int64) RecoveryView {
	v := RecoveryView{ID: r.ID, Server: r.Server, BackupID: r.BackupID, Target: r.Target, TargetTime: r.TargetTime,
		Path: r.Path, ArchivePath: r.ArchivePath, SizeBytes: r.SizeBytes, ArchiveBytes: r.ArchiveSize,
		RunID: r.RunID, CreatedAt: r.CreatedAt, Actor: r.Actor}
	if r.ArchivePath != "" && (max <= 0 || r.ArchiveSize <= max) {
		if fi, err := os.Stat(r.ArchivePath); err == nil && fi.Mode().IsRegular() {
			v.Downloadable = true
		}
	}
	return v
}

// ---- paths ----

var errNothingThere = errors.New("nothing there")

// underRecoveries checks that p is <root>/<server>/<name>, exactly two levels
// below root, with no symbolic link in the way, so that a recovery can never
// read, write or delete anything outside the recoveries directory. It returns
// errNothingThere when the parent does not exist (nothing to remove).
func underRecoveries(root, p string) error {
	outside := fmt.Errorf("refusing to touch %s: it is not inside the recoveries directory %s", p, root)
	root, p = filepath.Clean(root), filepath.Clean(p)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return outside
	}
	if len(strings.Split(rel, string(filepath.Separator))) != 2 {
		return outside
	}
	rr, err := filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) {
		return errNothingThere
	}
	if err != nil {
		return err
	}
	pp, err := filepath.EvalSymlinks(filepath.Dir(p))
	if errors.Is(err, fs.ErrNotExist) {
		return errNothingThere
	}
	if err != nil {
		return err
	}
	if filepath.Dir(pp) != rr {
		return outside
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return outside
	}
	return nil
}

// recoveryDest is where a restore of backupID of server is written.
func recoveryDest(root, server, backupID string, at time.Time) (string, error) {
	if !barman.ValidName(server) || !barman.BackupIDPattern.MatchString(backupID) {
		return "", fmt.Errorf("refusing to build a restore path for %q/%q", server, backupID)
	}
	dest := filepath.Join(root, server, backupID+"-"+at.Format("20060102T150405"))
	if err := underRecoveries(root, dest); err != nil && !errors.Is(err, errNothingThere) {
		return "", err
	}
	return dest, nil
}

// prepareRecoveryDir creates dest. When pgCellar runs as root and drives
// Barman as another user (sudo), that user must own what it will write into.
func prepareRecoveryDir(dest string, cfg *config.Config) error {
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return fmt.Errorf("cannot create %s: %w", dest, err)
	}
	if os.Geteuid() == 0 && cfg.Barman.User != "" {
		if u, err := user.Lookup(cfg.Barman.User); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			for _, d := range []string{dest, filepath.Dir(dest)} {
				_ = os.Chown(d, uid, gid)
			}
		}
	}
	return nil
}

// ---- target time ----

var timeLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05Z07",
	"2006-01-02T15:04:05Z07",
}

var localLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

// parseTargetTime reads a point in time. Without a zone it is the time zone of
// the server pgCellar runs on (what a datetime-local field means).
func parseTargetTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	for _, l := range localLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date and time: write it like 2026-10-20 14:30:00+02:00", s)
}

// barmanTime is the form Barman wants for --target-time.
func barmanTime(t time.Time) string { return t.Format("2006-01-02 15:04:05-07:00") }

// ---- start ----

const recoveryRunKey = "recover:"

// StartRecovery validates the request and starts the restore in the
// background. It returns the id of the run to follow.
func (a *App) StartRecovery(ctx context.Context, actor, server string, in RecoveryInput) (int64, error) {
	if err := a.knownServer(ctx, server); err != nil {
		return 0, err
	}
	in.BackupID = strings.TrimSpace(in.BackupID)
	in.Target = strings.ToLower(strings.TrimSpace(in.Target))
	if in.Target == "" {
		in.Target = TargetLatest
	}
	var problems []string
	if !barman.BackupIDPattern.MatchString(in.BackupID) {
		problems = append(problems, "Choose the backup to restore from.")
	}
	opts := barman.RecoverOptions{}
	targetTime := ""
	var when time.Time
	switch in.Target {
	case TargetLatest:
	case TargetBackupEnd:
		opts.TargetImmediate = true
	case TargetTime:
		t, err := parseTargetTime(in.TargetTime)
		if err != nil {
			problems = append(problems, "Point in time: "+err.Error()+".")
		} else {
			when = t
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
		var bk *BackupView
		for i := range st.Backups {
			if st.Backups[i].ID == in.BackupID {
				bk = &st.Backups[i]
			}
		}
		switch {
		case bk == nil:
			problems = append(problems, fmt.Sprintf("Backup %s is not in the list of %s.", in.BackupID, server))
		case bk.Status != "DONE":
			problems = append(problems, fmt.Sprintf("Backup %s is %s: only a complete backup (DONE) can be restored.", in.BackupID, bk.Status))
		case in.Target == TargetTime && !bk.EndTime.IsZero() && when.Before(bk.EndTime):
			problems = append(problems, fmt.Sprintf("The point in time %s is before the end of backup %s (%s). Choose the end of that backup, a later time, or an older backup.",
				barmanTime(when), in.BackupID, barmanTime(bk.EndTime.Local())))
		}
	}
	if len(problems) > 0 {
		return 0, &ProblemsError{Problems: problems}
	}

	key := recoveryRunKey + server
	a.runMu.Lock()
	if id, busy := a.running[key]; busy {
		a.runMu.Unlock()
		return id, &ConflictError{Msg: fmt.Sprintf("A restore of %s is already running (run #%d).", server, id)}
	}
	id, err := a.Store.StartRun(server, store.KindRecover, actor)
	if err != nil {
		a.runMu.Unlock()
		return 0, err
	}
	a.running[key] = id
	a.runMu.Unlock()
	_ = a.Store.AddAudit(actor, "recover.start", server+"/"+in.BackupID, fmt.Sprintf("run #%d target=%s %s archive=%t", id, in.Target, targetTime, in.Archive))

	cfg := a.Config()
	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		defer func() {
			a.runMu.Lock()
			delete(a.running, key)
			a.runMu.Unlock()
		}()
		a.runRecovery(id, actor, server, in, targetTime, opts, cfg)
	}()
	return id, nil
}

func (a *App) runRecovery(id int64, actor, server string, in RecoveryInput, targetTime string, opts barman.RecoverOptions, cfg *config.Config) {
	rctx, cancel := context.WithTimeout(barman.WithActor(context.Background(), actor), 12*time.Hour)
	defer cancel()
	cr := &captureRunner{inner: a.Runner}
	var extra []string // lines added to the run's error output

	finish := func(code int) {
		stderr := strings.TrimSpace(cr.stderr.String() + "\n" + strings.Join(extra, "\n"))
		if err := a.Store.FinishRun(id, code, tail(cr.stdout.Bytes()), tail([]byte(stderr))); err != nil {
			a.Logf("cannot record the end of run #%d: %v", id, err)
		}
	}

	root := cfg.Recoveries.Dir
	dest, err := recoveryDest(root, server, in.BackupID, time.Now())
	if err == nil {
		err = prepareRecoveryDir(dest, cfg)
	}
	if err != nil {
		extra = append(extra, err.Error())
		finish(1)
		return
	}
	if _, err := barman.NewEngine(cr).Recover(rctx, server, in.BackupID, dest, opts); err != nil {
		extra = append(extra, err.Error())
		_ = removeTree(root, dest) // do not leave a half restore behind
		finish(exitCodeOf(err))
		return
	}

	rec := store.Recovery{Server: server, BackupID: in.BackupID, Target: in.Target, TargetTime: targetTime,
		Path: dest, SizeBytes: dirSize(dest), RunID: id, CreatedAt: time.Now(), Actor: actor}
	code := 0
	if in.Archive {
		archive := dest + ".tar.gz"
		if n, aerr := writeTarGz(dest, archive); aerr != nil {
			_ = os.Remove(archive + ".partial")
			extra = append(extra, "The directory was restored but the .tar.gz could not be built: "+aerr.Error())
			code = 1
		} else {
			rec.ArchivePath, rec.ArchiveSize = archive, n
		}
	}
	if _, err := a.Store.AddRecovery(rec); err != nil {
		a.Logf("cannot record the recovery of run #%d: %v", id, err)
		extra = append(extra, "The restore is on disk at "+dest+" but could not be recorded: "+err.Error())
		code = 1
	}
	finish(code)
}

// removeTree deletes dest after checking it belongs to the recoveries
// directory.
func removeTree(root, dest string) error {
	if err := underRecoveries(root, dest); err != nil {
		if errors.Is(err, errNothingThere) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dest)
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// writeTarGz writes srcDir as <name>/... into a gzip-compressed tar at out,
// streaming (nothing is loaded in memory). The file appears under its final
// name only when complete. It returns the size of the archive.
func writeTarGz(srcDir, out string) (int64, error) {
	tmp := out + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	base := filepath.Base(srcDir)
	walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if !mode.IsRegular() && !mode.IsDir() && mode&os.ModeSymlink == 0 {
			return nil // sockets, devices: nothing to keep
		}
		link := ""
		if mode&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(srcDir, p)
		name := base
		if rel != "." {
			name = base + "/" + filepath.ToSlash(rel)
		}
		if mode.IsDir() {
			name += "/"
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !mode.IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, cerr := io.Copy(tw, in)
		_ = in.Close()
		return cerr
	})
	err = walkErr
	for _, closeFn := range []func() error{tw.Close, gz.Close, f.Sync, f.Close} {
		if cerr := closeFn(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	fi, err := os.Stat(out)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// ---- read, download, delete, purge ----

// ListRecoveries lists the restores of a server (all when empty), newest first.
func (a *App) ListRecoveries(server string) ([]RecoveryView, error) {
	list, err := a.Store.ListRecoveries(server)
	if err != nil {
		return nil, err
	}
	max := a.Config().Recoveries.MaxDownloadBytes
	out := make([]RecoveryView, 0, len(list))
	for _, r := range list {
		out = append(out, a.recoveryView(r, max))
	}
	return out, nil
}

// GetRecovery returns one restore, or ErrNotFound.
func (a *App) GetRecovery(id int64) (*RecoveryView, error) {
	r, err := a.Store.GetRecovery(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := a.recoveryView(*r, a.Config().Recoveries.MaxDownloadBytes)
	return &v, nil
}

// RecoveryOfRun returns the restore a run produced, if it produced one.
func (a *App) RecoveryOfRun(runID int64) (*RecoveryView, bool) {
	r, err := a.Store.RecoveryForRun(runID)
	if err != nil {
		return nil, false
	}
	v := a.recoveryView(*r, a.Config().Recoveries.MaxDownloadBytes)
	return &v, true
}

// RecoveryArchive returns the path of the archive of a restore, for
// download. ErrNotFound when there is none, ErrTooLarge when it exceeds
// recoveries.max_download_bytes. The download is audited.
func (a *App) RecoveryArchive(actor string, id int64) (string, *RecoveryView, error) {
	v, err := a.GetRecovery(id)
	if err != nil {
		return "", nil, err
	}
	if v.ArchivePath == "" {
		return "", nil, ErrNotFound
	}
	root := a.Config().Recoveries.Dir
	if err := underRecoveries(root, v.ArchivePath); err != nil {
		if errors.Is(err, errNothingThere) {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}
	if fi, err := os.Stat(v.ArchivePath); err != nil || !fi.Mode().IsRegular() {
		return "", nil, ErrNotFound
	}
	if max := a.Config().Recoveries.MaxDownloadBytes; max > 0 && v.ArchiveBytes > max {
		return "", v, ErrTooLarge
	}
	_ = a.Store.AddAudit(actor, "recovery.download", fmt.Sprintf("%s/%s", v.Server, filepath.Base(v.ArchivePath)), fmt.Sprintf("recovery #%d", id))
	return v.ArchivePath, v, nil
}

// removeRecoveryFiles deletes the directory and the archive of a restore,
// after checking both are inside the recoveries directory.
func removeRecoveryFiles(root string, r store.Recovery) error {
	paths := []string{r.Path}
	if r.ArchivePath != "" {
		if r.ArchivePath != r.Path+".tar.gz" {
			return fmt.Errorf("refusing to touch %s: it is not the archive of %s", r.ArchivePath, r.Path)
		}
		paths = append(paths, r.ArchivePath)
	}
	for _, p := range paths {
		if err := underRecoveries(root, p); err != nil && !errors.Is(err, errNothingThere) {
			return err
		}
	}
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRecovery removes the directory, the archive and the record of a
// restore. Audited.
func (a *App) DeleteRecovery(actor string, id int64) error {
	r, err := a.Store.GetRecovery(id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := removeRecoveryFiles(a.Config().Recoveries.Dir, *r); err != nil {
		return err
	}
	if err := a.Store.DeleteRecovery(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_ = a.Store.AddAudit(actor, "recovery.delete", r.Server+"/"+filepath.Base(r.Path), fmt.Sprintf("recovery #%d", id))
	return nil
}

// PurgeRecoveries deletes the restores older than recoveries.keep_days (0
// keeps everything) and returns how many were removed. The background loop
// calls it once a day.
func (a *App) PurgeRecoveries() int {
	a.purgeOrphanDrillCopies()
	cfg := a.Config().Recoveries
	if cfg.EffectiveKeepDays() <= 0 {
		return 0
	}
	old, err := a.Store.RecoveriesOlderThan(time.Now().AddDate(0, 0, -cfg.EffectiveKeepDays()))
	if err != nil {
		a.Logf("recoveries purge failed: %v", err)
		return 0
	}
	n := 0
	for _, r := range old {
		if err := removeRecoveryFiles(cfg.Dir, r); err != nil {
			a.Logf("recoveries purge: %v", err)
			continue
		}
		if err := a.Store.DeleteRecovery(r.ID); err != nil {
			a.Logf("recoveries purge: %v", err)
			continue
		}
		_ = a.Store.AddAudit(systemActor, "recovery.purge", r.Server+"/"+filepath.Base(r.Path), fmt.Sprintf("recovery #%d older than %d days", r.ID, cfg.EffectiveKeepDays()))
		n++
	}
	if n > 0 {
		a.Logf("recoveries purge: removed %d restore(s) older than %d days", n, cfg.EffectiveKeepDays())
	}
	return n
}

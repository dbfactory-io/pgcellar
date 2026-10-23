package app

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/barman"
)

// Storage defaults.
const (
	// DefaultMinFreePercent is alerts.min_free_percent when it is not set.
	DefaultMinFreePercent = 10
	// projectionWindow is how far back the growth rate looks.
	projectionWindow = 7 * 24 * time.Hour
	// projectionHorizon: the "full in N days" sentence is shown below this.
	projectionHorizon = 90
)

// FSUsage is one file system, as `df` sees it.
type FSUsage struct {
	// Path is what is shown: <barman_home>, or <barman_home>/<server> when
	// that directory is a mount of its own. The mount point itself is not
	// known to pgCellar.
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"` // available to a normal user (Bavail)
	UsedBytes  int64  `json:"used_bytes"` // Blocks - Bfree, like df
	// UsedPercent is used / (used + free), like df.
	UsedPercent float64 `json:"used_percent"`
	// Level is ok, warn or fail against alerts.min_free_percent / min_free_bytes.
	Level string `json:"level"`
}

// FreePercent is the share of the file system that is still available.
func (f FSUsage) FreePercent() float64 {
	if f.UsedBytes+f.FreeBytes <= 0 {
		return 0
	}
	return 100 * float64(f.FreeBytes) / float64(f.UsedBytes+f.FreeBytes)
}

// UsedPct is UsedPercent rounded, for display.
func (f FSUsage) UsedPct() int { return int(math.Round(f.UsedPercent)) }

// FreeText, TotalText: sizes for people.
func (f FSUsage) FreeText() string  { return FormatSize(f.FreeBytes) }
func (f FSUsage) TotalText() string { return FormatSize(f.TotalBytes) }

// StorageInfo is what a server takes on disk, and what is left. It is
// computed when the status is refreshed, never when a page is shown.
type StorageInfo struct {
	// BackupBytes: sum of "size" of the DONE backups (backup.info), what the
	// data directories add up to. BackupDiskBytes: sum of "deduplicated_size",
	// what they take on disk when Barman shares files between backups (rsync
	// with reuse_backup); equal to BackupBytes when Barman does not say.
	BackupBytes     int64 `json:"backup_bytes"`
	BackupDiskBytes int64 `json:"backup_disk_bytes"`
	Backups         int   `json:"backups"`
	// WALBytes: sum of the sizes in the WAL catalogue (xlog.db), as archived
	// (compressed when Barman compresses).
	WALBytes int64 `json:"wal_bytes"`
	WALFiles int   `json:"wal_files"`
	// RecoveriesBytes: restores of this server under recoveries.dir that
	// pgCellar still lists (directories and archives).
	RecoveriesBytes int64 `json:"recoveries_bytes"`
	// TotalBytes = BackupDiskBytes + WALBytes + RecoveriesBytes.
	TotalBytes int64 `json:"total_bytes"`
	// AddedPerDay: (WAL archived + backups finished in the last 7 days) / the
	// number of days that window really covers (1 to 7). Backups count for
	// their on-disk size.
	AddedPerDay int64 `json:"added_per_day"`
	// DaysUntilFull: free space / AddedPerDay, only when under 90 days.
	DaysUntilFull *int `json:"days_until_full,omitempty"`
	// FS is the file system of <barman_home>/<server>; RecoveriesFS the one
	// of recoveries.dir, only when it is another file system.
	FS           *FSUsage `json:"fs,omitempty"`
	RecoveriesFS *FSUsage `json:"recoveries_fs,omitempty"`
	// Error says what could not be read (the other numbers are still shown).
	Error string `json:"error,omitempty"`
}

// StorageBar gives the two segments of the bar, in percent of the file system.
type StorageBar struct{ Other, Mine float64 }

func bar(f *FSUsage, mine int64) StorageBar {
	if f == nil || f.TotalBytes <= 0 {
		return StorageBar{}
	}
	m := 100 * float64(mine) / float64(f.TotalBytes)
	if mine > 0 && m < 0.5 {
		m = 0.5 // visible
	}
	if m > f.UsedPercent {
		m = f.UsedPercent
	}
	o := f.UsedPercent - m
	if o < 0 {
		o = 0
	}
	return StorageBar{Other: math.Round(o*10) / 10, Mine: math.Round(m*10) / 10}
}

// Bar is the bar of the backup file system: what others use, what this server uses.
func (s *StorageInfo) Bar() StorageBar {
	mine := s.BackupDiskBytes + s.WALBytes
	if s.RecoveriesFS == nil {
		mine += s.RecoveriesBytes
	}
	return bar(s.FS, mine)
}

// RecBar is the bar of the recoveries file system.
func (s *StorageInfo) RecBar() StorageBar { return bar(s.RecoveriesFS, s.RecoveriesBytes) }

// Texts for the templates.
func (s *StorageInfo) BackupsText() string { return FormatSize(s.BackupBytes) }
func (s *StorageInfo) DiskText() string    { return FormatSize(s.BackupDiskBytes) }
func (s *StorageInfo) WALText() string     { return FormatSize(s.WALBytes) }
func (s *StorageInfo) RestoresText() string {
	return FormatSize(s.RecoveriesBytes)
}
func (s *StorageInfo) PerDayText() string { return FormatSize(s.AddedPerDay) }

// Days is DaysUntilFull as a number, for the templates.
func (s *StorageInfo) Days() int {
	if s.DaysUntilFull == nil {
		return 0
	}
	return *s.DaysUntilFull
}

// Dedup reports whether the backups take less on disk than their size.
func (s *StorageInfo) Dedup() bool { return s.BackupDiskBytes > 0 && s.BackupDiskBytes < s.BackupBytes }

// FormatSize renders a size for people: "113 KiB", "12.6 MiB", "0 B".
func FormatSize(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// fsStat is syscall.Statfs; tests replace it.
var fsStat = statFS

// stat measures a path: the demo's figures when FSStat is set, statfs
// otherwise.
func (a *App) stat(path string) (fsRaw, error) {
	if a.FSStat != nil {
		total, free, used, dev, err := a.FSStat(path)
		return fsRaw{total: total, free: free, used: used, dev: dev}, err
	}
	return fsStat(path)
}

type fsRaw struct {
	total, free, used int64
	dev               uint64
}

func statFS(path string) (fsRaw, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsRaw{}, err
	}
	bs := int64(st.Bsize)
	fi, err := os.Stat(path)
	if err != nil {
		return fsRaw{}, err
	}
	var dev uint64
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		dev = uint64(sys.Dev) //nolint:unconvert // int32 on macOS, uint64 on Linux
	}
	return fsRaw{
		total: int64(st.Blocks) * bs,
		free:  int64(st.Bavail) * bs,
		used:  (int64(st.Blocks) - int64(st.Bfree)) * bs,
		dev:   dev,
	}, nil
}

func usageOf(path string, r fsRaw) *FSUsage {
	f := &FSUsage{Path: path, TotalBytes: r.total, FreeBytes: r.free, UsedBytes: r.used}
	if d := r.used + r.free; d > 0 {
		f.UsedPercent = 100 * float64(r.used) / float64(d)
	}
	return f
}

// fsLevel compares the free space with the two thresholds: under the
// threshold is orange, under half of it is red. A threshold of 0 is ignored.
func fsLevel(f FSUsage, minPercent float64, minBytes int64) string {
	level := LevelOK
	bump := func(l string) {
		if l == LevelFail || level == LevelOK {
			level = l
		}
	}
	if minPercent > 0 && f.TotalBytes > 0 {
		p := f.FreePercent()
		switch {
		case p < minPercent/2:
			bump(LevelFail)
		case p < minPercent:
			bump(LevelWarn)
		}
	}
	if minBytes > 0 {
		switch {
		case f.FreeBytes < minBytes/2:
			bump(LevelFail)
		case f.FreeBytes < minBytes:
			bump(LevelWarn)
		}
	}
	return level
}

// growthPerDay estimates the bytes added per day from the last 7 days of WAL
// and backups. The window is shortened to the age of the oldest data, so that
// a server that is two days old is not divided by seven.
func growthPerDay(now time.Time, infos []barman.BackupInfo, wals []barman.WALFile) int64 {
	from := now.Add(-projectionWindow)
	var added int64
	oldest := now
	seen := func(t time.Time) {
		if !t.IsZero() && t.Before(oldest) {
			oldest = t
		}
	}
	for _, w := range wals {
		seen(w.Time)
		if !w.Time.Before(from) && !w.Time.After(now) {
			added += w.Size
		}
	}
	for _, b := range infos {
		if b.Status != "DONE" {
			continue
		}
		t := b.EndTime
		if t.IsZero() {
			t = b.BeginTime
		}
		seen(t)
		if !t.Before(from) && !t.After(now) {
			added += onDisk(b)
		}
	}
	span := now.Sub(oldest)
	if span < 24*time.Hour {
		span = 24 * time.Hour
	}
	if span > projectionWindow {
		span = projectionWindow
	}
	return int64(float64(added) / (float64(span) / float64(24*time.Hour)))
}

func onDisk(b barman.BackupInfo) int64 {
	if b.DeduplicatedSize > 0 {
		return b.DeduplicatedSize
	}
	return b.Size
}

// daysUntilFull returns the whole days left at this pace, only under 90.
func daysUntilFull(free, perDay int64) (int, bool) {
	if perDay <= 0 || free < 0 {
		return 0, false
	}
	d := int(free / perDay)
	return d, d < projectionHorizon
}

// measureStorage reads the numbers of one server: three small files of
// Barman and one statfs. It never runs a command.
func (a *App) measureStorage(name string) *StorageInfo {
	home := a.barmanHome()
	s := &StorageInfo{}
	var infos []barman.BackupInfo
	var wals []barman.WALFile
	var problems []string

	if list, _, err := barman.ReadBackupInfos(home, name); err != nil {
		problems = append(problems, fmt.Sprintf("backup.info: %v", err))
	} else {
		infos = list
		for _, b := range infos {
			if b.Status != "DONE" {
				continue
			}
			s.Backups++
			s.BackupBytes += b.Size
			s.BackupDiskBytes += onDisk(b)
		}
	}
	if list, err := barman.ReadXlogDB(home, name); err != nil {
		problems = append(problems, fmt.Sprintf("xlog.db: %v", err))
	} else {
		wals = list
		s.WALFiles = len(wals)
		for _, w := range wals {
			s.WALBytes += w.Size
		}
	}
	if a.Store != nil {
		if recs, err := a.Store.ListRecoveries(name); err == nil {
			for _, r := range recs {
				s.RecoveriesBytes += r.SizeBytes + r.ArchiveSize
			}
		}
	}
	s.TotalBytes = s.BackupDiskBytes + s.WALBytes + s.RecoveriesBytes
	s.AddedPerDay = growthPerDay(a.now(), infos, wals)

	cfg := a.Config()
	main, dev := a.statDir(home, name)
	if main != nil {
		s.FS = main
	} else {
		problems = append(problems, "the file system of "+filepath.Join(home, name)+" cannot be read")
	}
	if rd := cfg.Recoveries.Dir; rd != "" {
		if r, err := a.stat(rd); err == nil {
			if main == nil || r.dev != dev {
				s.RecoveriesFS = usageOf(rd, r)
			}
		}
	}
	if s.FS != nil {
		if d, ok := daysUntilFull(s.FS.FreeBytes, s.AddedPerDay); ok {
			s.DaysUntilFull = &d
		}
	}
	minPct, minBytes := cfg.Alerts.MinFreePercent, cfg.Alerts.MinFreeBytes
	for _, f := range []*FSUsage{s.FS, s.RecoveriesFS} {
		if f != nil {
			f.Level = fsLevel(*f, minPct, minBytes)
		}
	}
	if len(problems) > 0 {
		s.Error = "Could not read: " + joinList(problems) + "."
	}
	return s
}

// statDir measures <home>/<name>, or <home> when the server has no directory
// yet. The path shown is the home, unless the server directory is a mount of
// its own.
func (a *App) statDir(home, name string) (*FSUsage, uint64) {
	dir := filepath.Join(home, name)
	r, err := a.stat(dir)
	if err != nil {
		r2, err2 := a.stat(home)
		if err2 != nil {
			return nil, 0
		}
		return usageOf(home, r2), r2.dev
	}
	shown := home
	if h, err := a.stat(home); err != nil || h.dev != r.dev {
		shown = dir
	}
	if a.DisplayHome != "" {
		shown = a.DisplayHome + strings.TrimPrefix(shown, home)
	}
	return usageOf(shown, r), r.dev
}

func joinList(l []string) string {
	out := ""
	for i, s := range l {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// applyStorage measures the server and, when a file system is under the
// thresholds, adds the problem and raises the level (orange, or red below half
// the threshold). It returns the new level.
func (a *App) applyStorage(st *ServerState, level string) string {
	st.Storage = a.measureStorage(st.Name)
	for _, f := range []struct {
		fs   *FSUsage
		what string
	}{{st.Storage.FS, "backups"}, {st.Storage.RecoveriesFS, "restores"}} {
		if f.fs == nil || f.fs.Level == LevelOK {
			continue
		}
		st.Problems = append(st.Problems, fmt.Sprintf("%d %% free on %s (%s): %s will start failing.", int(f.fs.FreePercent()), f.fs.Path, FormatSize(f.fs.FreeBytes), f.what))
		if f.fs.Level == LevelFail {
			level = LevelFail
		} else if level == LevelOK {
			level = LevelWarn
		}
	}
	return level
}

// FormatDecimalSize renders a size in powers of 1000, the way storage is
// sold: "500 GB", "1.2 TB", "8.5 kB".
func FormatDecimalSize(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), -1
	for v >= 1000 && exp < 5 {
		v /= 1000
		exp++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + []string{"kB", "MB", "GB", "TB", "PB", "EB"}[exp]
}

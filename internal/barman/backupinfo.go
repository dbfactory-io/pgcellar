package barman

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BackupInfo is what Barman writes about one backup in its own metadata file:
// <barman_home>/<server>/meta/<id>-backup.info on Barman 3.x, or
// <barman_home>/<server>/base/<id>/backup.info on older versions. The file is
// "key=value" lines. Only the keys pgCellar draws are kept.
type BackupInfo struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"` // DONE, FAILED, WAITING_FOR_WALS, STARTED, ...
	BeginTime        time.Time `json:"begin_time,omitzero"`
	EndTime          time.Time `json:"end_time,omitzero"`
	BeginWAL         string    `json:"begin_wal,omitempty"`
	EndWAL           string    `json:"end_wal,omitempty"`
	Timeline         uint32    `json:"timeline,omitempty"`
	Size             int64     `json:"size"`              // bytes, 0 when Barman wrote None
	DeduplicatedSize int64     `json:"deduplicated_size"` // bytes
	ParentID         string    `json:"parent_backup_id,omitempty"`
	Version          int       `json:"version,omitempty"` // PostgreSQL server_version_num
	SystemID         string    `json:"systemid,omitempty"`
	SegmentSize      int64     `json:"xlog_segment_size,omitempty"` // bytes, 16 MiB when absent
}

// Incremental reports whether the backup depends on a parent backup.
func (b BackupInfo) Incremental() bool { return b.ParentID != "" }

func parseInfoInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func noneEmpty(s string) string {
	s = strings.TrimSpace(s)
	if s == "None" {
		return ""
	}
	return s
}

// ParseBackupInfo reads one backup.info file. Unknown keys are ignored; a
// file without a begin time or a status is an error.
func ParseBackupInfo(id string, r io.Reader) (BackupInfo, error) {
	b := BackupInfo{ID: id}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // backup_label is one long line
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "status":
			b.Status = noneEmpty(v)
		case "begin_time":
			b.BeginTime = parseTime(v)
		case "end_time":
			b.EndTime = parseTime(v)
		case "begin_wal":
			b.BeginWAL = noneEmpty(v)
		case "end_wal":
			b.EndWAL = noneEmpty(v)
		case "timeline":
			b.Timeline = uint32(parseInfoInt(v))
		case "size":
			b.Size = parseInfoInt(v)
		case "deduplicated_size":
			b.DeduplicatedSize = parseInfoInt(v)
		case "parent_backup_id":
			b.ParentID = noneEmpty(v)
		case "version":
			b.Version = int(parseInfoInt(v))
		case "systemid":
			b.SystemID = noneEmpty(v)
		case "xlog_segment_size":
			b.SegmentSize = parseInfoInt(v)
		}
	}
	if err := sc.Err(); err != nil {
		return BackupInfo{}, err
	}
	if b.Status == "" || b.BeginTime.IsZero() {
		return BackupInfo{}, fmt.Errorf("backup %s: no status or begin_time in its backup.info", id)
	}
	return b, nil
}

// ReadBackupInfos reads the metadata of every backup of a server, oldest
// first (by begin time). It looks in meta/ (Barman 3.x) and, for ids not found
// there, in base/<id>/backup.info (older Barman). The files are only read.
// A backup whose file cannot be parsed is skipped and its problem is returned
// in skipped; an error is returned only when neither directory can be read.
func ReadBackupInfos(home, server string) (infos []BackupInfo, skipped []string, err error) {
	seen := map[string]bool{}
	metaDir := filepath.Join(home, server, "meta")
	baseDir := filepath.Join(home, server, "base")
	metaEntries, metaErr := os.ReadDir(metaDir)
	baseEntries, baseErr := os.ReadDir(baseDir)
	if metaErr != nil && baseErr != nil {
		return nil, nil, unwrapPathError(metaErr)
	}
	load := func(id, path string) {
		if seen[id] {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", id, unwrapPathError(err)))
			return
		}
		defer func() { _ = f.Close() }()
		b, err := ParseBackupInfo(id, f)
		if err != nil {
			skipped = append(skipped, err.Error())
			return
		}
		seen[id] = true
		infos = append(infos, b)
	}
	for _, e := range metaEntries {
		if id, ok := strings.CutSuffix(e.Name(), "-backup.info"); ok && !e.IsDir() {
			load(id, filepath.Join(metaDir, e.Name()))
		}
	}
	for _, e := range baseEntries {
		if e.IsDir() {
			p := filepath.Join(baseDir, e.Name(), "backup.info")
			if _, err := os.Stat(p); err == nil {
				load(e.Name(), p)
			}
		}
	}
	sort.SliceStable(infos, func(i, j int) bool {
		if !infos[i].BeginTime.Equal(infos[j].BeginTime) {
			return infos[i].BeginTime.Before(infos[j].BeginTime)
		}
		return infos[i].ID < infos[j].ID
	})
	return infos, skipped, nil
}

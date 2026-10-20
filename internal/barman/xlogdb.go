package barman

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBarmanHome is Barman's own default for barman_home.
const DefaultBarmanHome = "/var/lib/barman"

// XlogDBPath returns the WAL catalogue file of a server. Barman 3.20 names it
// <server>-xlog.db; older versions used xlog.db. The first one that exists is
// returned; when neither does, the 3.20 name (for the error message).
func XlogDBPath(home, server string) string {
	dir := filepath.Join(home, server, "wals")
	for _, name := range []string{server + "-xlog.db", "xlog.db"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dir, server+"-xlog.db")
}

// ReadLastWAL reads the last non-empty line of the server's WAL catalogue:
// "<wal name>\t<size>\t<epoch>\t<compression>\t<encryption>", one line per
// archived WAL, appended in archive order, so the last line is the last WAL
// archived. An empty file (nothing archived yet) gives an empty name and a
// zero time without error. The file is only read, never written, and only its
// last 8 KiB are looked at. path is returned even on error.
func ReadLastWAL(home, server string) (path, name string, when time.Time, err error) {
	path = XlogDBPath(home, server)
	f, err := os.Open(path)
	if err != nil {
		return path, "", time.Time{}, unwrapPathError(err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return path, "", time.Time{}, err
	}
	const tail = 8192
	if fi.Size() > tail {
		if _, err := f.Seek(-tail, io.SeekEnd); err != nil {
			return path, "", time.Time{}, err
		}
	}
	last := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	if err := sc.Err(); err != nil {
		return path, "", time.Time{}, err
	}
	if last == "" {
		return path, "", time.Time{}, nil
	}
	name, when, err = ParseXlogDBLine(last)
	return path, name, when, err
}

// ParseXlogDBLine reads one catalogue line (tab or space separated).
func ParseXlogDBLine(line string) (string, time.Time, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", time.Time{}, fmt.Errorf("unexpected line in the WAL catalogue: %q", line)
	}
	t := parseTime(fields[2])
	if t.IsZero() {
		return "", time.Time{}, fmt.Errorf("no archive time in the WAL catalogue line: %q", line)
	}
	return fields[0], t, nil
}

func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// ReadLastReceived returns the modification time of the newest regular file in
// <home>/<server>/streaming, where the WAL receiver writes the segment in
// progress (name.partial). It is the time data last reached Barman, which a
// quiet database moves far more often than it closes a segment. Only file
// metadata is read. The zero time means the directory is absent or unreadable
// or empty.
func ReadLastReceived(home, server string) time.Time {
	entries, err := os.ReadDir(filepath.Join(home, server, "streaming"))
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// DefaultSegmentSize is the WAL segment size of a default PostgreSQL build.
const DefaultSegmentSize int64 = 16 << 20

// WALFile is one line of the WAL catalogue that names a WAL segment.
type WALFile struct {
	Name        string    `json:"name"`
	Timeline    uint32    `json:"timeline"`
	Log         uint32    `json:"log"`
	Seg         uint32    `json:"seg"`
	Size        int64     `json:"size"` // bytes as stored (compressed when Compression is set)
	Time        time.Time `json:"time"` // when Barman archived the file
	Compression string    `json:"compression,omitempty"`
}

// Index is the position of the segment on its timeline: log * segments per
// log + seg. segSize is xlog_segment_size in bytes (0 means 16 MiB).
func (w WALFile) Index(segSize int64) uint64 { return SegmentIndex(w.Log, w.Seg, segSize) }

// SegmentsPerLog is how many segments make up one "log" number (the high 32
// bits of the LSN): 256 for 16 MiB segments.
func SegmentsPerLog(segSize int64) uint64 {
	if segSize <= 0 {
		segSize = DefaultSegmentSize
	}
	return uint64(1<<32) / uint64(segSize)
}

// SegmentIndex turns (log, seg) into a number that grows by one from a
// segment to the next one.
func SegmentIndex(log, seg uint32, segSize int64) uint64 {
	return uint64(log)*SegmentsPerLog(segSize) + uint64(seg)
}

// ParseWALName splits a 24 hex digit segment name into timeline, log and seg.
// ok is false for anything else (.history, .backup, .partial files).
func ParseWALName(name string) (tli, log, seg uint32, ok bool) {
	if len(name) != 24 {
		return 0, 0, 0, false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') && (c < 'a' || c > 'f') {
			return 0, 0, 0, false
		}
	}
	a, _ := strconv.ParseUint(name[0:8], 16, 32)
	b, _ := strconv.ParseUint(name[8:16], 16, 32)
	c, _ := strconv.ParseUint(name[16:24], 16, 32)
	return uint32(a), uint32(b), uint32(c), true
}

// ReadXlogDB reads the whole WAL catalogue of a server: every line that names
// a WAL segment, sorted by timeline then segment position. Lines for other
// files (.history, .backup) and blank lines are skipped; a line that cannot be
// read is an error. A missing file is an error, an empty one is not.
func ReadXlogDB(home, server string) ([]WALFile, error) {
	f, err := os.Open(XlogDBPath(home, server))
	if err != nil {
		return nil, unwrapPathError(err)
	}
	defer func() { _ = f.Close() }()
	var out []WALFile
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("unexpected line in the WAL catalogue: %q", line)
		}
		tli, lg, sg, ok := ParseWALName(fields[0])
		if !ok {
			continue
		}
		t := parseTime(fields[2])
		if t.IsZero() {
			return nil, fmt.Errorf("no archive time in the WAL catalogue line: %q", line)
		}
		size, _ := strconv.ParseInt(fields[1], 10, 64)
		w := WALFile{Name: fields[0], Timeline: tli, Log: lg, Seg: sg, Size: size, Time: t}
		if len(fields) > 3 && fields[3] != "None" {
			w.Compression = fields[3]
		}
		out = append(out, w)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Timeline != b.Timeline {
			return a.Timeline < b.Timeline
		}
		if a.Log != b.Log {
			return a.Log < b.Log
		}
		return a.Seg < b.Seg
	})
	return out, nil
}

// WALGap is a run of missing segments on one timeline, between two segments
// that are in the catalogue. After is the segment that came after the hole,
// which is the one whose arrival time places the gap.
type WALGap struct {
	Timeline uint32  `json:"timeline"`
	First    uint64  `json:"first"`   // index of the first missing segment
	Missing  uint64  `json:"missing"` // how many segments are missing
	Before   WALFile `json:"-"`       // the last segment before the hole
	After    WALFile `json:"-"`
}

// FindWALGaps lists the holes of each timeline between its first and last
// segment. files must be sorted as ReadXlogDB returns them.
func FindWALGaps(files []WALFile, segSize int64) []WALGap {
	var gaps []WALGap
	for i := 1; i < len(files); i++ {
		p, c := files[i-1], files[i]
		if p.Timeline != c.Timeline {
			continue
		}
		pi, ci := p.Index(segSize), c.Index(segSize)
		if ci > pi+1 {
			gaps = append(gaps, WALGap{Timeline: c.Timeline, First: pi + 1, Missing: ci - pi - 1, Before: p, After: c})
		}
	}
	return gaps
}

// History is one <timeline>.history file: the timeline was created by a
// recovery or a promotion that forked from Parent at Switch (an LSN such as
// "0/5000060"). Time is the modification time of the file in wals/, which is
// when Barman archived it, a few seconds after the fork.
type History struct {
	Timeline uint32    `json:"timeline"`
	Parent   uint32    `json:"parent"`
	Switch   string    `json:"switch"`
	Reason   string    `json:"reason,omitempty"`
	Time     time.Time `json:"time,omitzero"`
}

// SwitchIndex is the position of the segment holding the switch point.
func (h History) SwitchIndex(segSize int64) (uint64, bool) {
	hi, lo, ok := strings.Cut(h.Switch, "/")
	if !ok {
		return 0, false
	}
	a, err1 := strconv.ParseUint(hi, 16, 32)
	b, err2 := strconv.ParseUint(lo, 16, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	if segSize <= 0 {
		segSize = DefaultSegmentSize
	}
	return a*SegmentsPerLog(segSize) + b/uint64(segSize), true
}

// ReadHistories reads the <timeline>.history files of <home>/<server>/wals,
// sorted by timeline. A file that cannot be read or understood is skipped.
func ReadHistories(home, server string) []History {
	dir := filepath.Join(home, server, "wals")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []History
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".history")
		if !ok || len(base) != 8 || e.IsDir() {
			continue
		}
		n, err := strconv.ParseUint(base, 16, 32)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		h := History{Timeline: uint32(n)}
		// The last line is the fork that created this timeline.
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			p, err := strconv.ParseUint(f[0], 10, 32)
			if err != nil {
				continue
			}
			h.Parent, h.Switch, h.Reason = uint32(p), f[1], strings.Join(f[2:], " ")
		}
		if h.Switch == "" {
			continue
		}
		if info, err := e.Info(); err == nil {
			h.Time = info.ModTime()
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timeline < out[j].Timeline })
	return out
}

package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
)

// Timeline is the picture of what a server can be restored to, built from
// Barman's own files (<barman_home>/<server>/meta/*-backup.info, wals/*-xlog.db
// and wals/*.history). It is computed on demand, takes a few milliseconds and
// never runs Barman.
type Timeline struct {
	Server      string    `json:"server"`
	Available   bool      `json:"available"`
	Unavailable string    `json:"unavailable,omitempty"` // "unavailable: <reason>" when Available is false
	Now         time.Time `json:"now"`
	AxisStart   time.Time `json:"axis_start,omitzero"`
	AxisEnd     time.Time `json:"axis_end,omitzero"`

	Retention RetentionWindow `json:"retention"`
	Backups   []TLBackup      `json:"backups"`   // oldest first, every status
	Intervals []TLInterval    `json:"intervals"` // WAL between consecutive DONE backups
	Branches  []WALTimeline   `json:"timelines"` // PostgreSQL timelines seen in the WAL
	Gaps      []TLGap         `json:"wal_gaps"`  // holes in the WAL chain of a timeline

	// Earliest is the end time of the oldest DONE backup whose WAL chain is
	// unbroken from its begin_wal to the last filled WAL file; zero when there
	// is none. Latest is the last byte received when streaming is live, else
	// the arrival time of the last filled WAL file.
	Earliest       time.Time `json:"earliest_restore_point,omitzero"`
	EarliestBackup string    `json:"earliest_backup,omitempty"`
	Latest         time.Time `json:"latest_restore_point,omitzero"`
	LatestSource   string    `json:"latest_source,omitempty"` // "streaming" | "last_filled_wal"
	LastFilledWAL  string    `json:"last_filled_wal,omitempty"`
	LastFilledTime time.Time `json:"last_filled_wal_time,omitzero"`
	LastReceived   time.Time `json:"last_data_received,omitzero"` // only while streaming is live
	NextBackup     time.Time `json:"next_backup,omitzero"`

	Totals TLTotals `json:"totals"`
	Notes  []string `json:"notes"`
}

// RetentionWindow is the retention policy of the server. Start is now minus
// the recovery window, zero when no window is set.
type RetentionWindow struct {
	Days              int       `json:"recovery_window_days"`
	Redundancy        int       `json:"redundancy"`
	MinimumRedundancy int       `json:"minimum_redundancy"`
	Start             time.Time `json:"start,omitzero"`
}

// TLBackup is a backup block.
type TLBackup struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Begin            time.Time `json:"begin_time"`
	End              time.Time `json:"end_time,omitzero"`
	Size             int64     `json:"size"`
	DeduplicatedSize int64     `json:"deduplicated_size"`
	Timeline         uint32    `json:"timeline"`
	BeginWAL         string    `json:"begin_wal,omitempty"`
	EndWAL           string    `json:"end_wal,omitempty"`
	ParentID         string    `json:"parent_backup_id,omitempty"`
	// Restorable: DONE and the WAL chain is unbroken from its begin_wal to
	// the last filled WAL file.
	Restorable bool `json:"restorable"`
}

// TLInterval is the WAL that arrived between the begin of one DONE backup and
// the begin of the next (the last one runs to the newest data).
type TLInterval struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	FromBackup  string    `json:"from_backup"`
	ToBackup    string    `json:"to_backup,omitempty"` // empty for the last interval
	Files       int       `json:"wal_files"`
	Bytes       int64     `json:"wal_bytes"` // as stored in xlog.db
	Continuous  bool      `json:"continuous"`
	MissingWALs uint64    `json:"missing_wal_files"`
}

// TLGap is a hole in the WAL of one timeline: Missing segments lie between the
// arrival of the segment before them (From) and of the one after (To).
type TLGap struct {
	Timeline uint32    `json:"timeline"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Missing  uint64    `json:"missing_wal_files"`
}

// WALTimeline is one PostgreSQL timeline seen in the WAL catalogue.
type WALTimeline struct {
	ID         uint32    `json:"id"`
	Parent     uint32    `json:"parent,omitempty"`
	BranchedAt time.Time `json:"branched_at,omitzero"` // when the fork was archived
	SwitchLSN  string    `json:"switch_lsn,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	First      time.Time `json:"first_wal_time,omitzero"`
	Last       time.Time `json:"last_wal_time,omitzero"`
	Files      int       `json:"wal_files"`
	Bytes      int64     `json:"wal_bytes"`
	Current    bool      `json:"current"` // the timeline of the newest WAL file
}

// TLTotals sums up the picture.
type TLTotals struct {
	Backups       int   `json:"backups"` // DONE
	Full          int   `json:"full_backups"`
	Incremental   int   `json:"incremental_backups"`
	BackupBytes   int64 `json:"backup_bytes"`
	WALFiles      int   `json:"wal_files"`
	WALBytes      int64 `json:"wal_bytes"`
	Gaps          int   `json:"wal_gaps"`
	MissingWALs   int64 `json:"missing_wal_files"`
	FailedBackups int   `json:"failed_backups"`
}

// GetTimeline builds the timeline of a server from Barman's files. It never
// runs Barman; ErrNotFound for an unknown server.
func (a *App) GetTimeline(ctx context.Context, name string) (*Timeline, error) {
	d, err := a.GetServer(ctx, name)
	if err != nil {
		return nil, err
	}
	in := timelineInput{
		Server:   name,
		Home:     a.barmanHome(),
		Now:      a.now(),
		Schedule: d.Schedule,
		Ret:      d.Retention,
	}
	if st := d.State; st != nil {
		in.Streaming = st.StreamingLive
		in.LastReceived = st.LastDataReceived
	}
	return buildTimeline(in), nil
}

type timelineInput struct {
	Server       string
	Home         string
	Now          time.Time
	Schedule     config.Schedule
	Ret          config.Retention
	Streaming    bool
	LastReceived time.Time
}

type walKey struct {
	tli uint32
	idx uint64
}

func buildTimeline(in timelineInput) *Timeline {
	now := in.Now
	t := &Timeline{Server: in.Server, Now: now, Backups: []TLBackup{}, Intervals: []TLInterval{}, Gaps: []TLGap{}, Branches: []WALTimeline{}, Notes: []string{}}
	t.Retention = RetentionWindow{Days: in.Ret.RecoveryWindowDays, Redundancy: in.Ret.Redundancy, MinimumRedundancy: in.Ret.MinimumRedundancy}
	if t.Retention.Days > 0 {
		t.Retention.Start = now.AddDate(0, 0, -t.Retention.Days)
	}

	infos, skipped, err := barman.ReadBackupInfos(in.Home, in.Server)
	if err != nil {
		t.Unavailable = "unavailable: cannot read the backup metadata of " + in.Server + " (" + err.Error() + ")"
		return t
	}
	t.Available = true
	for _, s := range skipped {
		t.Notes = append(t.Notes, "A backup.info file could not be read: "+s)
	}

	files, ferr := barman.ReadXlogDB(in.Home, in.Server)
	if ferr != nil {
		t.Notes = append(t.Notes, "The WAL catalogue cannot be read ("+ferr.Error()+"): WAL coverage and restore points are unknown.")
		files = nil
	}
	hist := barman.ReadHistories(in.Home, in.Server)

	segSize := barman.DefaultSegmentSize
	for _, b := range infos {
		if b.SegmentSize > 0 {
			segSize = b.SegmentSize
			break
		}
	}

	// The newest WAL file (by arrival) is the head of the history.
	var head barman.WALFile
	for _, f := range files {
		if head.Name == "" || f.Time.After(head.Time) || (f.Time.Equal(head.Time) && f.Timeline >= head.Timeline && f.Index(segSize) > head.Index(segSize)) {
			head = f
		}
	}
	if head.Name != "" {
		t.LastFilledWAL, t.LastFilledTime = head.Name, head.Time
		t.Latest, t.LatestSource = head.Time, "last_filled_wal"
		if in.Streaming && in.LastReceived.After(head.Time) {
			t.LastReceived = in.LastReceived
			t.Latest, t.LatestSource = in.LastReceived, "streaming"
		}
	}

	set := make(map[walKey]bool, len(files))
	for _, f := range files {
		set[walKey{f.Timeline, f.Index(segSize)}] = true
	}
	ancestry := map[uint32]bool{}
	for tli, guard := head.Timeline, 0; head.Name != "" && guard < 1000; guard++ {
		ancestry[tli] = true
		parent := uint32(0)
		for _, h := range hist {
			if h.Timeline == tli {
				parent = h.Parent
			}
		}
		if parent == 0 {
			break
		}
		tli = parent
	}
	chainOK := func(b barman.BackupInfo) bool {
		if head.Name == "" {
			return false
		}
		tli, lg, sg, ok := barman.ParseWALName(b.BeginWAL)
		if !ok {
			return false
		}
		idx := barman.SegmentIndex(lg, sg, segSize)
		if !set[walKey{tli, idx}] {
			return false
		}
		headIdx := head.Index(segSize)
		for steps := 0; steps <= len(files)+len(hist)+2; steps++ {
			if tli == head.Timeline && idx >= headIdx {
				return true
			}
			moved := false
			for _, h := range hist {
				if h.Parent != tli || !ancestry[h.Timeline] {
					continue
				}
				if si, ok := h.SwitchIndex(segSize); ok && (si == idx || si == idx+1) && set[walKey{h.Timeline, si}] {
					tli, idx, moved = h.Timeline, si, true
					break
				}
			}
			if moved {
				continue
			}
			if set[walKey{tli, idx + 1}] {
				idx++
				continue
			}
			return false
		}
		return false
	}

	// Backups, totals, earliest restore point.
	var done []barman.BackupInfo
	for _, b := range infos {
		tb := TLBackup{ID: b.ID, Status: b.Status, Begin: b.BeginTime, End: b.EndTime, Size: b.Size, DeduplicatedSize: b.DeduplicatedSize,
			Timeline: b.Timeline, BeginWAL: b.BeginWAL, EndWAL: b.EndWAL, ParentID: b.ParentID}
		switch b.Status {
		case "DONE":
			done = append(done, b)
			tb.Restorable = chainOK(b)
			t.Totals.Backups++
			t.Totals.BackupBytes += b.Size
			if b.Incremental() {
				t.Totals.Incremental++
			} else {
				t.Totals.Full++
			}
			if tb.Restorable && t.Earliest.IsZero() {
				t.Earliest, t.EarliestBackup = b.EndTime, b.ID
				if t.Earliest.IsZero() {
					t.Earliest = b.BeginTime
				}
			}
		case "FAILED":
			t.Totals.FailedBackups++
		}
		t.Backups = append(t.Backups, tb)
	}
	for _, f := range files {
		t.Totals.WALFiles++
		t.Totals.WALBytes += f.Size
	}

	// WAL between consecutive DONE backups, with the holes of each timeline.
	gaps := barman.FindWALGaps(files, segSize)
	for _, g := range gaps {
		t.Totals.Gaps++
		t.Totals.MissingWALs += int64(g.Missing)
		t.Gaps = append(t.Gaps, TLGap{Timeline: g.Timeline, From: g.Before.Time, To: g.After.Time, Missing: g.Missing})
	}
	end := t.Latest
	if end.IsZero() {
		end = now
	}
	for i, b := range done {
		iv := TLInterval{From: b.BeginTime, To: end, FromBackup: b.ID, Continuous: true}
		upper := time.Time{} // WAL files with From < time <= upper belong to the interval
		if i+1 < len(done) {
			iv.To, iv.ToBackup = done[i+1].BeginTime, done[i+1].ID
			upper = iv.To
		}
		for _, f := range files {
			if f.Time.After(iv.From) && (upper.IsZero() || !f.Time.After(upper)) {
				iv.Files++
				iv.Bytes += f.Size
			}
		}
		for _, g := range gaps {
			// The hole lies between the arrival of the segment before it and
			// of the one after it: every interval that overlaps that span is
			// marked.
			if g.Before.Time.Before(iv.To) && g.After.Time.After(iv.From) {
				iv.Continuous = false
				iv.MissingWALs += g.Missing
			}
		}
		t.Intervals = append(t.Intervals, iv)
	}

	// PostgreSQL timelines.
	byTLI := map[uint32]*WALTimeline{}
	for _, f := range files {
		w := byTLI[f.Timeline]
		if w == nil {
			w = &WALTimeline{ID: f.Timeline, First: f.Time, Last: f.Time}
			byTLI[f.Timeline] = w
		}
		w.Files++
		w.Bytes += f.Size
		if f.Time.Before(w.First) {
			w.First = f.Time
		}
		if f.Time.After(w.Last) {
			w.Last = f.Time
		}
	}
	for _, h := range hist {
		w := byTLI[h.Timeline]
		if w == nil {
			w = &WALTimeline{ID: h.Timeline}
			byTLI[h.Timeline] = w
		}
		w.Parent, w.SwitchLSN, w.Reason, w.BranchedAt = h.Parent, h.Switch, h.Reason, h.Time
		if w.BranchedAt.IsZero() {
			w.BranchedAt = w.First
		}
	}
	for _, w := range byTLI {
		w.Current = head.Name != "" && w.ID == head.Timeline
		t.Branches = append(t.Branches, *w)
	}
	sort.Slice(t.Branches, func(i, j int) bool { return t.Branches[i].ID < t.Branches[j].ID })

	// Next planned backup.
	if in.Schedule.Enabled && in.Schedule.Backup != "" {
		if cron, _, err := config.ParseSchedule(in.Schedule.Backup); err == nil {
			t.NextBackup, _ = nextCron(cron, now)
		}
	}

	t.axis()
	return t
}

// axis chooses the time range of the picture: from a little before the oldest
// backup or WAL file (at least two hours wide) to now, stretched to the next
// planned backup when it is close enough to be useful.
func (t *Timeline) axis() {
	start := t.Now.Add(-24 * time.Hour)
	oldest := time.Time{}
	note := func(ts time.Time) {
		if !ts.IsZero() && (oldest.IsZero() || ts.Before(oldest)) {
			oldest = ts
		}
	}
	for _, b := range t.Backups {
		note(b.Begin)
	}
	for _, w := range t.Branches {
		note(w.First)
	}
	if !oldest.IsZero() {
		start = oldest
	}
	end := t.Now
	if !t.Latest.IsZero() && t.Latest.After(end) {
		end = t.Latest
	}
	span := end.Sub(start)
	if span < 2*time.Hour {
		span = 2 * time.Hour
		start = end.Add(-span)
	}
	if !t.NextBackup.IsZero() && t.NextBackup.Sub(end) <= span/3 {
		end = t.NextBackup
		span = end.Sub(start)
	}
	t.AxisStart = start.Add(-span / 50)
	t.AxisEnd = end.Add(span / 50)
}

// nextCron returns the first minute after `after` that matches a 5-field cron
// expression (minute hour day-of-month month day-of-week; lists, ranges and
// steps; day-of-month and day-of-week are ORed when both are restricted, as
// cron does). The search stops after two years.
func nextCron(expr string, after time.Time) (time.Time, bool) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return time.Time{}, false
	}
	limits := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	var sets [5][]bool
	for i := range f {
		s, ok := cronSet(f[i], limits[i][0], limits[i][1])
		if !ok {
			return time.Time{}, false
		}
		sets[i] = s
	}
	sets[4][0] = sets[4][0] || sets[4][7]
	domAny, dowAny := strings.HasPrefix(f[2], "*"), strings.HasPrefix(f[4], "*")
	t := after.Truncate(time.Minute).Add(time.Minute)
	for n := 0; n < 2*366*24*60; n++ {
		day := sets[2][t.Day()]
		dow := sets[4][int(t.Weekday())]
		var dayOK bool
		switch {
		case domAny:
			dayOK = dow
		case dowAny:
			dayOK = day
		default:
			dayOK = day || dow
		}
		if sets[3][int(t.Month())] && dayOK && sets[1][t.Hour()] && sets[0][t.Minute()] {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

func cronSet(field string, lo, hi int) ([]bool, bool) {
	set := make([]bool, hi+1)
	for _, part := range strings.Split(field, ",") {
		base, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 {
				return nil, false
			}
			step = n
		}
		from, to := lo, hi
		if base != "*" {
			a, b, isRange := strings.Cut(base, "-")
			x, err := strconv.Atoi(a)
			if err != nil || x < lo || x > hi {
				return nil, false
			}
			from, to = x, x
			if isRange {
				y, err := strconv.Atoi(b)
				if err != nil || y < x || y > hi {
					return nil, false
				}
				to = y
			} else if hasStep {
				to = hi
			}
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return set, true
}

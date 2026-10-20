package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/config"
)

var tlT0 = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func tlHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"meta", "wals"} {
		if err := os.MkdirAll(filepath.Join(home, "s", d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func tlBackup(t *testing.T, home, id string, begin time.Time, beginWAL string, tli int, status string) {
	t.Helper()
	body := fmt.Sprintf("begin_time=%s\nend_time=%s\nbegin_wal=%s\nend_wal=%s\ntimeline=%d\nsize=1048576\ndeduplicated_size=1048576\nstatus=%s\nparent_backup_id=None\nxlog_segment_size=16777216\n",
		begin.Format("2006-01-02 15:04:05.000000-07:00"), begin.Add(time.Minute).Format("2006-01-02 15:04:05.000000-07:00"), beginWAL, beginWAL, tli, status)
	if err := os.WriteFile(filepath.Join(home, "s", "meta", id+"-backup.info"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

// tlWAL writes the catalogue: segs are "tli:seg@minutes-after-T0" entries.
func tlWAL(t *testing.T, home string, entries ...string) {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		var tli, seg, min int
		if _, err := fmt.Sscanf(e, "%d:%d@%d", &tli, &seg, &min); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%08X%08X%08X\t1000\t%d.0\tgzip\tNone\n", tli, 0, seg, tlT0.Add(time.Duration(min)*time.Minute).Unix())
	}
	if err := os.WriteFile(filepath.Join(home, "s", "wals", "s-xlog.db"), []byte(b.String()), 0o640); err != nil {
		t.Fatal(err)
	}
}

func tlBuild(home string, now time.Time) *Timeline {
	return buildTimeline(timelineInput{Server: "s", Home: home, Now: now, Ret: config.Retention{RecoveryWindowDays: 7},
		Schedule: config.Schedule{Backup: "daily 03:10", Enabled: true}})
}

func TestTimelineContinuousChain(t *testing.T) {
	home := tlHome(t)
	tlBackup(t, home, "20261020T000000", tlT0.Add(10*time.Minute), "000000010000000000000001", 1, "DONE")
	tlBackup(t, home, "20261020T060000", tlT0.Add(6*time.Hour), "000000010000000000000004", 1, "DONE")
	tlBackup(t, home, "20261020T120000", tlT0.Add(12*time.Hour), "000000010000000000000007", 1, "DONE")
	tlWAL(t, home, "1:1@5", "1:2@60", "1:3@120", "1:4@359", "1:5@400", "1:6@500", "1:7@721", "1:8@800")
	now := tlT0.Add(14 * time.Hour)
	tl := tlBuild(home, now)
	if !tl.Available || len(tl.Backups) != 3 || tl.Totals.Backups != 3 || tl.Totals.Full != 3 || tl.Totals.BackupBytes != 3<<20 {
		t.Fatalf("%+v", tl.Totals)
	}
	if !tl.Earliest.Equal(tlT0.Add(10*time.Minute)) || tl.EarliestBackup != "20261020T000000" {
		t.Errorf("earliest %v %s", tl.Earliest, tl.EarliestBackup)
	}
	for _, b := range tl.Backups {
		if !b.Restorable {
			t.Errorf("%s must be restorable", b.ID)
		}
	}
	if !tl.Latest.Equal(tlT0.Add(800*time.Minute)) || tl.LatestSource != "last_filled_wal" || tl.LastFilledWAL != "000000010000000000000008" {
		t.Errorf("latest %v %s %s", tl.Latest, tl.LatestSource, tl.LastFilledWAL)
	}
	if len(tl.Intervals) != 3 {
		t.Fatalf("intervals %+v", tl.Intervals)
	}
	// 2,3,4 arrive after backup 1 and up to backup 2 (360 min); 5,6 up to 720.
	wantFiles := []int{3, 2, 2}
	for i, iv := range tl.Intervals {
		if iv.Files != wantFiles[i] || iv.Bytes != int64(wantFiles[i])*1000 || !iv.Continuous {
			t.Errorf("interval %d: %+v", i, iv)
		}
	}
	if tl.Totals.WALFiles != 8 || tl.Totals.WALBytes != 8000 || tl.Totals.Gaps != 0 || len(tl.Branches) != 1 {
		t.Errorf("totals %+v branches %+v", tl.Totals, tl.Branches)
	}
	if tl.Retention.Start.IsZero() || tl.Retention.Days != 7 {
		t.Errorf("retention %+v", tl.Retention)
	}
	if want := time.Date(2026, 10, 21, 3, 10, 0, 0, now.Location()); !tl.NextBackup.Equal(want) {
		t.Errorf("next backup %v want %v", tl.NextBackup, want)
	}
	if !tl.AxisStart.Before(tl.Earliest) || !tl.AxisEnd.After(now) {
		t.Errorf("axis %v %v", tl.AxisStart, tl.AxisEnd)
	}
}

func TestTimelineGapMovesTheEarliestPoint(t *testing.T) {
	home := tlHome(t)
	tlBackup(t, home, "b1", tlT0.Add(10*time.Minute), "000000010000000000000001", 1, "DONE")
	tlBackup(t, home, "b2", tlT0.Add(6*time.Hour), "000000010000000000000005", 1, "DONE")
	tlBackup(t, home, "b3", tlT0.Add(12*time.Hour), "000000010000000000000008", 1, "DONE")
	tlBackup(t, home, "bad", tlT0.Add(13*time.Hour), "000000010000000000000009", 1, "FAILED")
	// Segment 4 never arrived: backup 1 cannot reach the end any more.
	tlWAL(t, home, "1:1@5", "1:2@60", "1:3@120", "1:5@350", "1:6@500", "1:7@600", "1:8@721", "1:9@800")
	tl := tlBuild(home, tlT0.Add(14*time.Hour))
	if tl.Totals.Gaps != 1 || tl.Totals.MissingWALs != 1 || tl.Totals.FailedBackups != 1 {
		t.Fatalf("totals %+v", tl.Totals)
	}
	if tl.EarliestBackup != "b2" || !tl.Earliest.Equal(tlT0.Add(6*time.Hour)) {
		t.Errorf("earliest %s %v", tl.EarliestBackup, tl.Earliest)
	}
	if tl.Backups[0].Restorable || !tl.Backups[1].Restorable || !tl.Backups[2].Restorable || tl.Backups[3].Restorable {
		t.Errorf("restorable flags %+v", tl.Backups)
	}
	// The hole lies between segment 3 (120 min) and segment 5 (350 min): it
	// overlaps the interval b1 -> b2 and not the others.
	if tl.Intervals[0].Continuous || tl.Intervals[0].MissingWALs != 1 || !tl.Intervals[1].Continuous || !tl.Intervals[2].Continuous {
		t.Errorf("intervals %+v", tl.Intervals)
	}
}

func TestTimelineTwoTimelinesAfterRestore(t *testing.T) {
	home := tlHome(t)
	tlBackup(t, home, "b1", tlT0.Add(10*time.Minute), "000000010000000000000001", 1, "DONE")
	tlBackup(t, home, "b2", tlT0.Add(10*time.Hour), "000000020000000000000007", 2, "DONE")
	// Timeline 2 forked from 1 inside segment 5 (LSN 0/5000060).
	tlWAL(t, home, "1:1@5", "1:2@60", "1:3@120", "1:4@180", "2:5@400", "2:6@500", "2:7@601", "2:8@700")
	hist := filepath.Join(home, "s", "wals", "00000002.history")
	if err := os.WriteFile(hist, []byte("1\t0/5000060\tno recovery target specified\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	at := tlT0.Add(390 * time.Minute)
	_ = os.Chtimes(hist, at, at)
	tl := tlBuild(home, tlT0.Add(12*time.Hour))
	if len(tl.Branches) != 2 {
		t.Fatalf("branches %+v", tl.Branches)
	}
	b1, b2 := tl.Branches[0], tl.Branches[1]
	if b1.ID != 1 || b1.Files != 4 || b1.Current || b2.ID != 2 || b2.Parent != 1 || b2.Files != 4 || !b2.Current ||
		b2.SwitchLSN != "0/5000060" || !b2.BranchedAt.Equal(at) {
		t.Errorf("%+v %+v", b1, b2)
	}
	// Backup 1 (timeline 1) reaches the head by following the fork.
	if tl.EarliestBackup != "b1" || !tl.Backups[0].Restorable || !tl.Backups[1].Restorable {
		t.Errorf("earliest %s, %+v", tl.EarliestBackup, tl.Backups)
	}
	if tl.Totals.Gaps != 0 {
		t.Errorf("gaps %d: a fork is not a hole", tl.Totals.Gaps)
	}
	// Without the history file the fork is unknown and backup 1 cannot follow it.
	if err := os.Remove(hist); err != nil {
		t.Fatal(err)
	}
	tl = tlBuild(home, tlT0.Add(12*time.Hour))
	if tl.Backups[0].Restorable || tl.EarliestBackup != "b2" {
		t.Errorf("no history: %s %+v", tl.EarliestBackup, tl.Backups)
	}
}

func TestTimelineUnavailableAndStreaming(t *testing.T) {
	tl := tlBuild(t.TempDir(), tlT0)
	if tl.Available || !strings.HasPrefix(tl.Unavailable, "unavailable: ") {
		t.Errorf("%+v", tl)
	}
	home := tlHome(t)
	tlBackup(t, home, "b1", tlT0.Add(time.Hour), "000000010000000000000001", 1, "DONE")
	tlWAL(t, home, "1:1@70")
	recv := tlT0.Add(3 * time.Hour)
	tl = buildTimeline(timelineInput{Server: "s", Home: home, Now: tlT0.Add(4 * time.Hour), Streaming: true, LastReceived: recv})
	if tl.LatestSource != "streaming" || !tl.Latest.Equal(recv) || !tl.NextBackup.IsZero() {
		t.Errorf("%s %v", tl.LatestSource, tl.Latest)
	}
	// No catalogue file at all: the model stays available, without restore points.
	if err := os.Remove(filepath.Join(home, "s", "wals", "s-xlog.db")); err != nil {
		t.Fatal(err)
	}
	tl = tlBuild(home, tlT0.Add(4*time.Hour))
	if !tl.Available || !tl.Earliest.IsZero() || len(tl.Notes) == 0 {
		t.Errorf("%+v", tl)
	}
}

func TestNextCron(t *testing.T) {
	loc := time.UTC
	at := func(s string) time.Time { v, _ := time.ParseInLocation("2006-01-02 15:04", s, loc); return v }
	for _, c := range []struct{ expr, after, want string }{
		{"10 3 * * *", "2026-10-20 14:00", "2026-10-21 03:10"},
		{"10 3 * * *", "2026-10-20 01:00", "2026-10-20 03:10"},
		{"0 */6 * * *", "2026-10-20 07:00", "2026-10-20 12:00"},
		{"0 3 * * 0", "2026-10-20 07:00", "2026-10-25 03:00"}, // Tuesday -> Sunday
		{"0 * * * *", "2026-10-20 07:30", "2026-10-20 08:00"},
	} {
		got, ok := nextCron(c.expr, at(c.after))
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("%s after %s: %v %v, want %s", c.expr, c.after, got, ok, c.want)
		}
	}
	if _, ok := nextCron("nope", at("2026-10-20 07:00")); ok {
		t.Error("bad expression")
	}
}

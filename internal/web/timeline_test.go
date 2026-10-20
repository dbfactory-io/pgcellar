package web

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
)

func TestServerPageHasTheTimelineSection(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	before, _ := os.ReadFile(f.env.CallLog)
	body := f.get("/servers/pg-main", admin).Body.String()
	if after, _ := os.ReadFile(f.env.CallLog); string(after) != string(before) {
		t.Fatal("the timeline must not run Barman")
	}
	i := strings.Index(body, `<section id="timeline">`)
	j := strings.Index(body, `<section id="setup">`)
	k := strings.Index(body, "<h2>Overview</h2>")
	if k < 0 || i < k || j < i {
		t.Fatalf("section order: overview %d, timeline %d, setup %d", k, i, j)
	}
	section := body[i:j]
	for _, want := range []string{
		`<svg class="tl" viewBox="0 0 640.0 `, `width="100%"`, "<desc id=\"tl-desc\">", "<title>Backup 20261019T020001 (DONE): 1.4 GiB",
		"1019T020001 · 1.4 GiB", `class="tl-wal"`, `class="tl-now"`, "<title>Now: 20 Oct 02:30</title>",
		"You can go back to 18 Oct 02:00", `class="tl-back"`, `class="tl-band"`, "Retention window: 14 days",
		"4 files · 64.0 MiB", "Point-in-time restore is possible between 18 Oct 02:00 and 20 Oct 02:25 (2 days)",
		"3 full backups, 4.2 GiB", "WAL kept: 112.0 MiB", "retention 14 days, min 2", "next backup tomorrow 02:00",
		`class="tl-live"`,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("timeline section lacks %q", want)
		}
	}
	for _, bad := range []string{"<script", "onclick", "style="} {
		if strings.Contains(section, bad) {
			t.Errorf("timeline section must not contain %q", bad)
		}
	}
}

func TestTimelineSectionShowsHolesAndForks(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	wals := filepath.Join(f.env.Home, "pg-main", "wals")
	cat := filepath.Join(wals, "pg-main-xlog.db")
	b, _ := os.ReadFile(cat)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	// Lose segment 4 and add timeline 2, forked inside segment 6.
	kept := append([]string{}, lines[:3]...)
	kept = append(kept, lines[4:]...)
	at := time.Date(2026, 10, 20, 2, 12, 0, 0, time.Local).Unix()
	kept = append(kept, "000000020000000000000006\t16777216\t"+strconv.FormatInt(at, 10)+".0\tgzip\tNone")
	if err := os.WriteFile(cat, []byte(strings.Join(kept, "\n")+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wals, "00000002.history"), []byte("1\t0/6000028\tno recovery target specified\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	body := f.get("/servers/pg-main", admin).Body.String()
	for _, want := range []string{
		`class="tl-hole"`, "Hole in the WAL: 1 file missing", "1 hole in the WAL (1 file missing)",
		`class="tl-tli"`, `class="tl-tli cur"`, "timeline 2", "Forked from timeline 1 at 0/6000028", `class="tl-link"`,
		"You can go back to 20 Oct 02:00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestTimelineUnavailableIsSaidInThePage(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	body := f.get("/servers/pg-rsync", admin).Body.String()
	if !strings.Contains(body, `<section id="timeline">`) || !strings.Contains(body, "unavailable: cannot read the backup metadata of pg-rsync") {
		t.Errorf("unavailable text missing")
	}
	if strings.Contains(body, `<svg class="tl"`) {
		t.Error("no picture when the metadata cannot be read")
	}
}

func TestTimelineTicksAndLabelDensity(t *testing.T) {
	start := time.Date(2026, 10, 20, 8, 30, 0, 0, time.Local)
	hours := tlTicks(tlScale{start, start.Add(6 * time.Hour)})
	if len(hours) < 4 || len(hours) > 8 || hours[0].Label != "09:00" {
		t.Errorf("hour ticks: %+v", hours)
	}
	days := tlTicks(tlScale{start, start.AddDate(0, 0, 20)})
	if len(days) < 3 || len(days) > 9 || !days[0].Major {
		t.Errorf("day ticks: %+v", days)
	}
	// Thirty backups in a few hours: the labels thin out, the blocks stay.
	tl := &app.Timeline{Server: "x", Available: true, Now: start.Add(5 * time.Hour), AxisStart: start, AxisEnd: start.Add(6 * time.Hour), Notes: []string{}}
	for i := 0; i < 30; i++ {
		b := start.Add(time.Duration(i) * 10 * time.Minute)
		tl.Backups = append(tl.Backups, app.TLBackup{ID: "20261020T08" + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "00", Status: "DONE", Begin: b, End: b.Add(time.Minute), Size: 1 << 20})
	}
	v := buildTimelineView(tl)
	if len(v.Blocks) != 30 || len(v.Labels) >= 30 || len(v.Labels) == 0 {
		t.Errorf("%d blocks, %d labels", len(v.Blocks), len(v.Labels))
	}
	for _, l := range v.Labels {
		if l.X < 0 || l.X+textWidth(l.Text) > tlW {
			t.Errorf("label out of the box: %+v", l)
		}
	}
}

func TestNextBackupText(t *testing.T) {
	now := time.Date(2026, 10, 20, 14, 45, 0, 0, time.Local)
	for next, want := range map[time.Time]string{
		time.Date(2026, 10, 21, 3, 10, 0, 0, time.Local): "tonight 03:10",
		time.Date(2026, 10, 20, 22, 0, 0, 0, time.Local): "tonight 22:00",
		time.Date(2026, 10, 20, 18, 0, 0, 0, time.Local): "today 18:00",
		time.Date(2026, 10, 21, 14, 0, 0, 0, time.Local): "tomorrow 14:00",
		time.Date(2026, 10, 25, 3, 0, 0, 0, time.Local):  "Sun 25 Oct 03:00",
	} {
		if got := nextBackupText(next, now); got != want {
			t.Errorf("%v: %q, want %q", next, got, want)
		}
	}
}

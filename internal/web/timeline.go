package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

// The timeline picture is an SVG written by the server: no script, no
// library. Everything is laid out here in a 640 x H box that the browser
// scales; colours come from CSS classes (app.css), so light and dark follow
// the page. Each element carries a <title> for the hover text.

const (
	tlW      = 640.0 // viewBox width
	tlMargin = 14.0  // left and right margin inside the box
	tlChar   = 6.6   // estimated width of one label character (12 px font)

	tlLabelMarker = 12.0  // baseline of the now / next backup labels
	tlLabelAxis   = 28.0  // baseline of the axis labels
	tlTop         = 34.0  // top of the plot (grid lines start here)
	tlBlockLabelA = 58.0  // baseline of the backup labels, first row
	tlBlockLabelB = 45.0  // second row
	tlBlockY      = 64.0  // top of the backup blocks
	tlBlockH      = 32.0  // height of the backup blocks
	tlCoverY      = 104.0 // top of the WAL coverage bar
	tlCoverH      = 8.0
	tlWALLabelA   = 128.0 // baseline of the WAL size labels, first row
	tlWALLabelB   = 142.0 // second row
	tlRangeY      = 152.0 // top of the restorable range bar
	tlRangeH      = 4.0
	tlRangeLabel  = 172.0 // baseline of "You can go back to"
	tlRowsY       = 190.0 // first timeline row
	tlRowStep     = 30.0
)

type tlRect struct {
	X, Y, W, H float64
	Class      string
	Title      string
}

type tlText struct {
	X, Y   float64
	Anchor string
	Text   string
	Class  string
	Title  string
}

type tlLine struct {
	X1, Y1, X2, Y2 float64
	Class          string
	Title          string
}

type tlTick struct {
	X     float64
	Label string
	Major bool
}

type tlRow struct {
	Bar   tlRect
	Label tlText
	Link  string // path d of the connector to the parent row, may be empty
	Dot   *tlRect
}

type tlRangeView struct {
	Line  tlLine
	Bar   tlRect
	Label tlText
}

// tlView is what the template draws.
type tlView struct {
	Available   bool
	Unavailable string
	H           float64
	W           float64
	Desc        string
	Ticks       []tlTick
	GridTop     float64
	GridBottom  float64
	Band        *tlRect
	Blocks      []tlRect
	Labels      []tlText
	Cover       []tlRect
	Holes       []tlRect
	Dotted      *tlLine
	Rows        []tlRow
	Now         *tlLine
	NowLabel    tlText
	Next        *tlLine
	NextLabel   tlText
	Range       *tlRangeView
	Summary     string
	Notes       []string
	ManyLanes   bool
}

type tlScale struct{ start, end time.Time }

func (s tlScale) x(t time.Time) float64 {
	span := s.end.Sub(s.start)
	if span <= 0 {
		return tlMargin
	}
	f := float64(t.Sub(s.start)) / float64(span)
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return tlMargin + f*(tlW-2*tlMargin)
}

// tlPlacer puts text labels on a few rows without letting them overlap.
type tlPlacer struct {
	rows [][][2]float64 // per row: occupied [x1, x2] ranges
}

func newPlacer(n int) *tlPlacer { return &tlPlacer{rows: make([][][2]float64, n)} }

// place returns the first row where [x1, x2] is free (and marks it), or -1.
func (p *tlPlacer) place(x1, x2 float64) int {
	for r := range p.rows {
		free := true
		for _, o := range p.rows[r] {
			if x1 < o[1]+4 && x2 > o[0]-4 {
				free = false
				break
			}
		}
		if free {
			p.rows[r] = append(p.rows[r], [2]float64{x1, x2})
			return r
		}
	}
	return -1
}

func textWidth(s string) float64 { return float64(len([]rune(s))) * tlChar }

// shortID drops the year of a backup id and says so with an ellipsis:
// 20261020T123810 -> …1020T123810. It is the second choice when the full id
// does not fit.
func shortID(id string) string {
	if len(id) == 15 && id[8] == 'T' {
		return "…" + id[4:]
	}
	return id
}

// whenShort writes a date for the picture: "20 Oct 12:38" in local time.
func whenShort(t time.Time) string { return t.Local().Format("2 Jan 15:04") }

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }

// buildTimelineView lays out the picture and writes the sentence under it.
func buildTimelineView(t *app.Timeline) tlView {
	v := tlView{W: tlW, H: 180, GridTop: tlTop}
	if !t.Available {
		v.Unavailable = t.Unavailable
		return v
	}
	v.Available = true
	v.Notes = t.Notes
	sc := tlScale{t.AxisStart.Local(), t.AxisEnd.Local()}
	multi := len(t.Branches) > 1
	if multi {
		v.H = tlRowsY + float64(len(t.Branches))*tlRowStep - 6
	}
	v.GridBottom = v.H - 6
	h := v.GridBottom - tlTop

	// Retention window.
	if !t.Retention.Start.IsZero() {
		x1 := sc.x(t.Retention.Start)
		x2 := sc.x(t.Now)
		title := fmt.Sprintf("Retention window: %s, back to %s", plural(t.Retention.Days, "day", "days"), whenShort(t.Retention.Start))
		if t.Retention.Start.Before(t.AxisStart) {
			title += " (starts before the left edge of the picture)"
		}
		v.Band = &tlRect{X: x1, Y: tlTop, W: x2 - x1, H: h, Class: "tl-band", Title: title}
	}

	v.Ticks = tlTicks(sc)

	// Backup blocks and their labels.
	place := newPlacer(2)
	for _, b := range t.Backups {
		x1 := sc.x(b.Begin)
		x2 := x1
		if !b.End.IsZero() {
			x2 = sc.x(b.End)
		}
		w := x2 - x1
		if w < 5 {
			w = 5
		}
		if x1+w > tlW-tlMargin {
			x1 = tlW - tlMargin - w
		}
		class := "tl-bk"
		switch b.Status {
		case "DONE":
		case "FAILED":
			class += " bad"
		default:
			class += " warn"
		}
		title := fmt.Sprintf("Backup %s (%s): %s, started %s", b.ID, b.Status, humanSize(b.Size), whenShort(b.Begin))
		if !b.End.IsZero() {
			title += ", took " + app.HumanDuration(b.End.Sub(b.Begin))
		}
		if b.Status == "DONE" {
			if b.Restorable {
				title += ". WAL chain unbroken up to the last WAL file."
			} else {
				title += ". WAL chain broken or missing: a point-in-time restore from this backup is not possible."
			}
		}
		if b.ParentID != "" {
			title += " Incremental, parent " + b.ParentID + "."
		}
		v.Blocks = append(v.Blocks, tlRect{X: x1, Y: tlBlockY, W: w, H: tlBlockH, Class: class, Title: title})
		for _, label := range []string{b.ID + " · " + humanSize(b.Size), shortID(b.ID) + " · " + humanSize(b.Size), humanSize(b.Size)} {
			lw := textWidth(label)
			lx := x1
			if lx+lw > tlW-tlMargin {
				lx = tlW - tlMargin - lw
			}
			if r := place.place(lx, lx+lw); r >= 0 {
				y := tlBlockLabelA
				if r == 1 {
					y = tlBlockLabelB
				}
				v.Labels = append(v.Labels, tlText{X: lx, Y: y, Anchor: "start", Text: label, Title: title})
				break
			}
		}
	}

	// WAL coverage between backups, with the size of the WAL under it.
	wplace := newPlacer(2)
	for i, iv := range t.Intervals {
		to := iv.To
		last := i == len(t.Intervals)-1
		if last && !t.LastFilledTime.IsZero() && t.LastFilledTime.After(iv.From) {
			to = t.LastFilledTime
		}
		x1, x2 := sc.x(iv.From), sc.x(to)
		if x2-x1 < 2 {
			x2 = x1 + 2
		}
		title := fmt.Sprintf("WAL after backup %s: %s, %s", iv.FromBackup, plural(iv.Files, "file", "files"), humanSize(iv.Bytes))
		if iv.Files == 0 {
			title = fmt.Sprintf("No WAL file archived after backup %s yet", iv.FromBackup)
		}
		if iv.Continuous {
			title += ". No hole."
		} else {
			title += fmt.Sprintf(". %s missing.", plural(int(iv.MissingWALs), "WAL file is", "WAL files are"))
		}
		v.Cover = append(v.Cover, tlRect{X: x1, Y: tlCoverY, W: x2 - x1, H: tlCoverH, Class: "tl-wal", Title: title})
		cx := (x1 + x2) / 2
		for _, label := range []string{plural(iv.Files, "file", "files") + " · " + humanSize(iv.Bytes), humanSize(iv.Bytes)} {
			if iv.Files == 0 {
				break
			}
			lw := textWidth(label)
			lx := cx - lw/2
			if lx < tlMargin {
				lx = tlMargin
			}
			if lx+lw > tlW-tlMargin {
				lx = tlW - tlMargin - lw
			}
			if lx < x1-lw/2 || lx+lw > x2+lw/2 {
				continue
			}
			if r := wplace.place(lx, lx+lw); r >= 0 {
				y := tlWALLabelA
				if r == 1 {
					y = tlWALLabelB
				}
				v.Labels = append(v.Labels, tlText{X: lx, Y: y, Anchor: "start", Text: label, Class: "tl-sub", Title: title})
				break
			}
		}
	}
	for _, g := range t.Gaps {
		x1, x2 := sc.x(g.From), sc.x(g.To)
		if x2-x1 < 3 {
			x2 = x1 + 3
		}
		v.Holes = append(v.Holes, tlRect{X: x1, Y: tlCoverY - 2, W: x2 - x1, H: tlCoverH + 4, Class: "tl-hole",
			Title: fmt.Sprintf("Hole in the WAL: %s missing between %s and %s (timeline %d). Restoring across it is not possible.", plural(int(g.Missing), "file", "files"), whenShort(g.From), whenShort(g.To), g.Timeline)})
	}
	if t.LatestSource == "streaming" && t.LastReceived.After(t.LastFilledTime) {
		x1, x2 := sc.x(t.LastFilledTime), sc.x(t.LastReceived)
		v.Dotted = &tlLine{X1: x1, X2: x2, Y1: tlCoverY + tlCoverH/2, Y2: tlCoverY + tlCoverH/2, Class: "tl-live",
			Title: "Streaming: data received up to " + whenShort(t.LastReceived) + ", not yet in a complete WAL file"}
	}

	// Restorable range.
	if !t.Earliest.IsZero() && !t.Latest.IsZero() {
		ex, lx := sc.x(t.Earliest), sc.x(t.Latest)
		if lx-ex < 3 {
			lx = ex + 3
		}
		label := tr("tl.goback") + " " + whenShort(t.Earliest)
		anchor, tx := "start", ex
		if ex+textWidth(label) > tlW-tlMargin {
			anchor, tx = "end", ex
			if ex-textWidth(label) < tlMargin {
				anchor, tx = "start", tlW-tlMargin-textWidth(label)
			}
		}
		title := fmt.Sprintf("Point-in-time restore is possible from %s (end of backup %s) to %s", whenShort(t.Earliest), t.EarliestBackup, whenShort(t.Latest))
		v.Range = &tlRangeView{
			Line:  tlLine{X1: ex, X2: ex, Y1: tlTop, Y2: tlRangeY + tlRangeH, Class: "tl-back", Title: title},
			Bar:   tlRect{X: ex, Y: tlRangeY, W: lx - ex, H: tlRangeH, Class: "tl-back-bar", Title: title},
			Label: tlText{X: tx, Y: tlRangeLabel, Anchor: anchor, Text: label, Class: "tl-back-t", Title: title},
		}
	}

	// PostgreSQL timelines, one row each, only when there is a fork.
	if multi {
		ys := map[uint32]float64{}
		for k, b := range t.Branches {
			y := tlRowsY + float64(k)*tlRowStep
			ys[b.ID] = y
			from := b.First
			if !b.BranchedAt.IsZero() && b.Parent != 0 {
				from = b.BranchedAt
			}
			to := b.Last
			if b.Current && !t.Latest.IsZero() && t.Latest.After(to) {
				to = t.Latest
			}
			x1, x2 := sc.x(from), sc.x(to)
			if x2-x1 < 4 {
				x2 = x1 + 4
			}
			title := fmt.Sprintf("Timeline %d: %s, %s", b.ID, plural(b.Files, "WAL file", "WAL files"), humanSize(b.Bytes))
			if b.Parent != 0 {
				title += fmt.Sprintf(". Forked from timeline %d at %s (%s)", b.Parent, b.SwitchLSN, b.Reason)
			}
			class := "tl-tli"
			if b.Current {
				class += " cur"
			}
			row := tlRow{
				Bar:   tlRect{X: x1, Y: y, W: x2 - x1, H: 6, Class: class, Title: title},
				Label: tlText{X: x1, Y: y - 5, Anchor: "start", Text: fmt.Sprintf("%s %d", tr("tl.timeline"), b.ID), Class: "tl-sub", Title: title},
			}
			if row.Label.X+textWidth(row.Label.Text) > tlW-tlMargin {
				row.Label.X = tlW - tlMargin - textWidth(row.Label.Text)
			}
			if py, ok := ys[b.Parent]; ok && b.Parent != 0 {
				row.Link = fmt.Sprintf("M%s %s V%s", f1(x1), f1(py+3), f1(y+3))
				row.Dot = &tlRect{X: x1, Y: py + 3, W: 3, Class: "tl-dot", Title: title}
			}
			v.Rows = append(v.Rows, row)
		}
	}

	// Now and next backup.
	nx := sc.x(t.Now)
	v.Now = &tlLine{X1: nx, X2: nx, Y1: tlTop - 4, Y2: v.GridBottom, Class: "tl-now", Title: "Now: " + whenShort(t.Now)}
	v.NowLabel = tlText{X: nx - 3, Y: tlLabelMarker, Anchor: "end", Text: tr("tl.now"), Class: "tl-sub", Title: v.Now.Title}
	if !t.NextBackup.IsZero() && !t.NextBackup.After(t.AxisEnd) {
		x := sc.x(t.NextBackup)
		v.Next = &tlLine{X1: x, X2: x, Y1: tlTop - 4, Y2: v.GridBottom, Class: "tl-next", Title: "Next backup: " + whenShort(t.NextBackup)}
		v.NextLabel = tlText{X: x + 3, Y: tlLabelMarker, Anchor: "start", Text: tr("tl.next"), Class: "tl-sub", Title: v.Next.Title}
	}

	v.Desc = timelineDesc(t)
	v.Summary = timelineSummary(t)
	return v
}

// tlTicks picks round hours (axis under two days) or round days.
func tlTicks(sc tlScale) []tlTick {
	span := sc.end.Sub(sc.start)
	var out []tlTick
	loc := sc.start.Location()
	if span < 48*time.Hour {
		step := time.Hour
		for _, c := range []time.Duration{1, 2, 3, 6, 12} {
			step = c * time.Hour
			if span/step <= 8 {
				break
			}
		}
		// Round down to the hour of the displayed zone, not of UTC: in a zone
		// with a half-hour offset (India, +05:30) Truncate would put every
		// tick on :30.
		s := sc.start.In(loc)
		t := time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), 0, 0, 0, loc)
		for ; t.Before(sc.start) || t.Hour()%int(step/time.Hour) != 0; t = t.Add(time.Hour) {
		}
		for ; !t.After(sc.end); t = t.Add(step) {
			lt := t.In(loc)
			tk := tlTick{X: sc.x(t), Label: lt.Format("15:04")}
			if lt.Hour() == 0 {
				tk.Label, tk.Major = lt.Format("2 Jan"), true
			}
			out = append(out, tk)
		}
		return out
	}
	days := 1
	for _, c := range []int{1, 2, 3, 7, 14, 30} {
		days = c
		if int(span.Hours()/24)/c <= 8 {
			break
		}
	}
	t := time.Date(sc.start.Year(), sc.start.Month(), sc.start.Day(), 0, 0, 0, 0, loc)
	if t.Before(sc.start) {
		t = t.AddDate(0, 0, 1)
	}
	for ; !t.After(sc.end); t = t.AddDate(0, 0, days) {
		out = append(out, tlTick{X: sc.x(t), Label: t.Format("2 Jan"), Major: true})
	}
	return out
}

func timelineDesc(t *app.Timeline) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Timeline of server %s from %s to %s. ", t.Server, whenShort(t.AxisStart), whenShort(t.AxisEnd))
	fmt.Fprintf(&b, "%s, %s of WAL kept", plural(t.Totals.Backups, "backup", "backups"), humanSize(t.Totals.WALBytes))
	if !t.Earliest.IsZero() {
		fmt.Fprintf(&b, "; point-in-time restore from %s to %s", whenShort(t.Earliest), whenShort(t.Latest))
	}
	if t.Totals.Gaps > 0 {
		fmt.Fprintf(&b, "; %s in the WAL", plural(t.Totals.Gaps, "hole", "holes"))
	}
	b.WriteString(".")
	return b.String()
}

// tonight / today / tomorrow wording for the next planned backup.
func nextBackupText(next, now time.Time) string {
	n, c := next.Local(), now.Local()
	hm := n.Format("15:04")
	days := int(time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Sub(time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, c.Location())).Hours() / 24)
	switch {
	case next.Sub(now) <= 14*time.Hour && (n.Hour() >= 21 || n.Hour() < 6):
		return "tonight " + hm
	case days == 0:
		return "today " + hm
	case days == 1:
		return "tomorrow " + hm
	}
	return n.Format("Mon 2 Jan") + " " + hm
}

// timelineSummary is the sentence under the picture.
func timelineSummary(t *app.Timeline) string {
	var parts []string
	switch {
	case !t.Earliest.IsZero():
		parts = append(parts, fmt.Sprintf("Point-in-time restore is possible between %s and %s (%s)", whenShort(t.Earliest), whenShort(t.Latest), app.HumanDuration(t.Latest.Sub(t.Earliest))))
	case t.Totals.Backups == 0:
		parts = append(parts, "No complete backup yet, so no point-in-time restore")
	default:
		parts = append(parts, "No point-in-time restore is possible: no backup has an unbroken WAL chain up to the last WAL file")
	}
	bk := plural(t.Totals.Full, "full backup", "full backups")
	if t.Totals.Incremental > 0 {
		bk += " + " + plural(t.Totals.Incremental, "incremental", "incrementals")
	}
	parts = append(parts, bk+", "+humanSize(t.Totals.BackupBytes))
	wal := humanSize(t.Totals.WALBytes)
	if t.Totals.WALBytes == 0 {
		wal = "none"
	}
	parts = append(parts, "WAL kept: "+wal)
	if t.Totals.Gaps > 0 {
		parts = append(parts, fmt.Sprintf("%s in the WAL (%s missing)", plural(t.Totals.Gaps, "hole", "holes"), plural(int(t.Totals.MissingWALs), "file", "files")))
	}
	if r := fmtRetention(config.Retention{RecoveryWindowDays: t.Retention.Days, Redundancy: t.Retention.Redundancy, MinimumRedundancy: t.Retention.MinimumRedundancy}); r != "-" {
		parts = append(parts, "retention "+r)
	}
	if !t.NextBackup.IsZero() {
		parts = append(parts, "next backup "+nextBackupText(t.NextBackup, t.Now))
	}
	return strings.Join(parts, " · ")
}

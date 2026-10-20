package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
)

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func ageSec(s *int64) string {
	if s == nil {
		return "-"
	}
	return app.HumanDuration(time.Duration(*s) * time.Second)
}

func humanSize(n int64) string {
	if n <= 0 {
		return "-"
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

func fmtRetention(r config.Retention) string {
	var parts []string
	if r.RecoveryWindowDays > 0 {
		parts = append(parts, fmt.Sprintf("%d days", r.RecoveryWindowDays))
	}
	if r.Redundancy > 0 {
		parts = append(parts, fmt.Sprintf("%d backups", r.Redundancy))
	}
	if r.MinimumRedundancy > 0 {
		parts = append(parts, fmt.Sprintf("min %d", r.MinimumRedundancy))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func runtime(r app.RunView) string {
	end := time.Now()
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	d := end.Sub(r.StartedAt)
	switch {
	case d < time.Second:
		return "less than a second"
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return app.HumanDuration(d)
}

// whenText writes a date for people: "today 14:58", "yesterday 09:03", then a
// short date ("3 Oct, 14:58"), with the year once it is not the current one.
func whenText(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	t, now = t.Local(), now.Local()
	day := func(x time.Time) time.Time { return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, x.Location()) }
	switch days := int(day(now).Sub(day(t)).Hours()/24 + 0.5); {
	case days == 0:
		return "today " + t.Format("15:04")
	case days == 1:
		return "yesterday " + t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("2 Jan, 15:04")
	}
	return t.Format("2 Jan 2006, 15:04")
}

// fmtWhen is the template form of whenText: a <time> element whose tooltip
// carries the exact date.
func fmtWhen(t time.Time) template.HTML {
	if t.IsZero() {
		return "-"
	}
	return template.HTML(fmt.Sprintf(`<time datetime=%q title=%q>%s</time>`,
		t.UTC().Format(time.RFC3339), template.HTMLEscapeString(fmtTime(t)), template.HTMLEscapeString(whenText(t, time.Now()))))
}

// summary counts the servers by level and writes the one-sentence headline of
// the status page, with the level it should be dressed in.
type summary struct {
	Level string // ok, warn, fail, empty
	Text  string
	Sub   string
}

func summarize(states []app.ServerState, stale bool) summary {
	var fail, warn int
	for _, s := range states {
		switch s.Level {
		case app.LevelFail:
			fail++
		case app.LevelWarn:
			warn++
		}
	}
	var sm summary
	switch {
	case len(states) == 0:
		sm = summary{Level: "empty", Text: tr("status.none")}
	case fail == 1:
		sm = summary{Level: app.LevelFail, Text: tr("status.fail_one")}
	case fail > 1:
		sm = summary{Level: app.LevelFail, Text: fmt.Sprintf(tr("status.fail_many"), fail)}
	case warn == 1:
		sm = summary{Level: app.LevelWarn, Text: tr("status.warn_one")}
	case warn > 1:
		sm = summary{Level: app.LevelWarn, Text: fmt.Sprintf(tr("status.warn_many"), warn)}
	default:
		sm = summary{Level: app.LevelOK, Text: tr("status.ok")}
	}
	if fail > 0 && warn == 1 {
		sm.Sub = tr("status.also_warn_one")
	} else if fail > 0 && warn > 1 {
		sm.Sub = fmt.Sprintf(tr("status.also_warn_many"), warn)
	}
	if stale && len(states) > 0 {
		sm.Sub = strings.TrimSpace(sm.Sub + " " + tr("status.refreshing"))
	}
	return sm
}

// fmtSpan shows a backup's time: start and duration when it lasted under a
// minute, start and end otherwise.
func fmtSpan(begin, end time.Time) template.HTML {
	if begin.IsZero() || end.IsZero() || end.Before(begin) {
		return fmtWhen(begin)
	}
	d := end.Sub(begin)
	if d < time.Minute {
		return fmtWhen(begin) + template.HTML(fmt.Sprintf(" &middot; %d s", int(d.Seconds())))
	}
	if sameDay(begin, end) {
		return fmtWhen(begin) + " &rarr; " + template.HTML(end.Local().Format("15:04"))
	}
	return fmtWhen(begin) + " &rarr; " + fmtWhen(end)
}

func sameDay(a, b time.Time) bool {
	a, b = a.Local(), b.Local()
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// fmtWhenStr is fmtWhen for a date kept as an RFC 3339 string (git history);
// a string that does not parse is shown as it is.
func fmtWhenStr(s string) template.HTML {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return fmtWhen(t)
}

// okCount is the number of passing checks.
func okCount(cs []app.CheckView) int {
	n := 0
	for _, c := range cs {
		if c.OK {
			n++
		}
	}
	return n
}

// setupOK counts the setup lines that are fine.
func setupOK(list []barman.SetupCheck) int {
	n := 0
	for _, c := range list {
		if c.Level == barman.SetupOK {
			n++
		}
	}
	return n
}

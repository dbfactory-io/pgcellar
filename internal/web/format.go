package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
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

func fmtRetention(r app.RetentionInput) string {
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

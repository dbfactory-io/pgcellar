package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	dailyRe  = regexp.MustCompile(`^daily\s+(\d{1,2}):(\d{2})$`)
	weeklyRe = regexp.MustCompile(`^weekly\s+([a-z]{3})\s+(\d{1,2}):(\d{2})$`)
	everyRe  = regexp.MustCompile(`^every\s+(\d+)\s+hours?$`)
)

var weekdays = map[string]struct {
	num  int
	name string
}{
	"sun": {0, "Sunday"}, "mon": {1, "Monday"}, "tue": {2, "Tuesday"},
	"wed": {3, "Wednesday"}, "thu": {4, "Thursday"}, "fri": {5, "Friday"}, "sat": {6, "Saturday"},
}

func parseHM(h, m string) (int, int, error) {
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	if hh > 23 || mm > 59 {
		return 0, 0, fmt.Errorf("invalid time %s:%s (expected 00:00 to 23:59)", h, m)
	}
	return hh, mm, nil
}

// ParseSchedule turns a human schedule into a 5-field cron expression and a
// readable description. Accepted forms: "daily HH:MM", "weekly <mon..sun> HH:MM",
// "hourly", "every N hours", or a raw 5-field cron expression.
func ParseSchedule(s string) (cron string, human string, err error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return "", "", fmt.Errorf("schedule is empty")
	}
	if m := dailyRe.FindStringSubmatch(s); m != nil {
		h, mi, err := parseHM(m[1], m[2])
		if err != nil {
			return "", "", err
		}
		return fmt.Sprintf("%d %d * * *", mi, h), fmt.Sprintf("Every day at %02d:%02d", h, mi), nil
	}
	if m := weeklyRe.FindStringSubmatch(s); m != nil {
		d, ok := weekdays[m[1]]
		if !ok {
			return "", "", fmt.Errorf("unknown weekday %q (use mon, tue, wed, thu, fri, sat or sun)", m[1])
		}
		h, mi, err := parseHM(m[2], m[3])
		if err != nil {
			return "", "", err
		}
		return fmt.Sprintf("%d %d * * %d", mi, h, d.num), fmt.Sprintf("Every %s at %02d:%02d", d.name, h, mi), nil
	}
	if s == "hourly" {
		return "0 * * * *", "Every hour", nil
	}
	if m := everyRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > 23 {
			return "", "", fmt.Errorf("\"every N hours\" needs N between 1 and 23")
		}
		if n == 1 {
			return "0 * * * *", "Every hour", nil
		}
		return fmt.Sprintf("0 */%d * * *", n), fmt.Sprintf("Every %d hours", n), nil
	}
	fields := strings.Fields(s)
	if len(fields) == 5 {
		if err := validateCronFields(fields); err != nil {
			return "", "", err
		}
		return strings.Join(fields, " "), "Cron: " + strings.Join(fields, " "), nil
	}
	return "", "", fmt.Errorf("cannot understand schedule %q (try \"daily 02:00\", \"weekly sun 03:00\", \"hourly\", \"every 6 hours\" or a 5-field cron expression)", s)
}

var cronLimits = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

func validateCronFields(fields []string) error {
	for i, f := range fields {
		for _, part := range strings.Split(f, ",") {
			if err := validateCronPart(part, cronLimits[i][0], cronLimits[i][1]); err != nil {
				return fmt.Errorf("cron field %d (%q): %w", i+1, f, err)
			}
		}
	}
	return nil
}

func validateCronPart(p string, lo, hi int) error {
	if p == "" {
		return fmt.Errorf("empty element")
	}
	base, step, hasStep := strings.Cut(p, "/")
	if hasStep {
		n, err := strconv.Atoi(step)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid step %q", step)
		}
	}
	if base == "*" {
		return nil
	}
	a, b, isRange := strings.Cut(base, "-")
	x, err := strconv.Atoi(a)
	if err != nil || x < lo || x > hi {
		return fmt.Errorf("value %q out of range %d-%d", a, lo, hi)
	}
	if isRange {
		y, err := strconv.Atoi(b)
		if err != nil || y < lo || y > hi || y < x {
			return fmt.Errorf("invalid range end %q", b)
		}
	}
	return nil
}

// RenderCrontab produces the lines for the barman user's crontab: one
// `barman cron` per minute, then one `barman backup <server>` per enabled
// server whose schedule parses.
func RenderCrontab(cfg *Config) string {
	var b strings.Builder
	b.WriteString("# Managed by pgCellar from pgcellar.yaml. Do not edit by hand.\n")
	bin := cfg.Barman.Binary
	if bin == "" {
		bin = "barman"
	}
	user := cfg.Barman.User
	if user == "" {
		user = "barman"
	}
	fmt.Fprintf(&b, "* * * * * %s %s cron\n", user, bin)
	for _, s := range cfg.Servers {
		if !s.Schedule.IsEnabled() {
			continue
		}
		cron, _, err := ParseSchedule(s.Schedule.Backup)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s backup %s\n", cron, user, bin, s.Name)
	}
	return b.String()
}

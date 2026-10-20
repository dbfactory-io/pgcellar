package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	windowRe     = regexp.MustCompile(`^RECOVERY\s+WINDOW\s+OF\s+(\d+)\s+(DAY|DAYS|WEEK|WEEKS|MONTH|MONTHS)$`)
	redundancyRe = regexp.MustCompile(`^REDUNDANCY\s+(\d+)$`)
)

// RetentionPolicy renders the Barman retention_policy value, or "" when none
// is set. A recovery window wins over redundancy when both are given.
func RetentionPolicy(r Retention) string {
	switch {
	case r.RecoveryWindowDays > 0:
		unit := "DAYS"
		if r.RecoveryWindowDays == 1 {
			unit = "DAY"
		}
		return fmt.Sprintf("RECOVERY WINDOW OF %d %s", r.RecoveryWindowDays, unit)
	case r.Redundancy > 0:
		return fmt.Sprintf("REDUNDANCY %d", r.Redundancy)
	}
	return ""
}

// MinimumRedundancy renders the minimum_redundancy value, or "" when unset.
func MinimumRedundancy(r Retention) string {
	if r.MinimumRedundancy <= 0 {
		return ""
	}
	return strconv.Itoa(r.MinimumRedundancy)
}

// ParseRetentionPolicy reads a Barman retention_policy value (case and
// spacing tolerant). Weeks and months are converted to days. An empty string
// yields a zero Retention.
func ParseRetentionPolicy(s string) (Retention, error) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return Retention{}, nil
	}
	if m := windowRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch {
		case strings.HasPrefix(m[2], "WEEK"):
			n *= 7
		case strings.HasPrefix(m[2], "MONTH"):
			n *= 30
		}
		return Retention{RecoveryWindowDays: n}, nil
	}
	if m := redundancyRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return Retention{Redundancy: n}, nil
	}
	return Retention{}, fmt.Errorf("cannot understand retention policy %q (use \"RECOVERY WINDOW OF 7 DAYS\" or \"REDUNDANCY 3\")", s)
}

// BuildRetention combines a retention_policy and a minimum_redundancy string
// as read from a Barman server section.
func BuildRetention(policy, minRedundancy string) (Retention, error) {
	r, err := ParseRetentionPolicy(policy)
	if err != nil {
		return r, err
	}
	minRedundancy = strings.TrimSpace(minRedundancy)
	if minRedundancy != "" {
		n, err := strconv.Atoi(minRedundancy)
		if err != nil || n < 0 {
			return r, fmt.Errorf("invalid minimum_redundancy %q", minRedundancy)
		}
		r.MinimumRedundancy = n
	}
	return r, nil
}

package updates

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version (https://semver.org): MAJOR.MINOR.PATCH
// with an optional pre-release; the build metadata is ignored for ordering.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // pre-release identifiers, empty for a release
}

// ParseVersion reads "1.2.3", "v1.2.3", "1.2.3-rc.1" or "1.2.3+build".
// "dev", an empty string or anything else is an error.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%q is not a version (MAJOR.MINOR.PATCH)", s)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("%q is not a version (MAJOR.MINOR.PATCH)", s)
		}
		n[i] = v
	}
	v := Version{Major: n[0], Minor: n[1], Patch: n[2]}
	if hasPre {
		if pre == "" {
			return Version{}, fmt.Errorf("%q has an empty pre-release", s)
		}
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if id == "" {
				return Version{}, fmt.Errorf("%q has an empty pre-release identifier", s)
			}
		}
	}
	return v, nil
}

// String writes the version back without a leading v.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Compare returns -1, 0 or 1 as v is older than, the same as, or newer than o.
func (v Version) Compare(o Version) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0: // a release is newer than its pre-releases
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := compareIdent(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}
	return sign(len(v.Pre) - len(o.Pre))
}

// compareIdent orders two pre-release identifiers: numbers numerically and
// before words, words in ASCII order.
func compareIdent(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return sign(na - nb)
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// Newer reports whether latest is a newer version than current. A current
// version that is not a release ("dev", empty) is never older than anything:
// pgCellar cannot tell, so it does not claim an update is available.
func Newer(current, latest string) bool {
	c, err := ParseVersion(current)
	if err != nil {
		return false
	}
	l, err := ParseVersion(latest)
	if err != nil {
		return false
	}
	return l.Compare(c) > 0
}

// IsRelease reports whether s is a version pgCellar can compare.
func IsRelease(s string) bool {
	_, err := ParseVersion(s)
	return err == nil
}

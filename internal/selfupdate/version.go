package selfupdate

import (
	"strconv"
	"strings"
)

// normalize drops a leading "v": tags are v0.1.1, binaries say 0.1.1.
func normalize(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// newer reports whether version a is strictly greater than b, comparing
// MAJOR.MINOR.PATCH numerically; a version with a suffix (0.2.0-rc.1, or a
// build from git describe such as 0.1.0-3-gabc123) sorts before the same
// version without one. Two suffixes compare as strings. A version that does
// not parse is never newer.
func newer(a, b string) bool {
	an, as, ok1 := parse(a)
	bn, bs, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return an[i] > bn[i]
		}
	}
	switch {
	case as == bs:
		return false
	case as == "":
		return true
	case bs == "":
		return false
	}
	return as > bs
}

func parse(v string) (nums [3]int, suffix string, ok bool) {
	v = normalize(v)
	core := v
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		core, suffix = v[:i], v[i+1:]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nums, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, "", false
		}
		nums[i] = n
	}
	return nums, suffix, true
}

// mentions reports whether the output of `pgcellar version` names version v
// as a whole word (0.1.1 must not match 0.1.10).
func mentions(out, v string) bool {
	v = normalize(v)
	for _, f := range strings.Fields(out) {
		if normalize(f) == v {
			return true
		}
	}
	return false
}

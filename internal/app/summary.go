package app

import (
	"fmt"
	"strings"
)

// SummaryTexts are the sentences of the headline "Everything is fine ✅". The
// web interface puts them in its catalogue (so they can be translated); the
// daily alert uses them as they are. One source of truth for the wording.
var SummaryTexts = map[string]string{
	"status.ok":             "Everything is fine ✅",
	"status.warn_one":       "1 server needs attention ⚠️",
	"status.warn_many":      "%d servers need attention ⚠️",
	"status.fail_one":       "1 server is failing ❌ — start there.",
	"status.fail_many":      "%d servers are failing ❌ — start there.",
	"status.also_warn_one":  "1 more server needs attention.",
	"status.also_warn_many": "%d more servers need attention.",
	"status.refreshing":     "🔄 Showing the last known state while pgCellar refreshes.",
	"status.none":           "No server to watch yet 🍷",
}

// Summary is the one-sentence headline of the Status page and of the daily
// summary, with the level it should be dressed in (ok, warn, fail, empty).
type Summary struct {
	Level string
	Text  string
	Sub   string
}

// Summarize counts the servers by level and writes the headline. tr looks a
// sentence up by key; nil uses SummaryTexts.
func Summarize(states []ServerState, stale bool, tr func(string) string) Summary {
	if tr == nil {
		tr = func(k string) string { return SummaryTexts[k] }
	}
	var fail, warn int
	for _, s := range states {
		switch s.Level {
		case LevelFail:
			fail++
		case LevelWarn:
			warn++
		}
	}
	var sm Summary
	switch {
	case len(states) == 0:
		sm = Summary{Level: "empty", Text: tr("status.none")}
	case fail == 1:
		sm = Summary{Level: LevelFail, Text: tr("status.fail_one")}
	case fail > 1:
		sm = Summary{Level: LevelFail, Text: fmt.Sprintf(tr("status.fail_many"), fail)}
	case warn == 1:
		sm = Summary{Level: LevelWarn, Text: tr("status.warn_one")}
	case warn > 1:
		sm = Summary{Level: LevelWarn, Text: fmt.Sprintf(tr("status.warn_many"), warn)}
	default:
		sm = Summary{Level: LevelOK, Text: tr("status.ok")}
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

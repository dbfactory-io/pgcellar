package app

import "time"

// DrillSummary is the sentence at the top of a drill report, for the fictional
// drills of the public demo.
func DrillSummary(server, backup string, at time.Time, ok bool, failedKey string, d DrillDetails) string {
	return drillSummary(server, backup, at, ok, failedKey, d)
}

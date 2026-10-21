package barman

import (
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/tztest"
)

// The rule for Barman dates, checked under two process zones: a date with an
// offset is the instant it says, whatever the zone; a date without a zone is
// a wall-clock time of the Barman server, which is this machine (time.Local).
func TestParseTimeZoneRule(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	end := time.Date(2026, 10, 20, 2, 10, 30, 123456000, cest)
	tztest.InZones(t, []string{"UTC", "Europe/Brussels"}, func(t *testing.T) {
		zoned := map[string]time.Time{
			"2026-10-20 02:10:30.123456+02:00": end,                       // backup.info, show-backup
			"2026-10-20 00:10:30.123456+00:00": end,                       // the same instant in UTC
			"2026-10-20T02:10:30.123456+02:00": end,                       // RFC 3339
			"1792455030":                       end.Truncate(time.Second), // *_timestamp keys
		}
		for in, want := range zoned {
			if got := parseTime(in); !got.Equal(want) {
				t.Errorf("parseTime(%q) = %v, want %v", in, got, want)
			}
		}
		wall := time.Date(2026, 10, 20, 2, 10, 30, 0, time.Local)
		for _, in := range []string{"Tue Oct 20 02:10:30 2026", "2026-10-20 02:10:30"} {
			if got := parseTime(in); !got.Equal(wall) {
				t.Errorf("parseTime(%q) = %v, want %v (local wall clock)", in, got, wall)
			}
		}
		// An abbreviation Go cannot resolve must not shift the date to UTC.
		if got := parseTime("Tue Oct 20 02:10:30 2026 XYZT"); !got.Equal(wall) {
			t.Errorf("unknown abbreviation: %v, want %v", got, wall)
		}
		for _, in := range []string{"", "None", "yesterday"} {
			if got := parseTime(in); !got.IsZero() {
				t.Errorf("parseTime(%q) = %v, want zero", in, got)
			}
		}
		if got := idTime("20261020T020001"); !got.Equal(time.Date(2026, 10, 20, 2, 0, 1, 0, time.Local)) {
			t.Errorf("idTime = %v", got)
		}
	})
}

// backup.info and the JSON of show-backup describe the same backup: both
// paths must give the same instant, in every process zone.
func TestBackupInfoAndJSONAgree(t *testing.T) {
	tztest.InZones(t, []string{"UTC", "Europe/Brussels", "America/Los_Angeles"}, func(t *testing.T) {
		info := "begin_time=2026-10-20 02:00:01.412345+02:00\nend_time=2026-10-20 02:10:30.123456+02:00\nstatus=DONE\n"
		b, err := ParseBackupInfo("20261020T020001", strings.NewReader(info))
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 10, 20, 0, 10, 30, 123456000, time.UTC)
		if !b.EndTime.Equal(want) {
			t.Errorf("backup.info end_time = %v, want %v", b.EndTime, want)
		}
		if got := parseTime("2026-10-20 02:10:30.123456+02:00"); !got.Equal(b.EndTime) {
			t.Errorf("JSON end_time %v differs from backup.info %v", got, b.EndTime)
		}
	})
}

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/tztest"
)

// The same Barman data and the same instant must give the same ages whatever
// the time zone of the pgCellar process: a CI runner in UTC, an operator's
// laptop in Brussels, a server on the US west coast or in India.
func TestMetricsAgesDoNotDependOnTheProcessZone(t *testing.T) {
	tztest.InZones(t, []string{"UTC", "Europe/Brussels", "America/Los_Angeles", "Asia/Kolkata"}, func(t *testing.T) {
		body := newFixture(t).get("/metrics", nil).Body.String()
		for _, want := range []string{
			`pgcellar_last_backup_age_seconds{server="pg-main"} 1170`,
			`pgcellar_last_wal_received_age_seconds{server="pg-main"} 300`,
			`pgcellar_backups_total{server="pg-main"} 3`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("zone %s: metrics lacks %q", time.Local, want)
			}
		}
	})
}

// Hour ticks fall on whole hours of the displayed zone, also where the offset
// is not a whole number of hours.
func TestTimelineTicksOnWholeLocalHours(t *testing.T) {
	for _, zone := range []string{"UTC", "Europe/Brussels", "Asia/Kolkata"} {
		t.Run(zone, func(t *testing.T) {
			loc := tztest.Load(t, zone)
			start := time.Date(2026, 10, 20, 8, 30, 0, 0, loc)
			hours := tlTicks(tlScale{start, start.Add(6 * time.Hour)})
			if len(hours) == 0 || hours[0].Label != "09:00" {
				t.Fatalf("hour ticks: %+v", hours)
			}
			for _, h := range hours {
				if !h.Major && !strings.HasSuffix(h.Label, ":00") {
					t.Errorf("tick off the hour: %+v", h)
				}
			}
		})
	}
}

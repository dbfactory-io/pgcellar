// Package tztest pins the local time zone of a test binary, so that the suite
// gives the same result on a laptop in Brussels and on a CI runner in UTC, and
// runs a test again under other zones.
//
// time.Local is a global read by every goroutine that calls time.Now (HTTP
// servers of earlier tests included), so it is set once, in TestMain, before
// any test starts. A test that must see another zone runs in a child process
// of the same test binary (InZones).
//
// Only import it from tests: it embeds the time zone database (time/tzdata)
// so that the zones load even on a machine without /usr/share/zoneinfo.
package tztest

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // zones must load on minimal CI images
)

// Fixtures is the zone the Barman fixtures of internal/barman/testdata were
// written in: their dates carry +02:00 and their zone-less strings (ctime
// output, backup ids) are wall-clock times of a server in Brussels.
const Fixtures = "Europe/Brussels"

// envZone names the zone a child process of InZones must run in.
const envZone = "PGCELLAR_TEST_ZONE"

// Main is the TestMain of a package whose tests depend on the local zone:
//
//	func TestMain(m *testing.M) { tztest.Main(m) }
//
// It sets time.Local to Fixtures (or to the zone InZones asked for) and runs
// the tests.
func Main(m *testing.M) {
	zone := os.Getenv(envZone)
	if zone == "" {
		zone = Fixtures
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "time zone %s: %v\n", zone, err)
		os.Exit(2)
	}
	time.Local = loc
	os.Exit(m.Run())
}

// Pinned reports whether Main set time.Local (an unpinned time.Local is
// called "Local").
func Pinned() bool { return time.Local.String() != "Local" }

// Load returns the named zone or fails the test.
func Load(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("time zone %s: %v", name, err)
	}
	return loc
}

// InZones runs body once per zone, each time in a child process of the test
// binary whose time.Local is that zone. Call it from a top-level test of a
// package that uses Main; body must give the same verdict in every zone.
func InZones(t *testing.T, zones []string, body func(t *testing.T)) {
	t.Helper()
	if os.Getenv(envZone) != "" { // the child: run the scenario here
		body(t)
		return
	}
	if strings.Contains(t.Name(), "/") {
		t.Fatal("tztest.InZones must be called from a top-level test")
	}
	for _, zone := range zones {
		t.Run(zone, func(t *testing.T) {
			Load(t, zone)
			cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name()[:strings.IndexByte(t.Name(), '/')])+"$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), envZone+"="+zone, "TZ="+zone)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("in time zone %s: %v\n%s", zone, err, out)
			}
		})
	}
}

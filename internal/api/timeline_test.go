package api

import (
	"os"
	"strings"
	"testing"
)

func TestTimelineEndpoint(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.call(t, "GET", "/servers/pg-main/timeline", "", ""); code != 401 {
		t.Errorf("no key: %d", code)
	}
	before, _ := os.ReadFile(f.env.CallLog)
	code, out := f.call(t, "GET", "/servers/pg-main/timeline", f.env.Keys["viewer"], "")
	if code != 200 {
		t.Fatalf("code %d: %v", code, out)
	}
	if after, _ := os.ReadFile(f.env.CallLog); string(after) != string(before) {
		t.Error("the endpoint must not run Barman")
	}
	d := out["data"].(map[string]any)
	if d["server"] != "pg-main" || d["available"] != true {
		t.Fatalf("data = %v", d)
	}
	if got := len(d["backups"].([]any)); got != 3 {
		t.Errorf("backups: %d", got)
	}
	tot := d["totals"].(map[string]any)
	if tot["backups"] != float64(3) || tot["wal_files"] != float64(7) || tot["wal_bytes"] != float64(7*16777216) || tot["wal_gaps"] != float64(0) {
		t.Errorf("totals = %v", tot)
	}
	if e, _ := d["earliest_restore_point"].(string); !strings.Contains(e, "2026-10-18T02:00:01") || d["earliest_backup"] != "20261018T020001" {
		t.Errorf("earliest = %v (%v)", d["earliest_restore_point"], d["earliest_backup"])
	}
	if d["latest_source"] != "streaming" || !strings.Contains(d["latest_restore_point"].(string), "T02:25:00") {
		t.Errorf("latest = %v (%v)", d["latest_restore_point"], d["latest_source"])
	}
	if d["next_backup"] == nil || d["axis_start"] == nil || d["retention"] == nil {
		t.Errorf("data = %v", d)
	}
	if code, _ := f.call(t, "GET", "/servers/nope/timeline", f.env.Keys["viewer"], ""); code != 404 {
		t.Errorf("unknown server: %d", code)
	}
	// A server whose metadata cannot be read says so instead of failing.
	code, out = f.call(t, "GET", "/servers/pg-rsync/timeline", f.env.Keys["viewer"], "")
	d, _ = out["data"].(map[string]any)
	if code != 200 || d["available"] != false || !strings.HasPrefix(d["unavailable"].(string), "unavailable: ") {
		t.Errorf("pg-rsync: %d %v", code, out)
	}
}

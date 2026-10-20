package api

import (
	"os"
	"strings"
	"testing"
)

func TestSetupEndpoint(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.call(t, "GET", "/servers/pg-main/setup", "", ""); code != 401 {
		t.Errorf("no key: %d", code)
	}
	before, _ := os.ReadFile(f.env.PsqlLog)
	code, out := f.call(t, "GET", "/servers/pg-main/setup", f.env.Keys["viewer"], "")
	if code != 200 {
		t.Fatalf("code %d: %v", code, out)
	}
	if after, _ := os.ReadFile(f.env.PsqlLog); string(after) != string(before) {
		t.Error("the endpoint must not run psql")
	}
	data := out["data"].(map[string]any)
	if data["server"] != "pg-main" || data["level"] != "warning" || data["at"] == nil {
		t.Fatalf("data = %v", data)
	}
	var found map[string]any
	for _, c := range data["checks"].([]any) {
		row := c.(map[string]any)
		if row["key"] == "archive_timeout" {
			found = row
		}
	}
	if found == nil || found["level"] != "warning" || found["restart"] != false || found["value"] != "0 s" ||
		!strings.Contains(found["fix"].(string), "ALTER SYSTEM SET archive_timeout = '1h';") ||
		!strings.Contains(found["why"].(string), "bounded recovery point") {
		t.Errorf("archive_timeout = %v", found)
	}
	if code, _ := f.call(t, "GET", "/servers/nope/setup", f.env.Keys["viewer"], ""); code != 404 {
		t.Errorf("unknown server: %d", code)
	}
	// The state of the server carries the same summary.
	_, srv := f.call(t, "GET", "/servers/pg-main", f.env.Keys["viewer"], "")
	state := srv["data"].(map[string]any)["state"].(map[string]any)
	if state["setup_level"] != "warning" || state["setup_issues"] != float64(1) {
		t.Errorf("state = %v", state)
	}
}

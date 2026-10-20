package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The rule: a key that is absent means the default, and Save never writes a
// default in a form that changes meaning later.

func loadYAML(t *testing.T, body string) (*Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pgcellar.yaml")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

const minimalYAML = `listen: ":8080"
servers:
  - name: pg-main
    schedule:
      backup: "daily 02:00"
alerts:
  mail:
    host: smtp.example.com
    from: a@example.com
    to: [b@example.com]
`

func TestAbsentKeysMeanDefaults(t *testing.T) {
	cfg, _ := loadYAML(t, minimalYAML)
	if !cfg.Drills.IsEnabled() || !cfg.Drills.KeepsLast() {
		t.Error("drills.enabled and drills.keep_last default to true")
	}
	if !cfg.Scheduler.IsEnabled() || cfg.Scheduler.EffectiveCatchUp() != DefaultCatchUp || cfg.Scheduler.Parallel != 1 {
		t.Errorf("scheduler defaults: %+v", cfg.Scheduler)
	}
	if !cfg.Servers[0].Schedule.IsEnabled() {
		t.Error("a schedule with a backup spec and no enabled key is on")
	}
	if (Schedule{}).IsEnabled() {
		t.Error("no backup spec = off")
	}
	if !cfg.Alerts.Mail.UsesStartTLS() {
		t.Error("alerts.mail.starttls defaults to true")
	}
	if cfg.Alerts.EffectiveRepeatEvery() != DefaultRepeatEvery {
		t.Error("alerts.repeat_every defaults to 24h")
	}
	if cfg.Recoveries.EffectiveKeepDays() != 7 || cfg.EffectiveAuditRetentionDays() != 90 {
		t.Error("keep_days 7 and audit_retention_days 90 by default")
	}
	if p := Validate(cfg); len(p) > 0 && strings.Contains(strings.Join(p, " "), "schedule") {
		t.Errorf("unexpected problem: %v", p)
	}
}

func TestSaveDoesNotMaterialiseDefaults(t *testing.T) {
	cfg, path := loadYAML(t, minimalYAML)
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	for _, key := range []string{"enabled:", "keep_last:", "catch_up:", "repeat_every:", "keep_days:", "audit_retention_days:", "starttls:"} {
		if strings.Contains(text, key) {
			t.Errorf("Save wrote %q for a key that was absent:\n%s", key, text)
		}
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Drills.IsEnabled() || !again.Drills.KeepsLast() || !again.Scheduler.IsEnabled() ||
		!again.Servers[0].Schedule.IsEnabled() || !again.Alerts.Mail.UsesStartTLS() ||
		again.Scheduler.EffectiveCatchUp() != DefaultCatchUp || again.Alerts.EffectiveRepeatEvery() != DefaultRepeatEvery ||
		again.Recoveries.EffectiveKeepDays() != 7 || again.EffectiveAuditRetentionDays() != 90 {
		t.Errorf("Save then Load changed a meaning: %+v", again)
	}
}

func TestExplicitValuesAreRespected(t *testing.T) {
	body := minimalYAML + `scheduler:
  enabled: false
  catch_up: 0s
drills:
  enabled: false
  keep_last: false
recoveries:
  keep_days: 0
audit_retention_days: 0
`
	body = strings.Replace(body, "alerts:\n", "alerts:\n  repeat_every: 0s\n", 1)
	body = strings.Replace(body, "      backup: \"daily 02:00\"\n", "      backup: \"daily 02:00\"\n      enabled: false\n", 1)
	body = strings.Replace(body, "    host: smtp.example.com\n", "    host: smtp.example.com\n    starttls: false\n", 1)
	check := func(label string, c *Config) {
		if c.Drills.IsEnabled() || c.Drills.KeepsLast() || c.Scheduler.IsEnabled() || c.Servers[0].Schedule.IsEnabled() || c.Alerts.Mail.UsesStartTLS() {
			t.Errorf("%s: an explicit false was lost", label)
		}
		if c.Scheduler.EffectiveCatchUp() != 0 || c.Alerts.EffectiveRepeatEvery() != 0 || c.Recoveries.EffectiveKeepDays() != 0 || c.EffectiveAuditRetentionDays() != 0 {
			t.Errorf("%s: an explicit 0 was lost: catch_up=%v repeat=%v keep_days=%d audit=%d", label,
				c.Scheduler.EffectiveCatchUp(), c.Alerts.EffectiveRepeatEvery(), c.Recoveries.EffectiveKeepDays(), c.EffectiveAuditRetentionDays())
		}
	}
	cfg, path := loadYAML(t, body)
	check("load", cfg)
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	check("save+load", again)
	if (time.Duration(0)) != again.Scheduler.EffectiveCatchUp() {
		t.Error("catch_up 0 must stay 0")
	}
}

func TestJSONExposesResolvedValues(t *testing.T) {
	cfg, _ := loadYAML(t, minimalYAML)
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["drills"].(map[string]any)["enabled"] != true || m["drills"].(map[string]any)["keep_last"] != true {
		t.Errorf("drills JSON: %v", m["drills"])
	}
	if m["scheduler"].(map[string]any)["enabled"] != true {
		t.Errorf("scheduler JSON: %v", m["scheduler"])
	}
	srv := m["servers"].([]any)[0].(map[string]any)
	if srv["schedule"].(map[string]any)["enabled"] != true {
		t.Errorf("schedule JSON: %v", srv["schedule"])
	}
	if m["audit_retention_days"] != float64(90) || m["recoveries"].(map[string]any)["keep_days"] != float64(7) {
		t.Errorf("numeric JSON: %v %v", m["audit_retention_days"], m["recoveries"])
	}
	var in Schedule
	if err := json.Unmarshal([]byte(`{"backup":"hourly","enabled":false}`), &in); err != nil || in.IsEnabled() {
		t.Errorf("JSON input enabled=false: %+v %v", in, err)
	}
	if err := json.Unmarshal([]byte(`{"backup":"hourly"}`), &in); err != nil {
		t.Fatal(err)
	}
}

func TestDrillsDisabledWarning(t *testing.T) {
	cfg, _ := loadYAML(t, minimalYAML+"drills:\n  enabled: false\n")
	w := Warnings(cfg)
	if len(w) != 1 || !strings.Contains(w[0], "drills.enabled is false; delete the line to get the default") {
		t.Fatalf("warnings: %v", w)
	}
	if p := Validate(cfg); containsAny(p, "drills.enabled") {
		t.Errorf("a warning is not a validation problem: %v", p)
	}
	cfg.Servers[0].Drills.Schedule = "weekly sun 04:30"
	if w := Warnings(cfg); len(w) != 0 {
		t.Errorf("a drill schedule means a deliberate choice: %v", w)
	}
	plain, _ := loadYAML(t, minimalYAML)
	if w := Warnings(plain); len(w) != 0 {
		t.Errorf("absent key: %v", w)
	}
}

func containsAny(list []string, s string) bool {
	for _, x := range list {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

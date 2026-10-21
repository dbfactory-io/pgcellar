package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestUpdatesDefaults(t *testing.T) {
	cfg, path := loadYAML(t, minimalYAML)
	u := cfg.Updates
	if !u.IsCheckEnabled() || u.IsAuto() || u.EffectiveInterval() != 24*time.Hour || u.EffectiveChannel() != "stable" || u.EffectiveWindow() != "02:00-05:00" || u.EffectiveSource() != DefaultUpdatesSource {
		t.Fatalf("defaults: %+v", u)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "updates:") {
		t.Errorf("Save wrote an updates block that was absent:\n%s", raw)
	}
}

func TestUpdatesValues(t *testing.T) {
	cfg, _ := loadYAML(t, minimalYAML+`updates:
  check: false
  interval: 6h
  auto: true
  window: "23:30-01:00"
  source: https://mirror.example.com/latest
`)
	u := cfg.Updates
	if u.IsCheckEnabled() || !u.IsAuto() || u.EffectiveInterval() != 6*time.Hour || u.ParsedWindow().String() != "23:30-01:00" || u.EffectiveSource() != "https://mirror.example.com/latest" {
		t.Fatalf("values: %+v", u)
	}
	for _, p := range Validate(cfg) {
		if strings.Contains(p, "updates") {
			t.Errorf("unexpected problem: %s", p)
		}
	}
}

func TestUpdatesValidation(t *testing.T) {
	cfg, _ := loadYAML(t, minimalYAML+`updates:
  interval: 30m
  channel: beta
  window: "25:00-03:00"
  source: ftp://example.com/x
`)
	got := strings.Join(Validate(cfg), "\n")
	for _, want := range []string{"updates.interval must be at least 1h", `updates.channel "beta"`, "updates.window", "updates.source"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 21, h, m, 0, 0, time.Local) }
	w, err := ParseWindow("02:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(1, 59), false}, {at(2, 0), true}, {at(4, 59), true}, {at(5, 0), false}, {at(14, 0), false}} {
		if got := w.Contains(c.t); got != c.want {
			t.Errorf("02:00-05:00 contains %s = %v", c.t.Format("15:04"), got)
		}
	}
	night, err := ParseWindow(" 23:00-01:30 ")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(22, 59), false}, {at(23, 0), true}, {at(23, 59), true}, {at(0, 0), true}, {at(1, 29), true}, {at(1, 30), false}, {at(12, 0), false}} {
		if got := night.Contains(c.t); got != c.want {
			t.Errorf("23:00-01:30 contains %s = %v", c.t.Format("15:04"), got)
		}
	}
	for _, bad := range []string{"", "02:00", "02:00-02:00", "2-5", "02:00-24:00", "aa:bb-cc:dd"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) should fail", bad)
		}
	}
}

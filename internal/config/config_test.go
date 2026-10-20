package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// bcrypt at the production cost makes every test that hashes a password slow.
func TestMain(m *testing.M) {
	BcryptCost = 4
	os.Exit(m.Run())
}

func sampleConfig(t *testing.T) *Config {
	t.Helper()
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.BaseURL = "https://cellar.example.com"
	cfg.APIKeys = []APIKey{} // YAML loads an empty list as non-nil
	cfg.Servers = []Server{{
		Name: "pg-main", Label: "Main", Tags: []string{"prod"},
		Schedule:  Schedule{Backup: "daily 02:00", Enabled: true},
		Retention: Retention{RecoveryWindowDays: 7, MinimumRedundancy: 2},
	}}
	cfg.Users = []User{{Name: "gilles", Role: "admin", PasswordHash: hash}}
	cfg.Alerts.MaxBackupAge = 26 * time.Hour
	cfg.Alerts.MaxWALAge = time.Hour
	return cfg
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgcellar.yaml")
	cfg := sampleConfig(t)
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# pgcellar.yaml") {
		t.Errorf("missing header: %q", raw[:30])
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, got) {
		t.Errorf("round trip mismatch:\nwant %+v\ngot  %+v", cfg, got)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".pgcellar-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("base_url: http://x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Barman.Binary != "barman" || cfg.Barman.ConfigDir != "/etc/barman.d" ||
		cfg.Barman.User != "barman" || cfg.DataDir != "/var/lib/pgcellar" || cfg.Barman.ConfigFile != "/etc/barman.conf" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestValidate(t *testing.T) {
	if p := Validate(sampleConfig(t)); len(p) != 0 {
		t.Fatalf("valid config reported problems: %v", p)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"dup server", func(c *Config) { c.Servers = append(c.Servers, c.Servers[0]) }, "more than once"},
		{"bad name", func(c *Config) { c.Servers[0].Name = "Bad Name" }, "invalid"},
		{"bad role", func(c *Config) { c.Users[0].Role = "root" }, "must be admin"},
		{"no admin", func(c *Config) { c.Users[0].Role = "viewer" }, "at least one user with the admin"},
		{"bad schedule", func(c *Config) { c.Servers[0].Schedule.Backup = "whenever" }, "cannot understand"},
		{"neg duration", func(c *Config) { c.Alerts.MaxWALAge = -time.Hour }, "max_wal_age"},
		{"both retention", func(c *Config) { c.Servers[0].Retention.Redundancy = 3 }, "not both"},
		{"min > red", func(c *Config) {
			c.Servers[0].Retention = Retention{Redundancy: 2, MinimumRedundancy: 5}
		}, "higher than redundancy"},
		{"bad api role", func(c *Config) { c.APIKeys = []APIKey{{Name: "k", Role: "x", KeyHash: "h"}} }, "api key"},
		{"drill schedule", func(c *Config) { c.Servers[0].Drills.Schedule = "nope" }, "drills.schedule"},
		{"drill timeout", func(c *Config) { c.Drills.Timeout = 0 }, "drills.timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sampleConfig(t)
			tc.mutate(cfg)
			p := Validate(cfg)
			if !strings.Contains(strings.Join(p, "\n"), tc.want) {
				t.Errorf("want a problem containing %q, got %v", tc.want, p)
			}
		})
	}
}

func TestParseSchedule(t *testing.T) {
	ok := map[string]string{
		"daily 02:00":          "0 2 * * *",
		"Daily 2:05":           "5 2 * * *",
		"weekly sun 03:30":     "30 3 * * 0",
		"  weekly   MON 23:59": "59 23 * * 1",
		"hourly":               "0 * * * *",
		"every 6 hours":        "0 */6 * * *",
		"every 1 hour":         "0 * * * *",
		"*/15 1-5 * * 1,3":     "*/15 1-5 * * 1,3",
	}
	for in, want := range ok {
		got, human, err := ParseSchedule(in)
		if err != nil || got != want || human == "" {
			t.Errorf("ParseSchedule(%q) = %q, %q, %v; want %q", in, got, human, err, want)
		}
	}
	for _, in := range []string{"", "daily 25:00", "daily 02:61", "weekly xyz 03:00", "every 0 hours", "every 24 hours", "61 * * * *", "* * * *", "later"} {
		if _, _, err := ParseSchedule(in); err == nil {
			t.Errorf("ParseSchedule(%q) should fail", in)
		}
	}
}

func TestRenderCrontab(t *testing.T) {
	cfg := sampleConfig(t)
	cfg.Servers = append(cfg.Servers,
		Server{Name: "off", Schedule: Schedule{Backup: "hourly", Enabled: false}},
		Server{Name: "wk", Schedule: Schedule{Backup: "weekly sun 03:00", Enabled: true}})
	out := RenderCrontab(cfg)
	for _, want := range []string{"* * * * * barman barman cron\n", "0 2 * * * barman barman backup pg-main\n", "0 3 * * 0 barman barman backup wk\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("crontab missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "backup off") {
		t.Error("disabled server must not be scheduled")
	}
	if strings.Count(out, " cron\n") != 1 {
		t.Error("barman cron must appear once")
	}
}

func TestRetention(t *testing.T) {
	if got := RetentionPolicy(Retention{RecoveryWindowDays: 7}); got != "RECOVERY WINDOW OF 7 DAYS" {
		t.Error(got)
	}
	if got := RetentionPolicy(Retention{RecoveryWindowDays: 1}); got != "RECOVERY WINDOW OF 1 DAY" {
		t.Error(got)
	}
	if got := RetentionPolicy(Retention{Redundancy: 3}); got != "REDUNDANCY 3" {
		t.Error(got)
	}
	if RetentionPolicy(Retention{}) != "" || MinimumRedundancy(Retention{}) != "" || MinimumRedundancy(Retention{MinimumRedundancy: 2}) != "2" {
		t.Error("empty retention should render empty")
	}
	cases := map[string]Retention{
		"recovery window of 7 days":     {RecoveryWindowDays: 7},
		"  Recovery  Window Of 2 WEEKS": {RecoveryWindowDays: 14},
		"RECOVERY WINDOW OF 1 MONTH":    {RecoveryWindowDays: 30},
		"redundancy   3":                {Redundancy: 3},
		"":                              {},
	}
	for in, want := range cases {
		got, err := ParseRetentionPolicy(in)
		if err != nil || got != want {
			t.Errorf("ParseRetentionPolicy(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := ParseRetentionPolicy("keep everything"); err == nil {
		t.Error("expected error")
	}
	r, err := BuildRetention("REDUNDANCY 4", " 2 ")
	if err != nil || r.Redundancy != 4 || r.MinimumRedundancy != 2 {
		t.Errorf("BuildRetention = %+v, %v", r, err)
	}
	if _, err := BuildRetention("", "abc"); err == nil {
		t.Error("expected error for bad minimum_redundancy")
	}
}

func TestPasswords(t *testing.T) {
	h, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "hunter2") || CheckPassword(h, "hunter3") || CheckPassword("garbage", "hunter2") {
		t.Error("password check misbehaves")
	}
}

func TestAPIKeys(t *testing.T) {
	clear, hash := NewAPIKey()
	clear2, _ := NewAPIKey()
	if !strings.HasPrefix(clear, "pgc_") || len(hash) != 64 || clear == clear2 {
		t.Fatalf("bad key %q / %q", clear, hash)
	}
	cfg := &Config{APIKeys: []APIKey{{Name: "ci", Role: "operator", KeyHash: hash}}}
	k, ok := MatchAPIKey(cfg, clear)
	if !ok || k.Name != "ci" {
		t.Error("key should match")
	}
	if _, ok := MatchAPIKey(cfg, clear2); ok {
		t.Error("other key must not match")
	}
	if _, ok := MatchAPIKey(cfg, ""); ok {
		t.Error("empty key must not match")
	}
}

func TestExampleDecodesStrictly(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pgcellar.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		t.Fatalf("example file has unknown or invalid keys: %v", err)
	}
	if cfg.Barman.CronInterval != time.Minute || cfg.AuditRetentionDays != 90 {
		t.Errorf("example values: cron_interval=%v audit_retention_days=%d", cfg.Barman.CronInterval, cfg.AuditRetentionDays)
	}
}

func TestAuditAndCronDefaults(t *testing.T) {
	d := Defaults()
	if d.Barman.CronInterval != time.Minute || d.AuditRetentionDays != 90 {
		t.Errorf("defaults: %v %d", d.Barman.CronInterval, d.AuditRetentionDays)
	}
	d.AuditRetentionDays = -1
	if len(Validate(d)) == 0 {
		t.Error("negative audit_retention_days should be a problem")
	}
}

func TestStatusInterval(t *testing.T) {
	d := Defaults()
	if d.StatusInterval != time.Minute {
		t.Errorf("default status_interval = %v", d.StatusInterval)
	}
	for _, c := range []struct {
		d  time.Duration
		ok bool
	}{{0, true}, {30 * time.Second, true}, {5 * time.Minute, true}, {10 * time.Second, false}, {-time.Minute, false}} {
		d.StatusInterval = c.d
		bad := false
		for _, p := range Validate(d) {
			bad = bad || strings.Contains(p, "status_interval")
		}
		if bad == c.ok {
			t.Errorf("status_interval %v: problem reported = %v", c.d, bad)
		}
	}
}

func TestSetupInterval(t *testing.T) {
	d := Defaults()
	if d.SetupInterval != 30*time.Minute {
		t.Errorf("default setup_interval = %v", d.SetupInterval)
	}
	for _, c := range []struct {
		d  time.Duration
		ok bool
	}{{0, true}, {time.Minute, true}, {2 * time.Hour, true}, {10 * time.Second, false}, {-time.Minute, false}} {
		d.SetupInterval = c.d
		bad := false
		for _, p := range Validate(d) {
			bad = bad || strings.Contains(p, "setup_interval")
		}
		if bad == c.ok {
			t.Errorf("setup_interval %v: problem reported = %v", c.d, bad)
		}
	}
}

func TestRecoveriesDefaultsAndValidation(t *testing.T) {
	d := Defaults().Recoveries
	if d.Dir != "/var/lib/pgcellar/recoveries" || d.MaxDownloadBytes != 2<<30 || d.KeepDays != 7 {
		t.Fatalf("defaults: %+v", d)
	}
	path := filepath.Join(t.TempDir(), "pgcellar.yaml")
	if err := os.WriteFile(path, []byte("recoveries:\n  dir: /srv/restores\n  keep_days: 0\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Recoveries.Dir != "/srv/restores" || cfg.Recoveries.KeepDays != 0 || cfg.Recoveries.MaxDownloadBytes != 2<<30 {
		t.Fatalf("partial section keeps the other defaults: %+v", cfg.Recoveries)
	}
	for name, mod := range map[string]func(*Config){
		"relative": func(c *Config) { c.Recoveries.Dir = "restores" },
		"empty":    func(c *Config) { c.Recoveries.Dir = "" },
		"root":     func(c *Config) { c.Recoveries.Dir = "/" },
		"neg size": func(c *Config) { c.Recoveries.MaxDownloadBytes = -1 },
		"neg days": func(c *Config) { c.Recoveries.KeepDays = -1 },
	} {
		c := sampleConfig(t)
		mod(c)
		if p := Validate(c); len(p) != 1 || !strings.Contains(p[0], "recoveries.") {
			t.Errorf("%s: %v", name, p)
		}
	}
	if p := Validate(sampleConfig(t)); len(p) != 0 {
		t.Errorf("sample config: %v", p)
	}
}

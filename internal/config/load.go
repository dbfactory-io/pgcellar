package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MinStatusInterval is the shortest allowed status_interval: every refresh
// starts several Python processes per server.
const MinStatusInterval = 30 * time.Second

// DefaultSetupInterval is setup_interval when it is not set; MinSetupInterval
// is the shortest allowed value.
const (
	DefaultSetupInterval = 30 * time.Minute
	MinSetupInterval     = time.Minute
)

// DefaultMinFreePercent is alerts.min_free_percent when the file does not say.
const DefaultMinFreePercent = 10

// ParseClock reads "08:30" as hour and minute.
func ParseClock(s string) (hour, minute int, ok bool) {
	var h, m int
	if n, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || n != 2 || h < 0 || h > 23 || m < 0 || m > 59 || len(strings.TrimSpace(s)) > 5 {
		return 0, 0, false
	}
	return h, m, true
}

// DefaultRecoveriesDir is where restores go when recoveries.dir is not set.
const DefaultRecoveriesDir = "/var/lib/pgcellar/recoveries"

func Defaults() *Config {
	return &Config{
		Listen:  ":8080",
		DataDir: "/var/lib/pgcellar",
		Barman: Barman{
			Binary:       "barman",
			ConfigFile:   "/etc/barman.conf",
			ConfigDir:    "/etc/barman.d",
			User:         "barman",
			CronInterval: time.Minute,
		},
		// Settings where the zero value means something else than the default
		// (enabled, keep_last, catch_up, repeat_every, keep_days,
		// audit_retention_days) are left nil on purpose: nil = the key is
		// absent = the default, resolved by the accessors. Save then never
		// writes them unless somebody chose a value.
		Drills: Drills{Timeout: DefaultDrillTimeout, RecoveryTimeout: DefaultDrillRecoveryTimeout,
			MaxAge: DefaultDrillMaxAge, BinDir: DefaultPGBinDir},
		Scheduler:      Scheduler{Parallel: 1},
		StatusInterval: time.Minute,
		Alerts:         Alerts{MinFreePercent: DefaultMinFreePercent},
		SetupInterval:  DefaultSetupInterval,
		Recoveries: Recoveries{
			Dir:              DefaultRecoveriesDir,
			MaxDownloadBytes: 2 << 30,
		},
	}
}

// Load reads pgcellar.yaml on top of the defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

const header = `# pgcellar.yaml - the source of truth for everything pgCellar owns.
# The web interface and the API only edit this file; you can edit it by hand too.
# Every change made through pgCellar is recorded in this directory's git history.
# Never put secrets here: passwords and API keys are stored as hashes, and
# alert credentials are referenced by environment variable name.

`

// Save writes the configuration atomically (temp file + rename, mode 0640).
func Save(path string, cfg *Config) error {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pgcellar-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(header + string(body)); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Warnings returns what is worth a look in a valid configuration but does not
// stop pgCellar: shown by `pgcellar check` and written to the journal at
// startup.
func Warnings(cfg *Config) []string {
	var w []string
	if cfg.Drills.Enabled != nil && !*cfg.Drills.Enabled {
		scheduled := false
		for _, s := range cfg.Servers {
			if strings.TrimSpace(s.Drills.Schedule) != "" {
				scheduled = true
			}
		}
		if !scheduled {
			w = append(w, "drills.enabled is false; delete the line to get the default (on). Older versions wrote it by themselves, and it switches off every scheduled restore drill")
		}
	}
	return w
}

var serverNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

var validRoles = map[string]bool{"admin": true, "operator": true, "viewer": true}

// Validate returns one clear sentence per problem; empty means valid.
func Validate(cfg *Config) []string { return ValidateWith(cfg, false) }

// ValidateWith is Validate; allowNoAdmin accepts a configuration without any
// administrator, which only the public demo (a single read-only user) uses.
func ValidateWith(cfg *Config, allowNoAdmin bool) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	if strings.TrimSpace(cfg.Listen) == "" {
		add("listen address is empty (for example \":8080\")")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		add("data_dir is empty")
	}
	if cfg.Barman.Binary == "" {
		add("barman.binary is empty")
	}
	if cfg.Barman.CronInterval < 0 {
		add("barman.cron_interval cannot be negative (0 disables it)")
	}
	if cfg.StatusInterval != 0 && cfg.StatusInterval < MinStatusInterval {
		add("status_interval must be at least %s (for example \"1m\" or \"5m\"); leave it out for the default", MinStatusInterval)
	}
	if cfg.SetupInterval != 0 && cfg.SetupInterval < MinSetupInterval {
		add("setup_interval must be at least %s (for example \"30m\"); leave it out for the default", MinSetupInterval)
	}
	if c := cfg.Scheduler.CatchUp; c != nil && *c < 0 {
		add("scheduler.catch_up cannot be negative (0 disables the catch-up)")
	}
	if cfg.Scheduler.Parallel < 1 {
		add("scheduler.parallel must be at least 1")
	}
	if cfg.EffectiveAuditRetentionDays() < 0 {
		add("audit_retention_days cannot be negative (0 keeps everything)")
	}
	if strings.TrimSpace(cfg.Recoveries.Dir) == "" || !filepath.IsAbs(cfg.Recoveries.Dir) {
		add("recoveries.dir must be an absolute path (default %s)", DefaultRecoveriesDir)
	} else if filepath.Clean(cfg.Recoveries.Dir) == string(filepath.Separator) {
		add("recoveries.dir cannot be the root directory")
	}
	if cfg.Recoveries.MaxDownloadBytes < 0 {
		add("recoveries.max_download_bytes cannot be negative (0 means no limit)")
	}
	if cfg.Recoveries.EffectiveKeepDays() < 0 {
		add("recoveries.keep_days cannot be negative (0 keeps everything)")
	}

	seen := map[string]bool{}
	for i, s := range cfg.Servers {
		switch {
		case s.Name == "":
			add("server #%d has no name", i+1)
			continue
		case !serverNameRe.MatchString(s.Name):
			add("server name %q is invalid: use lowercase letters, digits, - and _ only", s.Name)
		}
		if seen[s.Name] {
			add("server name %q is used more than once", s.Name)
		}
		seen[s.Name] = true
		if s.Schedule.IsEnabled() {
			if _, _, err := ParseSchedule(s.Schedule.Backup); err != nil {
				add("server %q: %v", s.Name, err)
			}
		}
		r := s.Retention
		if r.RecoveryWindowDays < 0 || r.Redundancy < 0 || r.MinimumRedundancy < 0 {
			add("server %q: retention values cannot be negative", s.Name)
		}
		if r.RecoveryWindowDays > 0 && r.Redundancy > 0 {
			add("server %q: set either recovery_window_days or redundancy, not both", s.Name)
		}
		if r.Redundancy > 0 && r.MinimumRedundancy > r.Redundancy {
			add("server %q: minimum_redundancy (%d) is higher than redundancy (%d)", s.Name, r.MinimumRedundancy, r.Redundancy)
		}
	}

	admins := 0
	users := map[string]bool{}
	for i, u := range cfg.Users {
		if u.Name == "" {
			add("user #%d has no name", i+1)
			continue
		}
		if users[u.Name] {
			add("user %q is defined more than once", u.Name)
		}
		users[u.Name] = true
		if !validRoles[u.Role] {
			add("user %q has role %q: must be admin, operator or viewer", u.Name, u.Role)
		}
		if u.Role == "admin" {
			admins++
		}
		if u.PasswordHash == "" {
			add("user %q has no password_hash", u.Name)
		}
	}
	if admins == 0 && !allowNoAdmin {
		add("there must be at least one user with the admin role")
	}

	keys := map[string]bool{}
	for i, k := range cfg.APIKeys {
		if k.Name == "" {
			add("api key #%d has no name", i+1)
			continue
		}
		if keys[k.Name] {
			add("api key %q is defined more than once", k.Name)
		}
		keys[k.Name] = true
		if !validRoles[k.Role] {
			add("api key %q has role %q: must be admin, operator or viewer", k.Name, k.Role)
		}
		if k.KeyHash == "" {
			add("api key %q has no key_hash", k.Name)
		}
	}

	if cfg.Alerts.MaxBackupAge < 0 {
		add("alerts.max_backup_age cannot be negative")
	}
	if cfg.Alerts.MaxWALAge < 0 {
		add("alerts.max_wal_age cannot be negative")
	}
	if cfg.Alerts.MinFreePercent < 0 || cfg.Alerts.MinFreePercent > 100 {
		add("alerts.min_free_percent must be between 0 and 100")
	}
	if cfg.Alerts.MinFreeBytes < 0 {
		add("alerts.min_free_bytes cannot be negative")
	}
	if m := cfg.Alerts.Mail; m != nil {
		if m.Host == "" || m.From == "" || len(m.To) == 0 {
			add("alerts.mail needs host, from and at least one recipient in to")
		}
		if m.Port < 0 || m.Port > 65535 {
			add("alerts.mail.port %d is not a valid port", m.Port)
		}
	}
	if cfg.Alerts.EffectiveRepeatEvery() < 0 {
		add("alerts.repeat_every cannot be negative")
	}
	if d := strings.TrimSpace(cfg.Alerts.DailySummary); d != "" {
		if _, _, ok := ParseClock(d); !ok {
			add("alerts.daily_summary %q is not a time of day (use \"08:30\", or leave it empty for no summary)", d)
		}
	}
	if t := cfg.Alerts.Telegram; t != nil && (t.TokenEnv == "" || t.ChatID == "") {
		add("alerts.telegram needs token_env and chat_id")
	}
	if w := cfg.Alerts.Webhook; w != nil && !strings.HasPrefix(w.URL, "http://") && !strings.HasPrefix(w.URL, "https://") {
		add("alerts.webhook.url must start with http:// or https://")
	}

	p = append(p, validateUpdates(cfg.Updates)...)

	if cfg.Drills.Timeout <= 0 {
		add("drills.timeout must be positive (for example \"30m\")")
	}
	if cfg.Drills.RecoveryTimeout <= 0 {
		add("drills.recovery_timeout must be positive (for example \"10m\")")
	}
	if cfg.Drills.MaxAge <= 0 {
		add("drills.max_age must be positive (for example \"192h\" for 8 days)")
	}
	if !filepath.IsAbs(cfg.Drills.BinDir) {
		add("drills.bin_dir must be an absolute path (default %s)", DefaultPGBinDir)
	}
	for _, s := range cfg.Servers {
		if strings.TrimSpace(s.Drills.Schedule) != "" {
			if _, _, err := ParseSchedule(s.Drills.Schedule); err != nil {
				add("server %s: drills.schedule: %v", s.Name, err)
			}
		}
	}
	return p
}

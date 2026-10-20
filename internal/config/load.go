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
		Drills: Drills{Enabled: true, Timeout: DefaultDrillTimeout, RecoveryTimeout: DefaultDrillRecoveryTimeout,
			KeepLast: true, MaxAge: DefaultDrillMaxAge, BinDir: DefaultPGBinDir},
		Scheduler:          Scheduler{Enabled: true, CatchUp: DefaultCatchUp, Parallel: 1},
		StatusInterval:     time.Minute,
		SetupInterval:      DefaultSetupInterval,
		AuditRetentionDays: 90,
		Recoveries: Recoveries{
			Dir:              DefaultRecoveriesDir,
			MaxDownloadBytes: 2 << 30,
			KeepDays:         7,
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

var serverNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

var validRoles = map[string]bool{"admin": true, "operator": true, "viewer": true}

// Validate returns one clear sentence per problem; empty means valid.
func Validate(cfg *Config) []string {
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
	if cfg.Scheduler.CatchUp < 0 {
		add("scheduler.catch_up cannot be negative (0 disables the catch-up)")
	}
	if cfg.Scheduler.Parallel < 1 {
		add("scheduler.parallel must be at least 1")
	}
	if cfg.AuditRetentionDays < 0 {
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
	if cfg.Recoveries.KeepDays < 0 {
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
		if s.Schedule.Enabled {
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
	if admins == 0 {
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
	if m := cfg.Alerts.Mail; m != nil {
		if m.SMTPHost == "" || m.From == "" || len(m.To) == 0 {
			add("alerts.mail needs smtp_host, from and at least one recipient in to")
		}
		if m.SMTPPort < 0 || m.SMTPPort > 65535 {
			add("alerts.mail.smtp_port %d is not a valid port", m.SMTPPort)
		}
	}
	if t := cfg.Alerts.Telegram; t != nil && (t.TokenEnv == "" || t.ChatID == "") {
		add("alerts.telegram needs token_env and chat_id")
	}
	if w := cfg.Alerts.Webhook; w != nil && !strings.HasPrefix(w.URL, "http://") && !strings.HasPrefix(w.URL, "https://") {
		add("alerts.webhook.url must start with http:// or https://")
	}

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

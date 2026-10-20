// Package config owns pgcellar.yaml, the only file that belongs to pgCellar
// (everything about Barman stays in Barman's own files, see internal/barman),
// and the local git history of the configuration directory.
package config

import "time"

// Config is the whole pgcellar.yaml. Keep it boring: strings, lists, maps.
// A sysadmin, an automation agent or git must be able to edit it by hand.
type Config struct {
	Listen  string   `yaml:"listen"`   // ":8080"
	BaseURL string   `yaml:"base_url"` // "https://cellar.example.com", used in links and alerts
	DataDir string   `yaml:"data_dir"` // SQLite and runtime files, e.g. /var/lib/pgcellar
	Barman  Barman   `yaml:"barman"`
	Servers []Server `yaml:"servers"` // what pgCellar adds on top of barman.d, keyed by name
	Alerts  Alerts   `yaml:"alerts"`
	Users   []User   `yaml:"users"`
	APIKeys []APIKey `yaml:"api_keys"`
	Drills  Drills   `yaml:"drills"`
}

// Barman says where Barman lives and how to call it.
type Barman struct {
	Binary     string `yaml:"binary"`      // "barman" (on PATH) or an absolute path
	ConfigFile string `yaml:"config_file"` // /etc/barman.conf
	ConfigDir  string `yaml:"config_dir"`  // /etc/barman.d
	User       string `yaml:"user"`        // "barman": commands run as this user when pgCellar runs as root
	Host       string `yaml:"host"`        // empty = local; "user@host" = run over SSH (v2)
}

// Server holds the pgCellar side of a server: schedule, retention, notes.
// The name must match a [section] in barman.d.
type Server struct {
	Name      string    `yaml:"name"`
	Label     string    `yaml:"label"` // free text shown in the UI
	Schedule  Schedule  `yaml:"schedule"`
	Retention Retention `yaml:"retention"`
	Tags      []string  `yaml:"tags"`
}

// Schedule is human-readable and rendered into a crontab line for `barman cron`
// plus one `barman backup` entry.
type Schedule struct {
	Backup  string `yaml:"backup"` // "daily 02:00", "weekly sun 03:00", or a raw cron expression
	Enabled bool   `yaml:"enabled"`
}

// Retention mirrors Barman's retention_policy and minimum_redundancy.
type Retention struct {
	RecoveryWindowDays int `yaml:"recovery_window_days"` // 0 = not set
	Redundancy         int `yaml:"redundancy"`           // number of backups to keep, 0 = not set
	MinimumRedundancy  int `yaml:"minimum_redundancy"`
}

// Alerts lists the channels. Empty = silent.
type Alerts struct {
	Mail     *MailAlert     `yaml:"mail,omitempty"`
	Telegram *TelegramAlert `yaml:"telegram,omitempty"`
	Webhook  *WebhookAlert  `yaml:"webhook,omitempty"`
	// MaxBackupAge: alert when the last successful backup is older than this.
	MaxBackupAge time.Duration `yaml:"max_backup_age"` // e.g. 26h
	// MaxWALAge: alert when the last archived WAL is older than this.
	MaxWALAge time.Duration `yaml:"max_wal_age"` // e.g. 1h
}

type MailAlert struct {
	SMTPHost    string   `yaml:"smtp_host"`
	SMTPPort    int      `yaml:"smtp_port"`
	From        string   `yaml:"from"`
	To          []string `yaml:"to"`
	UserEnv     string   `yaml:"user_env"`     // name of the env var holding the SMTP user
	PasswordEnv string   `yaml:"password_env"` // name of the env var holding the SMTP password
}

type TelegramAlert struct {
	TokenEnv string `yaml:"token_env"` // env var name, never the token itself
	ChatID   string `yaml:"chat_id"`
}

type WebhookAlert struct {
	URL string `yaml:"url"`
}

// User is a web user. Password hashes are bcrypt. Roles: admin, operator, viewer.
type User struct {
	Name         string `yaml:"name"`
	Role         string `yaml:"role"`
	PasswordHash string `yaml:"password_hash"`
}

// APIKey identifies an API client. Only the SHA-256 of the key is stored.
type APIKey struct {
	Name    string `yaml:"name"`
	Role    string `yaml:"role"`
	KeyHash string `yaml:"key_hash"`
}

// Drills configures the automatic restore drills (milestone 5).
type Drills struct {
	Enabled   bool   `yaml:"enabled"`
	Schedule  string `yaml:"schedule"` // "weekly sun 05:00"
	Image     string `yaml:"image"`    // postgres:16 for the throwaway container
	ReportDir string `yaml:"report_dir"`
}

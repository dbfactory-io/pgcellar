// Package config owns pgcellar.yaml, the only file that belongs to pgCellar
// (everything about Barman stays in Barman's own files, see internal/barman),
// and the local git history of the configuration directory.
package config

import "time"

// Config is the whole pgcellar.yaml. Keep it boring: strings, lists, maps.
// A sysadmin, an automation agent or git must be able to edit it by hand.
type Config struct {
	Listen  string   `yaml:"listen" json:"listen"`     // ":8080"
	BaseURL string   `yaml:"base_url" json:"base_url"` // "https://cellar.example.com", used in links and alerts
	DataDir string   `yaml:"data_dir" json:"data_dir"` // SQLite and runtime files, e.g. /var/lib/pgcellar
	Barman  Barman   `yaml:"barman" json:"barman"`
	Servers []Server `yaml:"servers" json:"servers"` // what pgCellar adds on top of barman.d, keyed by name
	Alerts  Alerts   `yaml:"alerts" json:"alerts"`
	Users   []User   `yaml:"users" json:"users"`
	APIKeys []APIKey `yaml:"api_keys" json:"api_keys"`
	Drills  Drills   `yaml:"drills" json:"drills"`
	// AuditRetentionDays: audit lines older than this are purged daily. 0 = keep forever.
	AuditRetentionDays int `yaml:"audit_retention_days" json:"audit_retention_days"`
}

// Barman says where Barman lives and how to call it.
type Barman struct {
	Binary     string `yaml:"binary" json:"binary"`           // "barman" (on PATH) or an absolute path
	ConfigFile string `yaml:"config_file" json:"config_file"` // /etc/barman.conf
	ConfigDir  string `yaml:"config_dir" json:"config_dir"`   // /etc/barman.d
	User       string `yaml:"user" json:"user"`               // "barman": commands run as this user when pgCellar runs as root
	Host       string `yaml:"host" json:"host"`               // empty = local; "user@host" = run over SSH (v2)
	// CronInterval is how often pgCellar runs `barman cron`. 0 = never.
	CronInterval time.Duration `yaml:"cron_interval" json:"cron_interval"`
}

// Server holds the pgCellar side of a server: schedule, retention, notes.
// The name must match a [section] in barman.d.
type Server struct {
	Name      string    `yaml:"name" json:"name"`
	Label     string    `yaml:"label" json:"label"` // free text shown in the UI
	Schedule  Schedule  `yaml:"schedule" json:"schedule"`
	Retention Retention `yaml:"retention" json:"retention"`
	Tags      []string  `yaml:"tags" json:"tags"`
}

// Schedule is human-readable and rendered into a crontab line for `barman cron`
// plus one `barman backup` entry.
type Schedule struct {
	Backup  string `yaml:"backup" json:"backup"` // "daily 02:00", "weekly sun 03:00", or a raw cron expression
	Enabled bool   `yaml:"enabled" json:"enabled"`
}

// Retention mirrors Barman's retention_policy and minimum_redundancy.
type Retention struct {
	RecoveryWindowDays int `yaml:"recovery_window_days" json:"recovery_window_days"` // 0 = not set
	Redundancy         int `yaml:"redundancy" json:"redundancy"`                     // number of backups to keep, 0 = not set
	MinimumRedundancy  int `yaml:"minimum_redundancy" json:"minimum_redundancy"`
}

// Alerts lists the channels. Empty = silent.
type Alerts struct {
	Mail     *MailAlert     `yaml:"mail,omitempty" json:"mail,omitempty"`
	Telegram *TelegramAlert `yaml:"telegram,omitempty" json:"telegram,omitempty"`
	Webhook  *WebhookAlert  `yaml:"webhook,omitempty" json:"webhook,omitempty"`
	// MaxBackupAge: alert when the last successful backup is older than this.
	MaxBackupAge time.Duration `yaml:"max_backup_age" json:"max_backup_age"` // e.g. 26h
	// MaxWALAge: alert when the last archived WAL is older than this.
	MaxWALAge time.Duration `yaml:"max_wal_age" json:"max_wal_age"` // e.g. 1h
}

type MailAlert struct {
	SMTPHost    string   `yaml:"smtp_host" json:"smtp_host"`
	SMTPPort    int      `yaml:"smtp_port" json:"smtp_port"`
	From        string   `yaml:"from" json:"from"`
	To          []string `yaml:"to" json:"to"`
	UserEnv     string   `yaml:"user_env" json:"user_env"`         // name of the env var holding the SMTP user
	PasswordEnv string   `yaml:"password_env" json:"password_env"` // name of the env var holding the SMTP password
}

type TelegramAlert struct {
	TokenEnv string `yaml:"token_env" json:"token_env"` // env var name, never the token itself
	ChatID   string `yaml:"chat_id" json:"chat_id"`
}

type WebhookAlert struct {
	URL string `yaml:"url" json:"url"`
}

// User is a web user. Password hashes are bcrypt. Roles: admin, operator, viewer.
type User struct {
	Name         string `yaml:"name" json:"name"`
	Role         string `yaml:"role" json:"role"`
	PasswordHash string `yaml:"password_hash" json:"password_hash"`
}

// APIKey identifies an API client. Only the SHA-256 of the key is stored.
type APIKey struct {
	Name    string `yaml:"name" json:"name"`
	Role    string `yaml:"role" json:"role"`
	KeyHash string `yaml:"key_hash" json:"key_hash"`
}

// Drills configures the automatic restore drills (milestone 5).
type Drills struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	Schedule  string `yaml:"schedule" json:"schedule"` // "weekly sun 05:00"
	Image     string `yaml:"image" json:"image"`       // postgres:16 for the throwaway container
	ReportDir string `yaml:"report_dir" json:"report_dir"`
}

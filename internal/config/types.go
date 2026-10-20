// Package config owns pgcellar.yaml, the only file that belongs to pgCellar
// (everything about Barman stays in Barman's own files, see internal/barman),
// and the local git history of the configuration directory.
package config

import "time"

// Config is the whole pgcellar.yaml. Keep it boring: strings, lists, maps.
// A sysadmin, an automation agent or git must be able to edit it by hand.
type Config struct {
	Listen  string `yaml:"listen" json:"listen"`     // ":8080"
	BaseURL string `yaml:"base_url" json:"base_url"` // "https://cellar.example.com", used in links and alerts
	DataDir string `yaml:"data_dir" json:"data_dir"` // SQLite and runtime files, e.g. /var/lib/pgcellar
	Barman  Barman `yaml:"barman" json:"barman"`
	// Scheduler runs the backup schedules of the servers.
	Scheduler Scheduler `yaml:"scheduler" json:"scheduler"`
	Servers   []Server  `yaml:"servers" json:"servers"` // what pgCellar adds on top of barman.d, keyed by name
	Alerts    Alerts    `yaml:"alerts" json:"alerts"`
	Users     []User    `yaml:"users" json:"users"`
	APIKeys   []APIKey  `yaml:"api_keys" json:"api_keys"`
	Drills    Drills    `yaml:"drills" json:"drills"`
	// Recoveries is where point-in-time restores are written.
	Recoveries Recoveries `yaml:"recoveries" json:"recoveries"`
	// StatusInterval is how often the cached status of every server is
	// refreshed. Each refresh runs several Barman commands per server.
	StatusInterval time.Duration `yaml:"status_interval" json:"status_interval"`
	// SetupInterval is how often the PostgreSQL settings of every server are
	// read again for the "Server setup" section (one psql query per server).
	SetupInterval time.Duration `yaml:"setup_interval" json:"setup_interval"`
	// SSHHost is the name PostgreSQL servers use to reach this host over SSH,
	// shown in the archive_command pgCellar advises (barman-wal-archive
	// <ssh_host> <server> %p). Empty = the host name of this machine.
	SSHHost string `yaml:"ssh_host" json:"ssh_host"`
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

// Scheduler configures the built-in scheduler: pgCellar starts the backups of
// every server whose schedule is enabled, no host cron needed.
type Scheduler struct {
	Enabled bool `yaml:"enabled" json:"enabled"` // default true
	// CatchUp: a due time missed while pgCellar was stopped is still run at
	// startup when it is not older than this. 0 = never catch up. Default 6h.
	CatchUp time.Duration `yaml:"catch_up" json:"catch_up"`
	// Parallel is the most backups running at once, all servers together.
	// Default 1.
	Parallel int `yaml:"parallel" json:"parallel"`
}

// DefaultCatchUp is the default scheduler.catch_up.
const DefaultCatchUp = 6 * time.Hour

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

// Recoveries configures the "restore to a directory" feature. The directory
// lives inside the container and must be mapped to the Docker host, owned by
// the barman user, to be of any use there.
type Recoveries struct {
	Dir string `yaml:"dir" json:"dir"` // every restore is written under <dir>/<server>/<backup id>-<timestamp>
	// MaxDownloadBytes: an archive bigger than this has no download link, only
	// its path. 0 = no limit.
	MaxDownloadBytes int64 `yaml:"max_download_bytes" json:"max_download_bytes"`
	// KeepDays: restores older than this are removed daily. 0 = keep forever.
	KeepDays int `yaml:"keep_days" json:"keep_days"`
}

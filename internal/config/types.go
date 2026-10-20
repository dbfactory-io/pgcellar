// Package config owns pgcellar.yaml, the only file that belongs to pgCellar
// (everything about Barman stays in Barman's own files, see internal/barman),
// and the local git history of the configuration directory.
package config

import (
	"encoding/json"
	"strings"
	"time"
)

// Ptr returns a pointer to v: how a setting is given an explicit value in a
// Go struct (nil = the key is absent = the default applies).
func Ptr[T any](v T) *T { return &v }

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
	License    License    `yaml:"license" json:"license"`
	// StatusInterval is how often the cached status of every server is
	// refreshed. Each refresh runs several Barman commands per server.
	StatusInterval time.Duration `yaml:"status_interval,omitempty" json:"status_interval"`
	// SetupInterval is how often the PostgreSQL settings of every server are
	// read again for the "Server setup" section (one psql query per server).
	SetupInterval time.Duration `yaml:"setup_interval,omitempty" json:"setup_interval"`
	// SSHHost is the name PostgreSQL servers use to reach this host over SSH,
	// shown in the archive_command pgCellar advises (barman-wal-archive
	// <ssh_host> <server> %p). Empty = the host name of this machine.
	SSHHost string `yaml:"ssh_host" json:"ssh_host"`
	// SSHPort is the port of the SSH companion container (the sshd that
	// receives WAL files), used in the advised archive_command
	// (barman-wal-archive --port <ssh_port> ...). 0 = 2222.
	SSHPort int `yaml:"ssh_port" json:"ssh_port"`
	// AuditRetentionDays: audit lines older than this are purged daily. 0 = keep forever.
	// nil = absent = 90 days.
	AuditRetentionDays *int `yaml:"audit_retention_days,omitempty" json:"audit_retention_days"`
}

// DefaultAuditRetentionDays is audit_retention_days when the key is absent.
const DefaultAuditRetentionDays = 90

// EffectiveAuditRetentionDays resolves audit_retention_days (0 = keep forever).
func (c Config) EffectiveAuditRetentionDays() int {
	if c.AuditRetentionDays == nil {
		return DefaultAuditRetentionDays
	}
	return *c.AuditRetentionDays
}

// MarshalJSON shows the resolved audit retention, never a null.
func (c Config) MarshalJSON() ([]byte, error) {
	type plain Config
	return json.Marshal(struct {
		plain
		AuditRetentionDays int `json:"audit_retention_days"`
	}{plain(c), c.EffectiveAuditRetentionDays()})
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
	// Enabled: nil = absent = true (see Config rules in docs/DECISIONS.md).
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled"`
	// CatchUp: a due time missed while pgCellar was stopped is still run at
	// startup when it is not older than this. 0 = never catch up; nil = absent
	// = 6h.
	CatchUp *time.Duration `yaml:"catch_up,omitempty" json:"catch_up"`
	// Parallel is the most backups running at once, all servers together.
	// Default 1 (0 or absent).
	Parallel int `yaml:"parallel,omitempty" json:"parallel"`
}

// IsEnabled resolves scheduler.enabled (absent = true).
func (s Scheduler) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// EffectiveCatchUp resolves scheduler.catch_up (absent = DefaultCatchUp, 0 = never).
func (s Scheduler) EffectiveCatchUp() time.Duration {
	if s.CatchUp == nil {
		return DefaultCatchUp
	}
	return *s.CatchUp
}

// MarshalJSON shows resolved values, never a pointer or a null.
func (s Scheduler) MarshalJSON() ([]byte, error) {
	type plain Scheduler
	return json.Marshal(struct {
		plain
		Enabled bool          `json:"enabled"`
		CatchUp time.Duration `json:"catch_up"`
	}{plain(s), s.IsEnabled(), s.EffectiveCatchUp()})
}

// DefaultCatchUp is the default scheduler.catch_up.
const DefaultCatchUp = 6 * time.Hour

// Server holds the pgCellar side of a server: schedule, retention, notes.
// The name must match a [section] in barman.d.
type Server struct {
	Name      string       `yaml:"name" json:"name"`
	Label     string       `yaml:"label" json:"label"` // free text shown in the UI
	Schedule  Schedule     `yaml:"schedule" json:"schedule"`
	Retention Retention    `yaml:"retention" json:"retention"`
	Drills    ServerDrills `yaml:"drills" json:"drills"`
	Tags      []string     `yaml:"tags" json:"tags"`
}

// Schedule is human-readable and rendered into a crontab line for `barman cron`
// plus one `barman backup` entry.
type Schedule struct {
	Backup string `yaml:"backup" json:"backup"` // "daily 02:00", "weekly sun 03:00", or a raw cron expression
	// Enabled: nil = absent = on when a backup schedule is given, off when
	// there is none.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled"`
}

// IsEnabled resolves schedule.enabled: absent means on as soon as a backup
// schedule is written.
func (s Schedule) IsEnabled() bool {
	if s.Enabled == nil {
		return strings.TrimSpace(s.Backup) != ""
	}
	return *s.Enabled
}

// MarshalJSON shows the resolved boolean, never a null.
func (s Schedule) MarshalJSON() ([]byte, error) {
	type plain Schedule
	return json.Marshal(struct {
		plain
		Enabled bool `json:"enabled"`
	}{plain(s), s.IsEnabled()})
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
	// MinFreePercent: a file system holding backups with less free space than
	// this (percent) turns its servers orange, under half of it red. 0 = off.
	MinFreePercent float64 `yaml:"min_free_percent" json:"min_free_percent"`
	// MinFreeBytes: same, in bytes. 0 = off.
	MinFreeBytes int64 `yaml:"min_free_bytes" json:"min_free_bytes"`
	// RepeatEvery: a condition that lasts is announced again only after this
	// delay. 0 = never repeat; nil = absent = 24h.
	RepeatEvery *time.Duration `yaml:"repeat_every,omitempty" json:"repeat_every"`
	// OnDrillSuccess also sends a message when a restore drill passes.
	OnDrillSuccess bool `yaml:"on_drill_success" json:"on_drill_success"`
	// DailySummary is a time of day ("08:30") at which one message sums up
	// every server. Empty = no summary.
	DailySummary string `yaml:"daily_summary" json:"daily_summary"`
}

// DefaultRepeatEvery is alerts.repeat_every when it is not set.
const DefaultRepeatEvery = 24 * time.Hour

// EffectiveRepeatEvery resolves alerts.repeat_every (absent = 24h, 0 = never).
func (a Alerts) EffectiveRepeatEvery() time.Duration {
	if a.RepeatEvery == nil {
		return DefaultRepeatEvery
	}
	return *a.RepeatEvery
}

// MarshalJSON shows the resolved delay, never a null.
func (a Alerts) MarshalJSON() ([]byte, error) {
	type plain Alerts
	return json.Marshal(struct {
		plain
		RepeatEvery time.Duration `json:"repeat_every"`
	}{plain(a), a.EffectiveRepeatEvery()})
}

// UsesStartTLS resolves alerts.mail.starttls (absent = true).
func (m MailAlert) UsesStartTLS() bool { return m.StartTLS == nil || *m.StartTLS }

type MailAlert struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
	// StartTLS upgrades the connection with STARTTLS (default true); false
	// sends in clear, for a relay on a trusted network.
	StartTLS    *bool    `yaml:"starttls,omitempty" json:"starttls,omitempty"`
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
	// SecretEnv names the env var holding the secret that signs the body
	// (X-PgCellar-Signature: sha256=<hmac>). Empty = unsigned.
	SecretEnv string `yaml:"secret_env" json:"secret_env"`
}

// License holds the licence key, or the file that holds it. Neither set =
// Free edition.
type License struct {
	Key  string `yaml:"key" json:"key"`
	File string `yaml:"file" json:"file"`
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

// Drills configures the automatic restore drills: a real restore of a backup,
// started as a throwaway PostgreSQL and checked, with a dated PDF report. What
// is global lives here; the schedule of each server is Server.Drills.
type Drills struct {
	// Enabled: nil = absent = true; false stops the scheduled drills only.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled"`
	// Timeout bounds a whole drill (restore included). Default 30m.
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout"`
	// RecoveryTimeout bounds the wait for the restored copy to finish its
	// recovery (WAL replay) once it is started. Default 10m.
	RecoveryTimeout time.Duration `yaml:"recovery_timeout,omitempty" json:"recovery_timeout"`
	// KeepLast keeps the data directory of the last successful drill of each
	// server and removes the previous one. nil = absent = true; false removes
	// it at once.
	KeepLast *bool `yaml:"keep_last,omitempty" json:"keep_last"`
	// VerifyTables adds a SELECT count(*) on every table: slow on big databases.
	VerifyTables bool `yaml:"verify_tables" json:"verify_tables"`
	// MaxAge: a server with a drill schedule turns orange when its last
	// successful drill is older than this. Default 192h (8 days).
	MaxAge time.Duration `yaml:"max_age,omitempty" json:"max_age"`
	// BinDir holds one directory per PostgreSQL major version, each with a
	// bin/ (pg_ctl, psql...). Default /usr/lib/postgresql.
	BinDir string `yaml:"bin_dir,omitempty" json:"bin_dir"`
}

// IsEnabled resolves drills.enabled (absent = true).
func (d Drills) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// KeepsLast resolves drills.keep_last (absent = true).
func (d Drills) KeepsLast() bool { return d.KeepLast == nil || *d.KeepLast }

// MarshalJSON shows resolved booleans, never a pointer or a null.
func (d Drills) MarshalJSON() ([]byte, error) {
	type plain Drills
	return json.Marshal(struct {
		plain
		Enabled  bool `json:"enabled"`
		KeepLast bool `json:"keep_last"`
	}{plain(d), d.IsEnabled(), d.KeepsLast()})
}

// ServerDrills is the drill setting of one server.
type ServerDrills struct {
	// Schedule: same syntax as schedule.backup. Empty = no automatic drill.
	Schedule string `yaml:"schedule" json:"schedule"`
}

// Default drill settings.
const (
	DefaultDrillTimeout         = 30 * time.Minute
	DefaultDrillRecoveryTimeout = 10 * time.Minute
	DefaultDrillMaxAge          = 8 * 24 * time.Hour
	DefaultPGBinDir             = "/usr/lib/postgresql"
)

// Recoveries configures the "restore to a directory" feature. The directory
// lives inside the container and must be mapped to the Docker host, owned by
// the barman user, to be of any use there.
type Recoveries struct {
	Dir string `yaml:"dir" json:"dir"` // every restore is written under <dir>/<server>/<backup id>-<timestamp>
	// MaxDownloadBytes: an archive bigger than this has no download link, only
	// its path. 0 = no limit.
	MaxDownloadBytes int64 `yaml:"max_download_bytes" json:"max_download_bytes"`
	// KeepDays: restores older than this are removed daily. 0 = keep forever;
	// nil = absent = 7.
	KeepDays *int `yaml:"keep_days,omitempty" json:"keep_days"`
}

// DefaultRecoveriesKeepDays is recoveries.keep_days when the key is absent.
const DefaultRecoveriesKeepDays = 7

// EffectiveKeepDays resolves recoveries.keep_days (0 = keep forever).
func (r Recoveries) EffectiveKeepDays() int {
	if r.KeepDays == nil {
		return DefaultRecoveriesKeepDays
	}
	return *r.KeepDays
}

// MarshalJSON shows the resolved number of days, never a null.
func (r Recoveries) MarshalJSON() ([]byte, error) {
	type plain Recoveries
	return json.Marshal(struct {
		plain
		KeepDays int `json:"keep_days"`
	}{plain(r), r.EffectiveKeepDays()})
}

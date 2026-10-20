package app

import (
	"time"

	"github.com/stranix79/pgcellar/internal/config"
)

// ServerInput is everything the user can set on a server: the Barman options
// pgCellar understands plus the pgCellar side (label, retention, schedule).
type ServerInput struct {
	Name              string           `json:"name"`
	Label             string           `json:"label"`
	Description       string           `json:"description"`
	BackupMethod      string           `json:"backup_method"` // postgres | rsync
	ConnInfo          string           `json:"conninfo"`
	StreamingConnInfo string           `json:"streaming_conninfo"`
	Slot              string           `json:"slot_name"`
	SSHCommand        string           `json:"ssh_command"`
	Retention         config.Retention `json:"retention"`
	Schedule          config.Schedule  `json:"schedule"`
}

// ServerDetail is a server as shown by the list and detail pages.
type ServerDetail struct {
	ServerInput
	HasFile       bool         `json:"has_file"`  // a section exists in barman.d
	InBarman      bool         `json:"in_barman"` // `barman list-servers` knows it
	File          string       `json:"file,omitempty"`
	ScheduleHuman string       `json:"schedule_human,omitempty"`
	Problems      []string     `json:"problems"` // what is wrong with the barman.d definition
	State         *ServerState `json:"state,omitempty"`
}

// Levels of a server state.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelFail = "fail"
)

// CheckView is one `barman check` line with its explanation.
type CheckView struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	OK      bool   `json:"ok"`
	What    string `json:"what"`
	Fix     string `json:"fix,omitempty"`
}

// ServerState is the cached digest of a server (refreshed every minute).
type ServerState struct {
	Name                 string      `json:"name"`
	Level                string      `json:"level"`
	LastBackupID         string      `json:"last_backup_id,omitempty"`
	LastBackupEnd        time.Time   `json:"last_backup_end,omitzero"`
	LastBackupSize       int64       `json:"last_backup_size,omitempty"`
	LastBackupAgeSeconds *int64      `json:"last_backup_age_seconds,omitempty"`
	LastWAL              time.Time   `json:"last_wal,omitzero"`
	LastWALAgeSeconds    *int64      `json:"last_wal_age_seconds,omitempty"`
	BackupCount          int         `json:"backup_count"`
	Checks               []CheckView `json:"checks"`
	CheckOK              bool        `json:"check_ok"`
	Problems             []string    `json:"problems"`
	Error                string      `json:"error,omitempty"`
	UpdatedAt            time.Time   `json:"updated_at"`
}

// BackupView is one backup of a server.
type BackupView struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	BeginTime    time.Time `json:"begin_time,omitzero"`
	EndTime      time.Time `json:"end_time,omitzero"`
	SizeBytes    int64     `json:"size_bytes"`
	WALSizeBytes int64     `json:"wal_size_bytes"`
	PGVersion    string    `json:"pg_version,omitempty"`
}

// Run states.
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunFailed  = "failed"
)

// RunView is one recorded execution.
type RunView struct {
	ID         int64      `json:"id"`
	Server     string     `json:"server"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Actor      string     `json:"actor"`
	Stdout     string     `json:"stdout,omitempty"`
	Stderr     string     `json:"stderr,omitempty"`
}

// HistoryItem is one commit of a configuration repository.
type HistoryItem struct {
	Repo    string `json:"repo"` // "config" or "barman"
	Hash    string `json:"hash"`
	Date    string `json:"date"`
	Author  string `json:"author"`
	Message string `json:"message"`
}

// WriteResult describes a configuration change that was written.
type WriteResult struct {
	Commit  string `json:"commit,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// AuditItem is one audit log line.
type AuditItem struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Target  string    `json:"target"`
	Details string    `json:"details"`
}

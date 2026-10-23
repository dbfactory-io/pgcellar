package app

import (
	"time"

	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

// ServerInput is everything the user can set on a server: the Barman options
// pgCellar understands plus the pgCellar side (label, retention, schedule).
type ServerInput struct {
	Name              string `json:"name"`
	Label             string `json:"label"`
	Description       string `json:"description"`
	BackupMethod      string `json:"backup_method"` // postgres | rsync
	ConnInfo          string `json:"conninfo"`
	StreamingConnInfo string `json:"streaming_conninfo"`
	Slot              string `json:"slot_name"`
	SSHCommand        string `json:"ssh_command"`
	// WALMethod says how WAL files reach Barman: streaming (streaming_archiver
	// = on, archiver = off), archive_command (archiver = on, streaming_archiver
	// = off) or both. Empty on input = keep the file as it is, or the usual
	// choice of the backup method for a new server.
	WALMethod string           `json:"wal_method"`
	Retention config.Retention `json:"retention"`
	Schedule  config.Schedule  `json:"schedule"`
}

// WAL methods of a server.
const (
	WALStreaming      = "streaming"
	WALArchiveCommand = "archive_command"
	WALBoth           = "both"
)

// ServerDetail is a server as shown by the list and detail pages.
type ServerDetail struct {
	ServerInput
	HasFile       bool         `json:"has_file"`  // a section exists in barman.d
	InBarman      bool         `json:"in_barman"` // `barman list-servers` knows it
	File          string       `json:"file,omitempty"`
	ScheduleHuman string       `json:"schedule_human,omitempty"`
	Problems      []string     `json:"problems"` // what is wrong with the barman.d definition
	State         *ServerState `json:"state,omitempty"`
	// NextBackup is the next due time of the schedule (zero when it is off).
	NextBackup    time.Time     `json:"next_backup,omitzero"`
	LastScheduled *ScheduledRun `json:"last_scheduled,omitempty"`
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

// ServerState is the cached digest of a server (refreshed every status interval).
type ServerState struct {
	Name  string `json:"name"`
	Level string `json:"level"`
	// NextBackup and LastScheduled come from the scheduler, not from Barman.
	NextBackup           time.Time     `json:"next_backup,omitzero"`
	LastScheduled        *ScheduledRun `json:"last_scheduled,omitempty"`
	LastBackupID         string        `json:"last_backup_id,omitempty"`
	LastBackupEnd        time.Time     `json:"last_backup_end,omitzero"`
	LastBackupSize       int64         `json:"last_backup_size,omitempty"`
	LastBackupAgeSeconds *int64        `json:"last_backup_age_seconds,omitempty"`
	// LastFilledWAL is the last WAL file Barman received complete (a WAL file
	// is 16 MB; a quiet server fills one slowly). Name and time come from the
	// WAL catalogue file.
	LastFilledWAL              string    `json:"last_filled_wal,omitempty"`
	LastFilledWALTime          time.Time `json:"last_filled_wal_time,omitzero"`
	LastFilledWALAgeSeconds    *int64    `json:"last_filled_wal_age_seconds,omitempty"`
	LastWAL                    time.Time `json:"last_wal,omitzero"`              // deprecated alias of last_filled_wal_time
	LastWALAgeSeconds          *int64    `json:"last_wal_age_seconds,omitempty"` // deprecated alias of last_filled_wal_age_seconds
	StreamingMonitored         bool      `json:"streaming_monitored"`            // Barman checks a WAL receiver for this server
	StreamingLive              bool      `json:"streaming_live"`                 // the WAL receiver runs (check receive_wal_running)
	LastDataReceived           time.Time `json:"last_data_received,omitzero"`    // when data last reached Barman (streaming directory)
	LastDataReceivedAgeSeconds *int64    `json:"last_data_received_age_seconds,omitempty"`
	// Drill is the state of the restore drills, filled when the state is read.
	Drill *DrillStatus `json:"drill,omitempty"`
	// Vault is the state of the off-site copy, filled when the state is read;
	// absent when the vault is off.
	Vault *VaultServerStatus `json:"vault,omitempty"`
	// Storage is what the server takes on disk and what is left (measured at
	// each refresh).
	Storage *StorageInfo `json:"storage,omitempty"`
	// Notes are remarks that are not alerts.
	Notes       []string    `json:"notes"`
	BackupCount int         `json:"backup_count"`
	Checks      []CheckView `json:"checks"`
	CheckOK     bool        `json:"check_ok"`
	Problems    []string    `json:"problems"`
	Error       string      `json:"error,omitempty"`
	// Setup is what pgCellar read in the PostgreSQL settings of the server
	// (the "Server setup" section). It is read again only every
	// setup_interval, or on "Run check now"; SetupAt says when. SetupLevel is
	// ok | warning | error, empty when it was never evaluated. SetupIssues
	// counts the warnings and errors.
	Setup       []barman.SetupCheck `json:"setup"`
	SetupLevel  string              `json:"setup_level"`
	SetupIssues int                 `json:"setup_issues"`
	SetupAt     time.Time           `json:"setup_at,omitzero"`
	UpdatedAt   time.Time           `json:"updated_at"`
	// Stale is true for a state reloaded from the database after a restart,
	// until the first refresh has completed.
	Stale bool `json:"stale"`

	// Backups is the list from the last refresh, newest first. Served by the
	// backups endpoint and the server page, never by running Barman.
	Backups []BackupView `json:"-"`
	raw     *barman.ServerStatus
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

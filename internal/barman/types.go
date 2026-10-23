// Package barman drives an installed Barman (EnterpriseDB, GPL-3.0) through
// its command line and its configuration files. Nothing from Barman is
// imported or linked: this package only knows the shape of its INI files and
// of its JSON output. That boundary is a product rule (see CLAUDE.md).
package barman

import (
	"context"
	"fmt"
	"time"
)

// Server is one [section] of a barman.d file: a PostgreSQL server Barman
// backs up. Only the options pgCellar understands are typed; everything else
// is kept verbatim in Extra so that a round trip never loses a line a
// sysadmin wrote by hand.
type Server struct {
	Name              string            // section name, e.g. "pg-main"
	Description       string            // description
	ConnInfo          string            // conninfo (libpq string)
	SSHCommand        string            // ssh_command (rsync method), empty for streaming
	BackupMethod      string            // backup_method: postgres | rsync
	StreamingConnInfo string            // streaming_conninfo
	StreamingArchiver string            // streaming_archiver: on | off
	Slot              string            // slot_name
	Archiver          string            // archiver: on | off
	RetentionPolicy   string            // retention_policy, e.g. "RECOVERY WINDOW OF 7 DAYS"
	MinimumRedundancy string            // minimum_redundancy
	Extra             map[string]string // every other key, untouched
	Order             []string          // key order as found in the file, to write it back the same way
	File              string            // absolute path of the .conf file this section came from
}

// GlobalConfig is the [barman] section of /etc/barman.conf.
type GlobalConfig struct {
	BarmanHome                  string // barman_home
	BarmanUser                  string // barman_user
	LogFile                     string // log_file
	ConfigurationFilesDirectory string // configuration_files_directory
	Extra                       map[string]string
	Order                       []string
	File                        string
}

// Backup is one entry of `barman list-backups --format json` enriched by
// `show-backup` when available.
type Backup struct {
	Server    string
	ID        string // e.g. 20261020T020001
	Status    string // DONE, FAILED, STARTED, WAITING_FOR_WALS...
	BeginTime time.Time
	EndTime   time.Time
	Size      int64 // bytes
	WALSize   int64 // bytes of WALs since this backup, if reported
	PGVersion string
	Raw       map[string]any // the JSON object as returned, for the UI "details" view
}

// CheckResult is one line of `barman check <server>`.
type CheckResult struct {
	Name string // e.g. "WAL archive", "PostgreSQL", "ssh"
	OK   bool
	Hint string // Barman's own hint when FAILED, may be empty
}

// ServerStatus is the digest pgCellar shows per server: built from `status`,
// `list-backups` and `check`.
type ServerStatus struct {
	Server      string
	LastBackup  *Backup
	LastWAL     string // name of the last archived WAL segment, "" if unknown
	LastWALTime time.Time
	BackupCount int
	// Backups is the list `list-backups` returned, oldest first. Kept so that
	// pages can show it from the cache without running Barman again.
	Backups []Backup
	Checks  []CheckResult
	// StreamingLive is true when the receive_wal_running check passes: the
	// WAL receiver is connected and writing.
	StreamingLive bool
	// LastDataReceived is the modification time of the newest file in
	// <barman_home>/<server>/streaming (the .partial segment being written),
	// zero when the directory is absent or unreadable. It moves while
	// LastWALTime (last closed segment) stays still on a quiet database.
	LastDataReceived time.Time
	Healthy          bool
	Problems         []string // human sentences, already explained
	// BaseProblems is Problems without the lines that come from Checks, so
	// that a fresh check can be applied without running everything again.
	BaseProblems []string
}

// CheckProblems turns the failed lines of a check into explained sentences.
func CheckProblems(checks []CheckResult) []string {
	var out []string
	for _, c := range checks {
		if c.OK {
			continue
		}
		what, fix := Explain(c)
		out = append(out, fmt.Sprintf("%s failed. %s %s", DisplayName(c.Name), what, fix))
	}
	return out
}

// StreamingLiveFrom reports whether the receive_wal_running check passes.
func StreamingLiveFrom(checks []CheckResult) bool {
	for _, c := range checks {
		if c.Name == "receive_wal_running" {
			return c.OK
		}
	}
	return false
}

// HasStreamingCheck reports whether Barman checks a WAL receiver at all
// (servers that archive through archive_command have no such check).
func HasStreamingCheck(checks []CheckResult) bool {
	for _, c := range checks {
		if c.Name == "receive_wal_running" {
			return true
		}
	}
	return false
}

// WithChecks returns a copy of the digest with fresh check results.
func (s *ServerStatus) WithChecks(checks []CheckResult) *ServerStatus {
	c := *s
	c.Checks = checks
	c.StreamingLive = StreamingLiveFrom(checks)
	c.Problems = append(append([]string(nil), s.BaseProblems...), CheckProblems(checks)...)
	c.Healthy = len(c.Problems) == 0
	return &c
}

// Runner executes Barman commands. There is exactly one implementation that
// shells out (run.go) and one fake used by tests and `--dev` mode. Every call
// is audited by the caller through the Audit hook.
type Runner interface {
	// Run executes `barman <args...>` and returns stdout, stderr and the exit
	// error. It must log the exact command, exit code and duration.
	Run(ctx context.Context, args ...string) (stdout, stderr []byte, err error)
}

// BinaryRunner runs a program other than Barman through the same audited path
// as Runner (CLIRunner implements both).
type BinaryRunner interface {
	RunBinary(ctx context.Context, binary string, args ...string) (stdout, stderr []byte, err error)
}

// EnvRunner is a BinaryRunner that can also hand environment variables to
// the program (restic and its S3 credentials). CLIRunner is one.
type EnvRunner interface {
	RunBinaryEnv(ctx context.Context, env []string, binary string, args ...string) (stdout, stderr []byte, err error)
}

// Audit receives one entry per executed command.
type Audit func(entry AuditEntry)

// AuditEntry describes one executed Barman command.
type AuditEntry struct {
	When     time.Time
	Args     []string // barman arguments as passed to Run (without binary or sudo)
	Command  string   // exact command line that was executed, including any sudo prefix
	ExitCode int
	Duration time.Duration
	Actor    string // user or api key name, set by the caller
	Binary   string // the program that was run: barman, psql, pg_basebackup
	Target   string // server the command is about, when the caller said so (WithTarget)
}

// Engine is the small surface the rest of pgCellar uses. A second engine
// (pgBackRest) would implement the same interface; it is designed for, not
// built.
type Engine interface {
	ListServers(ctx context.Context) ([]string, error)
	Check(ctx context.Context, server string) ([]CheckResult, error)
	ListBackups(ctx context.Context, server string) ([]Backup, error)
	ShowBackup(ctx context.Context, server, backupID string) (*Backup, error)
	Status(ctx context.Context, server string) (*ServerStatus, error)
	Backup(ctx context.Context, server string) error    // runs `barman backup <server>`
	SwitchWAL(ctx context.Context, server string) error // `barman switch-wal --force --archive <server>`
	Cron(ctx context.Context) error                     // `barman cron`
	// DeleteBackup runs `barman delete <server> <backup_id>` and returns what
	// Barman printed. Barman itself refuses to go below minimum_redundancy.
	DeleteBackup(ctx context.Context, server, backupID string) (string, error)
	// Recover runs `barman recover` on this host into dest, see RecoverOptions.
	Recover(ctx context.Context, server, backupID, dest string, opts RecoverOptions) (string, error)
}

// RecoverOptions are the optional parts of `barman recover`.
type RecoverOptions struct {
	// TargetTime is the point in time to recover to, passed as
	// --target-time. Empty means "as far as the archived WAL goes" (no
	// target at all), which is also what Barman does by default.
	TargetTime string
	// TargetImmediate recovers to the end of the backup itself and stops as
	// soon as the data is consistent (--target-immediate).
	TargetImmediate bool
}

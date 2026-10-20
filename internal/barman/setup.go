package barman

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Levels of a setup check.
const (
	SetupOK      = "ok"
	SetupInfo    = "info"
	SetupWarning = "warning"
	SetupError   = "error"
)

// Keys of the setup checks that come from `barman check` results rather than
// from the PostgreSQL settings: they are recomputed at every refresh.
const (
	SetupKeyStreaming  = "streaming_access"
	SetupKeyPrivileges = "backup_privileges"
	SetupKeySlot       = "replication_slot"
	// The three lines below are Barman's own verdict on WAL archiving.
	SetupKeyBarmanWALArchive     = "barman_wal_archive"
	SetupKeyBarmanArchiveCommand = "barman_archive_command"
	SetupKeyBarmanArchiveMode    = "barman_archive_mode"
	// SetupKeyUnreadable is the single line shown when psql failed.
	SetupKeyUnreadable = "unreadable"
)

// SetupCheck is one line of the "Server setup" section: what pgCellar read on
// the PostgreSQL server, what it expects, and what to change there.
type SetupCheck struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Level    string `json:"level"` // ok | info | warning | error
	Value    string `json:"value"`
	Expected string `json:"expected,omitempty"`
	Why      string `json:"why"`
	Fix      string `json:"fix,omitempty"`
	Restart  bool   `json:"restart"` // the fix needs a restart of PostgreSQL, otherwise a reload is enough
}

// IsDerived reports whether the check is built from `barman check` results.
func (c SetupCheck) IsDerived() bool {
	switch c.Key {
	case SetupKeyStreaming, SetupKeyPrivileges, SetupKeySlot,
		SetupKeyBarmanWALArchive, SetupKeyBarmanArchiveCommand, SetupKeyBarmanArchiveMode:
		return true
	}
	return false
}

// SetupSettings are the pg_settings rows read by SetupQuery.
var setupNames = []string{
	"archive_command", "archive_mode", "archive_timeout", "data_checksums", "full_page_writes",
	"max_replication_slots", "max_slot_wal_keep_size", "max_wal_senders",
	"server_version_num", "summarize_wal", "wal_compression", "wal_level",
}

// SetupQuery is the one read-only query run on the PostgreSQL server. Four
// tab-separated columns: name, setting, unit, context. Settings that do not
// exist in the server version simply give no row.
func SetupQuery() string {
	quoted := make([]string, len(setupNames))
	for i, n := range setupNames {
		quoted[i] = "'" + n + "'"
	}
	return "select name, setting, coalesce(unit, ''), context from pg_settings where name in (" +
		strings.Join(quoted, ", ") + ") order by name"
}

// PGSetting is one row of pg_settings.
type PGSetting struct {
	Value   string
	Unit    string
	Context string
}

// ParseSettings reads the output of SetupQuery (psql -At -F tab).
func ParseSettings(out []byte) (map[string]PGSetting, error) {
	m := map[string]PGSetting{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected psql output line %q", line)
		}
		m[f[0]] = PGSetting{Value: f[1], Unit: f[2], Context: f[3]}
	}
	if _, ok := m["server_version_num"]; !ok {
		return nil, errors.New("the answer holds no server_version_num")
	}
	return m, nil
}

var passwordRE = regexp.MustCompile(`(?i)\s*\bpassword\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// PsqlConnInfo prepares a conninfo for psql: a password is dropped (it must
// come from ~/.pgpass, never from a command line) and a connection timeout is
// added so that an unreachable server does not hold the refresh.
func PsqlConnInfo(conninfo string) string {
	c := strings.TrimSpace(passwordRE.ReplaceAllString(conninfo, ""))
	if strings.Contains(c, "://") {
		return c
	}
	if !regexp.MustCompile(`(?i)\bconnect_timeout\s*=`).MatchString(c) {
		c += " connect_timeout=10"
	}
	return strings.TrimSpace(c)
}

// PsqlArgs is the argument list of the setup query for a conninfo.
func PsqlArgs(conninfo string) []string {
	return []string{PsqlConnInfo(conninfo), "-X", "-At", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-c", SetupQuery()}
}

// SetupInput is everything the rules need.
type SetupInput struct {
	Settings      map[string]PGSetting
	ClientVersion string // output of `pg_basebackup --version`, empty when it could not be read
	SlotName      string // slot_name of the Barman server
	ArchiverOn    bool   // archiver = on in the Barman server (WAL by archive_command)
	ServerName    string // name of the Barman server, used in the advised archive_command
	SSHHost       string // how PostgreSQL reaches pgCellar over SSH, used in the advised archive_command
}

// SetupTools names the PostgreSQL client programs; empty means the usual name
// on PATH. Tests and --dev point them to the fakes.
type SetupTools struct {
	Psql       string
	Basebackup string
}

// ReadSetup runs the query and the version probe through r and returns the
// checks that come from PostgreSQL itself. When psql fails the result is one
// error line and the rest is not evaluated.
func ReadSetup(ctx context.Context, r BinaryRunner, tools SetupTools, srv Server, sshHost string) []SetupCheck {
	ctx = WithTarget(ctx, srv.Name)
	psql := tools.Psql
	if psql == "" {
		psql = "psql"
	}
	out, errOut, err := r.RunBinary(ctx, psql, PsqlArgs(srv.ConnInfo)...)
	if err != nil {
		return []SetupCheck{unreadable(err, errOut)}
	}
	settings, err := ParseSettings(out)
	if err != nil {
		return []SetupCheck{unreadable(err, nil)}
	}
	basebackup := tools.Basebackup
	if basebackup == "" {
		basebackup = "pg_basebackup"
	}
	if p := strings.TrimSpace(srv.Extra["path_prefix"]); p != "" {
		basebackup = filepath.Join(p, "pg_basebackup")
	}
	client := ""
	if vo, _, verr := r.RunBinary(ctx, basebackup, "--version"); verr == nil {
		client = strings.TrimSpace(string(vo))
	}
	return EvaluateSetup(SetupInput{
		Settings:      settings,
		ClientVersion: client,
		SlotName:      strings.TrimSpace(srv.Slot),
		ArchiverOn:    IsOn(srv.Archiver),
		ServerName:    srv.Name,
		SSHHost:       sshHost,
	})
}

func unreadable(err error, stderr []byte) SetupCheck {
	msg := strings.TrimSpace(string(stderr))
	var re *RunError
	if msg == "" && errors.As(err, &re) {
		msg = strings.TrimSpace(re.Stderr)
	}
	if msg == "" {
		msg = err.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return SetupCheck{
		Key: SetupKeyUnreadable, Name: "Server settings", Level: SetupError,
		Value: "not readable",
		Why:   "Could not read the server settings: " + msg,
		Fix:   "Check that the server is reachable from the pgCellar host, and that the barman user's ~/.pgpass holds the password for the conninfo of this server (host:port:database:user:password, mode 0600).",
	}
}

// alter builds the fix of a parameter: the SQL, and a reload when the
// parameter does not need a restart.
func alter(name, value string, restart bool) string {
	s := "ALTER SYSTEM SET " + name + " = " + value + ";"
	if !restart {
		s += "\nSELECT pg_reload_conf();"
	}
	return s
}

// ArchiveCommand is the archive_command advised for a server: barman-wal-archive
// sends each WAL file to pgCellar's Barman over SSH (package barman-cli on the
// PostgreSQL server). host is how PostgreSQL reaches the pgCellar host.
func ArchiveCommand(host, server string) string {
	if strings.TrimSpace(host) == "" {
		host = "<pgcellar host>"
	}
	if strings.TrimSpace(server) == "" {
		server = "<server>"
	}
	return "barman-wal-archive " + host + " " + server + " %p"
}

const insufficientPrivilege = "<insufficient privilege>"

func archiveCommandCheck(in SetupInput, s PGSetting) SetupCheck {
	cmd := strings.TrimSpace(s.Value)
	c := SetupCheck{Key: "archive_command", Name: "archive_command", Value: cmd, Restart: needsRestart(s)}
	want := ArchiveCommand(in.SSHHost, in.ServerName)
	fix := alter("archive_command", "'"+want+"'", c.Restart)
	switch {
	case !in.ArchiverOn:
		c.Level, c.Why = SetupInfo, "Not used: WAL reaches Barman by streaming, not by archive_command."
	case cmd == insufficientPrivilege:
		c.Level, c.Value = SetupInfo, "hidden"
		c.Why = "The role pgCellar connects with may not read archive_command. Grant it pg_read_all_settings (or pg_monitor) so that pgCellar can check it."
		c.Fix = "GRANT pg_read_all_settings TO <role>;\n-- the command to expect:\n" + fix
	case cmd == "" || cmd == "(disabled)" || (!strings.Contains(cmd, "barman-wal-archive") && !strings.Contains(cmd, "/incoming/")):
		if cmd == "" {
			c.Value = "(empty)"
		}
		c.Level, c.Expected = SetupError, "barman-wal-archive "+orPlaceholder(in.SSHHost, "<pgcellar host>")+" "+orPlaceholder(in.ServerName, "<server>")+" %p"
		c.Why = "This server is backed up with archiver = on but PostgreSQL does not send its WAL files to Barman: archive_command is empty or is not barman-wal-archive."
		c.Fix = fix + "\n-- barman-cli must be installed on the PostgreSQL server, and the SSH key of pgCellar authorised for the postgres user (page SSH key)."
	default:
		c.Level, c.Why = SetupInfo, "PostgreSQL sends WAL files to Barman with this command. pgCellar cannot test it from here: run switch-wal --archive on the server page and look at the WAL archive check."
		c.Fix = fix
	}
	return c
}

func orPlaceholder(v, p string) string {
	if strings.TrimSpace(v) == "" {
		return p
	}
	return v
}

func needsRestart(s PGSetting) bool { return s.Context == "postmaster" }

func shown(s PGSetting) string {
	if s.Unit != "" {
		return s.Value + " " + s.Unit
	}
	return s.Value
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

// EvaluateSetup applies the rules to the settings read on the server.
func EvaluateSetup(in SetupInput) []SetupCheck {
	var out []SetupCheck
	get := func(n string) (PGSetting, bool) {
		s, ok := in.Settings[n]
		return s, ok && strings.TrimSpace(s.Value) != ""
	}

	if s, ok := get("wal_level"); ok {
		c := SetupCheck{Key: "wal_level", Name: "wal_level", Value: s.Value, Expected: "replica or logical", Restart: needsRestart(s)}
		if s.Value == "replica" || s.Value == "logical" {
			c.Level, c.Why = SetupOK, "WAL carries enough information for backups and recovery."
		} else {
			c.Level, c.Why = SetupError, "With wal_level = minimal the WAL cannot be used for a backup or a recovery."
			c.Fix = alter("wal_level", "'replica'", c.Restart)
		}
		out = append(out, c)
	}

	if s, ok := get("max_wal_senders"); ok {
		c := SetupCheck{Key: "max_wal_senders", Name: "max_wal_senders", Value: s.Value, Expected: "2 or more", Restart: needsRestart(s)}
		if n, _ := atoi(s.Value); n >= 2 {
			c.Level, c.Why = SetupOK, "Enough WAL sender processes for streaming and a base backup at the same time."
		} else {
			c.Level, c.Why = SetupError, "Barman streams WAL and takes the base backup over two replication connections; with fewer than 2 senders streaming is impossible."
			c.Fix = alter("max_wal_senders", "10", c.Restart)
		}
		out = append(out, c)
	}

	if s, ok := get("max_replication_slots"); ok {
		c := SetupCheck{Key: "max_replication_slots", Name: "max_replication_slots", Value: s.Value, Restart: needsRestart(s)}
		n, _ := atoi(s.Value)
		switch {
		case in.SlotName == "":
			c.Level, c.Expected, c.Why = SetupInfo, "not required", "No slot_name in the Barman server file, so no replication slot is needed."
		case n >= 1:
			c.Level, c.Expected, c.Why = SetupOK, "1 or more", "Room for the replication slot "+in.SlotName+"."
		default:
			c.Level, c.Expected = SetupError, "1 or more"
			c.Why = "The server file names the slot " + in.SlotName + " but PostgreSQL has no room for a replication slot."
			c.Fix = alter("max_replication_slots", "10", c.Restart)
		}
		out = append(out, c)
	}

	if s, ok := get("archive_mode"); ok {
		c := SetupCheck{Key: "archive_mode", Name: "archive_mode", Value: s.Value, Restart: needsRestart(s)}
		switch {
		case !in.ArchiverOn:
			c.Level, c.Why = SetupInfo, "Not used: WAL reaches Barman by streaming, not by archive_command."
		case s.Value == "on" || s.Value == "always":
			c.Level, c.Expected, c.Why = SetupOK, "on", "PostgreSQL archives WAL; also set archive_command to barman-wal-archive."
		default:
			c.Level, c.Expected = SetupError, "on"
			c.Why = "This server is backed up with archiver = on (archive_command) but PostgreSQL does not archive WAL."
			c.Fix = alter("archive_mode", "'on'", c.Restart) + "\n-- archive_mode needs a restart of PostgreSQL; a reload is not enough.\n" +
				alter("archive_command", "'"+ArchiveCommand(in.SSHHost, in.ServerName)+"'", false)
		}
		out = append(out, c)
	}

	if s, ok := in.Settings["archive_command"]; ok {
		out = append(out, archiveCommandCheck(in, s))
	}

	if s, ok := get("archive_timeout"); ok {
		c := SetupCheck{Key: "archive_timeout", Name: "archive_timeout", Value: shown(s), Expected: "more than 0, for example 1h", Restart: needsRestart(s)}
		if n, _ := atoi(s.Value); n > 0 {
			c.Level, c.Why = SetupOK, "A WAL file is closed at least that often, so the recovery point is bounded."
		} else {
			c.Level = SetupWarning
			c.Why = "A quiet database never fills a WAL file; set archive_timeout = '1h' for a bounded recovery point."
			c.Fix = alter("archive_timeout", "'1h'", c.Restart)
		}
		out = append(out, c)
	}

	if s, ok := get("max_slot_wal_keep_size"); ok {
		c := SetupCheck{Key: "max_slot_wal_keep_size", Name: "max_slot_wal_keep_size", Value: shown(s), Expected: "a limit, for example 2GB", Restart: needsRestart(s)}
		if s.Value == "-1" {
			c.Value = "-1 (no limit)"
		}
		switch {
		case s.Value != "-1":
			c.Level, c.Why = SetupOK, "The WAL kept for a slot is capped."
		case in.SlotName == "":
			c.Level, c.Why = SetupInfo, "No slot_name in the Barman server file, so no slot holds WAL back."
		default:
			c.Level = SetupWarning
			c.Why = "If the backup host is down for long, WAL kept by the slot can fill the disk; set 2GB (or more)."
			c.Fix = alter("max_slot_wal_keep_size", "'2GB'", c.Restart)
		}
		out = append(out, c)
	}

	if s, ok := get("full_page_writes"); ok {
		c := SetupCheck{Key: "full_page_writes", Name: "full_page_writes", Value: s.Value, Expected: "on", Restart: needsRestart(s)}
		if s.Value == "off" {
			c.Level = SetupError
			c.Why = "With full_page_writes off a backup taken while the server runs can be inconsistent and unrecoverable."
			c.Fix = alter("full_page_writes", "on", c.Restart)
		} else {
			c.Level, c.Why = SetupOK, "Backups taken while the server runs are consistent."
		}
		out = append(out, c)
	}

	if s, ok := get("data_checksums"); ok {
		c := SetupCheck{Key: "data_checksums", Name: "data_checksums", Value: s.Value, Expected: "on"}
		if s.Value == "on" {
			c.Level, c.Why = SetupOK, "PostgreSQL detects corrupted data pages."
		} else {
			c.Level, c.Restart = SetupWarning, true
			c.Why = "Without checksums a corrupted data page goes unnoticed, in the backup as well; restore drills cannot tell."
			c.Fix = "This is not a parameter. Enable it when the cluster is created (initdb --data-checksums),\nor on a stopped cluster: pg_checksums --enable -D <data directory>, then start PostgreSQL again."
		}
		out = append(out, c)
	}

	out = append(out, versionCheck(in))

	if s, ok := get("wal_compression"); ok {
		out = append(out, SetupCheck{Key: "wal_compression", Name: "wal_compression", Level: SetupInfo, Value: s.Value,
			Why: "Compresses full-page images in the WAL: smaller WAL, a little more CPU. Optional."})
	}

	if s, ok := get("summarize_wal"); ok {
		c := SetupCheck{Key: "summarize_wal", Name: "summarize_wal", Level: SetupInfo, Value: s.Value, Restart: needsRestart(s)}
		if s.Value == "on" {
			c.Why = "WAL summaries are kept: incremental backups are available."
		} else {
			c.Why = "off: incremental backups unavailable. Turn it on to use them."
			c.Fix = alter("summarize_wal", "on", c.Restart)
		}
		out = append(out, c)
	}
	return out
}

var clientVersionRE = regexp.MustCompile(`\)\s+(\d+)(?:\.(\d+))?`)

func versionCheck(in SetupInput) SetupCheck {
	c := SetupCheck{Key: "client_version", Name: "Server and client tools versions"}
	num, _ := atoi(in.Settings["server_version_num"].Value)
	server := fmt.Sprintf("%d.%d", num/10000, num%10000)
	if num < 100000 {
		server = fmt.Sprintf("%d.%d.%d", num/10000, num/100%100, num%100)
	}
	smajor := num / 10000
	m := clientVersionRE.FindStringSubmatch(in.ClientVersion)
	if m == nil {
		c.Level, c.Value = SetupInfo, "server "+server+", client tools unknown"
		c.Why = "pg_basebackup --version could not be read, so the tools could not be compared with the server."
		return c
	}
	cmajor, _ := strconv.Atoi(m[1])
	client := m[1]
	if m[2] != "" {
		client += "." + m[2]
	}
	c.Value = "server " + server + ", client tools " + client
	c.Expected = "client tools at least as recent as the server"
	if smajor > cmajor {
		c.Level = SetupError
		c.Why = fmt.Sprintf("pg_basebackup %s cannot back up a PostgreSQL %d server.", client, smajor)
		c.Fix = fmt.Sprintf("Use client tools %d or newer on the Barman host: install postgresql-client-%d (in the Docker image, a build argument), or point path_prefix in the server file to their bin directory.", smajor, smajor)
	} else {
		c.Level, c.Why = SetupOK, "The client tools can talk to this server."
	}
	return c
}

// streamingUser extracts user=... from a conninfo.
func streamingUser(conninfo string) string {
	m := regexp.MustCompile(`(?i)\buser\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`).FindStringSubmatch(conninfo)
	if m == nil {
		return "<user>"
	}
	return strings.Trim(m[1], "'")
}

// DerivedSetup builds the three lines that come from the cached `barman check`
// results (nothing is run). A check that Barman does not report (no streaming
// on an rsync server) gives no line.
func DerivedSetup(checks []CheckResult, srv Server) []SetupCheck {
	spec := []struct {
		check, key, name string
	}{
		{"postgresql_streaming", SetupKeyStreaming, "Streaming access"},
		{"superuser_or_standard_user_with_backup_privileges", SetupKeyPrivileges, "Role privileges"},
		{"replication_slot", SetupKeySlot, "Replication slot"},
		{"wal_archive", SetupKeyBarmanWALArchive, "WAL archive (Barman check)"},
		{"archive_command", SetupKeyBarmanArchiveCommand, "archive_command (Barman check)"},
		{"archive_mode", SetupKeyBarmanArchiveMode, "archive_mode (Barman check)"},
	}
	var out []SetupCheck
	for _, sp := range spec {
		for _, ch := range checks {
			if normalizeKey(ch.Name) != sp.check {
				continue
			}
			what, fix := Explain(ch)
			c := SetupCheck{Key: sp.key, Name: sp.name, Expected: "check passes", Why: what}
			if ch.OK {
				c.Level, c.Value = SetupOK, "OK"
			} else {
				c.Level, c.Value, c.Fix = SetupError, "FAILED", fix
				if sp.key == SetupKeyStreaming {
					c.Fix += "\nIn pg_hba.conf allow the replication connection, for example:\nhost replication " +
						streamingUser(srv.StreamingConnInfo) + " <barman host>/32 scram-sha-256\nthen run: SELECT pg_reload_conf();"
				}
			}
			out = append(out, c)
			break
		}
	}
	return out
}

// WorstSetup returns the level of a list: error, warning or ok (info counts as ok).
func WorstSetup(list []SetupCheck) string {
	worst := SetupOK
	for _, c := range list {
		switch c.Level {
		case SetupError:
			return SetupError
		case SetupWarning:
			worst = SetupWarning
		}
	}
	return worst
}

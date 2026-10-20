package barman

import "strings"

// explanation describes one standard `barman check` line.
type explanation struct {
	label string // readable name
	what  string // what the check verifies, one sentence (shown when it passes)
	bad   string // what is wrong when it fails, one sentence
	fix   string // where to look when it fails
}

// mangleKey reproduces the way Barman turns a check name into a JSON key.
func mangleKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return strings.ReplaceAll(s, ".", "")
}

// checkTable is indexed by the JSON key of the check (see mangleKey).
var checkTable = map[string]explanation{
	"postgresql": {"PostgreSQL",
		"Barman can open a normal connection to PostgreSQL with conninfo and the server version is supported.",
		"Barman cannot open a normal connection to PostgreSQL with conninfo, or the server version is not supported.",
		"Check conninfo (host, port, user, dbname), pg_hba.conf on the PostgreSQL server, the firewall, and that PostgreSQL is running. Try the same connection string with psql as the barman user."},
	"superuser_or_standard_user_with_backup_privileges": {"Backup privileges",
		"The PostgreSQL user in conninfo is a superuser or has the privileges needed to call the backup functions.",
		"The PostgreSQL user in conninfo is neither a superuser nor granted the privileges needed to call the backup functions.",
		"Make the user a superuser, or grant it pg_read_all_settings, pg_read_all_stats, pg_checkpoint and EXECUTE on the backup functions as described in the Barman manual."},
	"is_superuser": {"Superuser",
		"The PostgreSQL user in conninfo is a superuser.",
		"The PostgreSQL user in conninfo is not a superuser.",
		"Use a superuser in conninfo, or a user with the documented backup privileges on PostgreSQL 15 and later."},
	"postgresql_streaming": {"PostgreSQL streaming connection",
		"Barman can open a replication connection with streaming_conninfo.",
		"Barman cannot open a replication connection with streaming_conninfo.",
		"Check streaming_conninfo, that the user has the REPLICATION attribute, a replication line in pg_hba.conf, and that max_wal_senders is high enough."},
	"wal_level": {"wal_level",
		"PostgreSQL wal_level is higher than minimal, so WAL files carry enough information for backups and recovery.",
		"PostgreSQL wal_level is set to minimal, so WAL files do not carry enough information for backups and recovery.",
		"Set wal_level = replica (or logical) in postgresql.conf and restart PostgreSQL."},
	"replication_slot": {"Replication slot",
		"The replication slot named by slot_name exists and is in use by Barman's WAL receiver.",
		"The replication slot named by slot_name does not exist, or it is not in use by Barman's WAL receiver.",
		"Create it with 'barman receive-wal --create-slot <server>', then run 'barman cron'."},
	"directories": {"Directories",
		"Barman can create and write the directories of this server under barman_home.",
		"Barman cannot create or write the directories of this server under barman_home.",
		"Check the owner and permissions of barman_home and of the paths set in the server file; they must belong to the barman user."},
	"configuration": {"Configuration",
		"The server configuration has no conflict, such as two servers sharing a directory.",
		"The server configuration has a conflict, such as two servers sharing a directory.",
		"Read the messages printed under this check and fix the duplicated paths or invalid options in /etc/barman.d."},
	"retention_policy_settings": {"Retention policy settings",
		"The retention_policy value is valid and Barman can enforce it.",
		"The retention_policy value is invalid, or Barman cannot enforce it.",
		"Use a form Barman accepts, for example 'RECOVERY WINDOW OF 7 DAYS' or 'REDUNDANCY 3', and check Barman's log for the exact reason."},
	"backup_maximum_age": {"Backup maximum age",
		"The latest backup is not older than last_backup_maximum_age.",
		"The latest backup is older than last_backup_maximum_age.",
		"Take a backup now, then check that the schedule (barman cron plus a backup job) runs; raise last_backup_maximum_age if the value is too tight."},
	"backup_minimum_size": {"Backup minimum size",
		"The last backup is at least as big as last_backup_minimum_size.",
		"The last backup is smaller than last_backup_minimum_size.",
		"A backup that is much smaller than expected may be incomplete: inspect it with show-backup and take a new one."},
	"wal_maximum_age": {"WAL maximum age",
		"The newest archived WAL file is not older than last_wal_maximum_age.",
		"The newest archived WAL file is older than last_wal_maximum_age.",
		"Check that WAL archiving works (archive_command or the streaming receiver) and force a segment with switch-wal --force --archive."},
	"wal_size": {"WAL size",
		"The WAL files received for this server stay below the configured limit.",
		"The WAL files received for this server exceed the configured limit.",
		"Check that retention is enforced and that old backups are being deleted; raise the limit if it is simply too low."},
	"compression_settings": {"Compression settings",
		"The compression option is supported by the tools installed on the Barman host.",
		"The compression option is not supported by the tools installed on the Barman host.",
		"Install the compressor (gzip, bzip2, lz4, zstd...) or change the compression option of the server."},
	"failed_backups": {"Failed backups",
		"There is no failed backup newer than the last good one.",
		"A backup failed after the last good one.",
		"Look at the backup log, fix the cause, then take a new backup and delete the failed one with 'barman delete'."},
	"minimum_redundancy_requirements": {"Minimum redundancy",
		"The number of backups is at least minimum_redundancy.",
		"There are fewer backups than minimum_redundancy requires.",
		"Take backups until the number is reached, or lower minimum_redundancy if it is set too high."},
	"ssh": {"SSH connection",
		"Barman can run commands on the PostgreSQL server through ssh_command without a password.",
		"Barman cannot run commands on the PostgreSQL server through ssh_command without a password.",
		"As the barman user, run the ssh_command by hand and fix keys, known_hosts and the remote user; the public key must be in authorized_keys of the PostgreSQL host."},
	"ssh_output_clean": {"SSH output",
		"The ssh connection prints nothing besides the command output.",
		"The ssh connection prints extra text besides the command output.",
		"Remove anything that writes to stdout in the remote shell startup files (.bashrc, motd banners)."},
	"primary_and_standby_have_same_system_id": {"System id coherence",
		"The primary and the standby it follows belong to the same PostgreSQL cluster.",
		"The primary and the standby it follows have different system ids, so they are not the same PostgreSQL cluster.",
		"Point primary_conninfo at the primary of this very cluster; a different system id means a different cluster."},
	"systemid_coherence": {"System id coherence",
		"The connection and the streaming connection reach the same PostgreSQL cluster.",
		"The connection and the streaming connection reach different PostgreSQL clusters.",
		"Make conninfo and streaming_conninfo point at the same server."},
	"primary_server_is_not_a_standby": {"Primary is not a standby",
		"The server named by primary_conninfo is a real primary.",
		"The server named by primary_conninfo is itself a standby, not a primary.",
		"Point primary_conninfo at the primary, not at another standby."},
	"postgresql_server_is_standby": {"Server is a standby",
		"The server is in recovery, as expected for a standby backup.",
		"The server is not in recovery, although it is configured as a standby.",
		"Check that the server is really a standby and that primary_conninfo is set only for standbys."},
	"pg_basebackup": {"pg_basebackup",
		"The pg_basebackup program is installed on the Barman host.",
		"The pg_basebackup program is not installed on the Barman host.",
		"Install the PostgreSQL client package that provides pg_basebackup, in a version at least as recent as the server."},
	"pg_basebackup_compatible": {"pg_basebackup version",
		"pg_basebackup is not older than the PostgreSQL server.",
		"pg_basebackup is older than the PostgreSQL server.",
		"Install a pg_basebackup of the same major version as the server, or newer, and set path_prefix if it lives elsewhere."},
	"pg_basebackup_supports_tablespaces_mapping": {"pg_basebackup tablespaces",
		"pg_basebackup can map tablespaces, needed when the server has some.",
		"pg_basebackup cannot map tablespaces, which this server needs.",
		"Use a more recent pg_basebackup, or remove the tablespaces from the server."},
	"pg_receivexlog": {"pg_receivewal",
		"The pg_receivewal (pg_receivexlog) program is installed on the Barman host.",
		"The pg_receivewal (pg_receivexlog) program is not installed on the Barman host.",
		"Install the PostgreSQL client package that provides pg_receivewal."},
	"pg_receivexlog_compatible": {"pg_receivewal version",
		"pg_receivewal is not older than the PostgreSQL server.",
		"pg_receivewal is older than the PostgreSQL server.",
		"Install a pg_receivewal of the same major version as the server, or newer."},
	"receive_wal_running": {"WAL receiver",
		"The process that streams WAL files from PostgreSQL is running.",
		"The process that streams WAL files from PostgreSQL is not running.",
		"Run 'barman cron' (it starts the receiver), and check that cron runs it every minute; look at the Barman log if it stops again."},
	"archive_mode": {"archive_mode",
		"PostgreSQL archive_mode is on or always, so WAL files are archived.",
		"PostgreSQL archive_mode is off, so WAL files are not archived.",
		"Set archive_mode = on in postgresql.conf and restart PostgreSQL."},
	"archive_command": {"archive_command",
		"PostgreSQL archive_command sends WAL files to Barman.",
		"PostgreSQL archive_command does not send WAL files to Barman.",
		"Set archive_command to barman-wal-archive (or the rsync command from the manual) and reload PostgreSQL."},
	"archiver_errors": {"Archiver errors",
		"No file sits in the errors directory of the WAL archive.",
		"Files sit in the errors directory of the WAL archive.",
		"Read the files in the server's errors directory, fix the cause (duplicate or corrupt WAL), then remove them."},
	"wal_archive": {"WAL archive",
		"WAL archiving is configured without conflict between the archiver and the streaming receiver.",
		"WAL archiving is not configured properly: the archiver and the streaming receiver conflict, or neither is set up.",
		"Turn on exactly one archiving mode that matches backup_method, and check that WAL files do arrive (switch-wal --force --archive)."},
	"empty_incoming_directory": {"Incoming directory",
		"The incoming WAL directory does not pile up unprocessed files.",
		"Unprocessed files are piling up in the incoming WAL directory.",
		"Run 'barman cron' to process them and check that it is scheduled every minute."},
	"empty_streaming_directory": {"Streaming directory",
		"The streaming WAL directory does not pile up unprocessed files.",
		"Unprocessed files are piling up in the streaming WAL directory.",
		"Run 'barman cron' to process them and check that it is scheduled every minute."},
	"incoming_wals_directory": {"Incoming WAL directory",
		"The incoming WAL directory is not holding too many files.",
		"The incoming WAL directory holds too many files.",
		"Run 'barman cron' to process them and check that it is scheduled every minute."},
	"streaming_wals_directory": {"Streaming WAL directory",
		"The streaming WAL directory is not holding too many files.",
		"The streaming WAL directory holds too many files.",
		"Run 'barman cron' to process them and check that it is scheduled every minute."},
	"not_in_recovery": {"Not in recovery",
		"The PostgreSQL server is not in recovery, which is required for this backup method.",
		"The PostgreSQL server is in recovery, which this backup method does not allow.",
		"Back up the primary, or use a standby-aware configuration with primary_conninfo."},
	"exclusive_backup_supported": {"Exclusive backups",
		"Exclusive backups are possible with this PostgreSQL version.",
		"Exclusive backups are not possible with this PostgreSQL version.",
		"Use backup_method = postgres or a concurrent backup mode; exclusive backups were removed in PostgreSQL 15."},
	"local_pgdata": {"Local PGDATA",
		"The data directory of a local-copy backup is reachable from the Barman host.",
		"The data directory of the local-copy backup cannot be reached from the Barman host.",
		"Check the path and permissions, or use ssh_command for a remote server."},
	"backup_label": {"backup_label",
		"No stale backup_label file is left in PGDATA from an interrupted exclusive backup.",
		"A stale backup_label file is left in PGDATA by an interrupted exclusive backup.",
		"Run pg_stop_backup() or remove the leftover backup_label after making sure no backup is running."},
	"postgres_minimal_version": {"PostgreSQL version",
		"The PostgreSQL version is recent enough for the chosen backup method.",
		"The PostgreSQL version is too old for the chosen backup method.",
		"Upgrade PostgreSQL or choose a backup method supported by your version."},
	"has_monitoring_privileges": {"Monitoring privileges",
		"The PostgreSQL user can read the monitoring views Barman uses.",
		"The PostgreSQL user cannot read the monitoring views Barman uses.",
		"Grant pg_monitor (or the individual pg_read_all_* roles) to the user in conninfo."},
	"encryption": {"Encryption",
		"The encryption options are valid and the key is available.",
		"The encryption options are invalid or the key is not available.",
		"Check the encryption settings in the server file and that the key is readable by the barman user."},
	"cloud_storage_configuration": {"Cloud storage",
		"The cloud snapshot or storage settings are complete and valid.",
		"The cloud snapshot or storage settings are incomplete or invalid.",
		"Review the snapshot_* options of the server and the credentials of the barman user."},
}

// normalizeKey maps whatever a caller holds (a JSON key, a printed check name,
// a name with a "(suffix)") to a checkTable key.
func normalizeKey(name string) string {
	k := mangleKey(name)
	if _, ok := checkTable[k]; ok {
		return k
	}
	if i := strings.Index(k, "_("); i > 0 {
		base := k[:i]
		if _, ok := checkTable[base]; ok {
			return base
		}
	}
	return k
}

// DisplayName returns a readable name for a check: a short label for the
// standard checks, the key with spaces for the others.
func DisplayName(name string) string {
	if e, ok := checkTable[normalizeKey(name)]; ok {
		return e.label
	}
	return strings.ReplaceAll(name, "_", " ")
}

// Explain returns, for a check, a one-sentence description and a pointer to
// the likely fix. A passing check gets what Barman verifies; a failing one gets
// what is wrong. Barman's own hint, when there is
// one, is appended to the fix. Unknown checks get a generic answer.
func Explain(check CheckResult) (what, howToFix string) {
	e, ok := checkTable[normalizeKey(check.Name)]
	if !ok {
		what = "Barman runs a check named \"" + DisplayName(check.Name) + "\" that is not in pgCellar's list."
		howToFix = "Run 'barman check <server>' on the Barman host and read the Barman manual entry for this check."
	} else {
		what, howToFix = e.what, e.fix
		if !check.OK {
			what = e.bad
		}
	}
	if hint := strings.TrimSpace(check.Hint); hint != "" && !check.OK {
		howToFix += " Barman says: " + hint
	}
	return what, howToFix
}

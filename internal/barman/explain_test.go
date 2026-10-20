package barman

import (
	"strings"
	"testing"
)

func TestExplain(t *testing.T) {
	what, fix := Explain(CheckResult{Name: "receive_wal_running", Hint: "See the Barman log file"})
	if !strings.Contains(what, "WAL") || !strings.Contains(fix, "barman cron") || !strings.Contains(fix, "See the Barman log file") {
		t.Errorf("what=%q fix=%q", what, fix)
	}
	// Printed names, JSON keys and suffixed names resolve to the same entry.
	for _, name := range []string{"receive-wal running", "receive_wal_running", "wal_level (standby)", "WAL archive", "PostgreSQL"} {
		if _, ok := checkTable[normalizeKey(name)]; !ok {
			t.Errorf("no explanation for %q", name)
		}
	}
	what, fix = Explain(CheckResult{Name: "something_new", Hint: "h"})
	if what == "" || !strings.Contains(fix, "h") {
		t.Errorf("fallback: %q %q", what, fix)
	}
	// An OK check does not repeat Barman's hint.
	_, fix = Explain(CheckResult{Name: "postgresql", OK: true, Hint: "irrelevant"})
	if strings.Contains(fix, "irrelevant") {
		t.Errorf("fix = %q", fix)
	}
	if DisplayName("pg_basebackup_compatible") != "pg_basebackup version" {
		t.Errorf("display = %q", DisplayName("pg_basebackup_compatible"))
	}
}

func TestStandardChecksAreExplained(t *testing.T) {
	for _, k := range []string{
		"postgresql", "superuser_or_standard_user_with_backup_privileges", "wal_level", "replication_slot",
		"directories", "retention_policy_settings", "backup_maximum_age", "compression_settings",
		"failed_backups", "minimum_redundancy_requirements", "ssh", "primary_and_standby_have_same_system_id",
		"pg_basebackup", "pg_receivexlog", "archive_mode", "archiver_errors", "receive_wal_running",
	} {
		e, ok := checkTable[k]
		if !ok || e.what == "" || e.fix == "" || e.label == "" {
			t.Errorf("check %q is missing or incomplete", k)
		}
	}
}

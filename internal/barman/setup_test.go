package barman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "real", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func levels(list []SetupCheck) map[string]string {
	m := map[string]string{}
	for _, c := range list {
		m[c.Key] = c.Level
	}
	return m
}

const clientTools = "pg_basebackup (PostgreSQL) 18.6 (Debian 18.6-1.pgdg12+2)"

// The exact answer of the setup query on the chutag test server (PostgreSQL 16).
func TestParseRealSetupOutput(t *testing.T) {
	got, err := ParseSettings(realFile(t, "psql-setup-chutag.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("%d settings: %v", len(got), got)
	}
	if s := got["max_slot_wal_keep_size"]; s.Value != "-1" || s.Unit != "MB" || s.Context != "sighup" {
		t.Errorf("max_slot_wal_keep_size = %+v", s)
	}
	if s := got["wal_level"]; s.Value != "replica" || s.Unit != "" || s.Context != "postmaster" {
		t.Errorf("wal_level = %+v", s)
	}
	if _, ok := got["summarize_wal"]; ok {
		t.Error("summarize_wal does not exist on PostgreSQL 16")
	}
}

func TestParseSettingsRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "wal_level\treplica\n", "hello world\n"} {
		if _, err := ParseSettings([]byte(in)); err == nil {
			t.Errorf("ParseSettings(%q) should fail", in)
		}
	}
}

func TestSetupOnChutag(t *testing.T) {
	settings, err := ParseSettings(realFile(t, "psql-setup-chutag.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	rows := EvaluateSetup(SetupInput{Settings: settings, ClientVersion: clientTools, SlotName: "barman_chutag"})
	checks, err := ParseCheck(realFile(t, "check-chutag.json"), "chutag")
	if err != nil {
		t.Fatal(err)
	}
	rows = append(rows, DerivedSetup(checks, Server{StreamingConnInfo: "host=10.8.0.1 port=5434 user=streaming_barman"})...)
	want := map[string]string{
		"wal_level": SetupOK, "max_wal_senders": SetupOK, "max_replication_slots": SetupOK,
		"archive_mode": SetupInfo, "archive_timeout": SetupWarning, "max_slot_wal_keep_size": SetupWarning,
		"full_page_writes": SetupOK, "data_checksums": SetupWarning, "client_version": SetupOK,
		"wal_compression": SetupInfo,
	}
	got := levels(rows)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["summarize_wal"]; ok {
		t.Error("summarize_wal must not show on PostgreSQL 16")
	}
	for _, k := range []string{SetupKeyStreaming, SetupKeyPrivileges, SetupKeySlot} {
		if got[k] != SetupOK {
			t.Errorf("%s: %q, want ok on the real check output", k, got[k])
		}
	}
	if WorstSetup(rows) != SetupWarning {
		t.Errorf("worst = %s", WorstSetup(rows))
	}
	for _, c := range rows {
		if c.Key == "client_version" && c.Value != "server 16.13, client tools 18.6" {
			t.Errorf("version value %q", c.Value)
		}
		if c.Key == "archive_timeout" && (!strings.Contains(c.Fix, "ALTER SYSTEM SET archive_timeout = '1h';") || !strings.Contains(c.Fix, "pg_reload_conf()") || c.Restart) {
			t.Errorf("archive_timeout fix: %+v", c)
		}
		if c.Key == "data_checksums" && (!c.Restart || !strings.Contains(c.Fix, "pg_checksums --enable")) {
			t.Errorf("data_checksums: %+v", c)
		}
	}
}

func set(pairs ...string) map[string]PGSetting {
	m := map[string]PGSetting{"server_version_num": {Value: "170005", Context: "internal"}}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = PGSetting{Value: pairs[i+1], Context: "sighup"}
	}
	return m
}

func TestSetupRules(t *testing.T) {
	restart := func(name, value string) map[string]PGSetting {
		m := set()
		m[name] = PGSetting{Value: value, Context: "postmaster"}
		return m
	}
	merge := func(a, b map[string]PGSetting) map[string]PGSetting {
		for k, v := range b {
			a[k] = v
		}
		return a
	}
	cases := []struct {
		name    string
		in      SetupInput
		key     string
		level   string
		restart bool
	}{
		{"wal_level minimal", SetupInput{Settings: restart("wal_level", "minimal")}, "wal_level", SetupError, true},
		{"wal_level replica", SetupInput{Settings: restart("wal_level", "replica")}, "wal_level", SetupOK, true},
		{"wal_level logical", SetupInput{Settings: restart("wal_level", "logical")}, "wal_level", SetupOK, true},
		{"senders 0", SetupInput{Settings: restart("max_wal_senders", "0")}, "max_wal_senders", SetupError, true},
		{"senders 1", SetupInput{Settings: restart("max_wal_senders", "1")}, "max_wal_senders", SetupError, true},
		{"senders 2", SetupInput{Settings: restart("max_wal_senders", "2")}, "max_wal_senders", SetupOK, true},
		{"slots 0 with slot", SetupInput{Settings: restart("max_replication_slots", "0"), SlotName: "barman"}, "max_replication_slots", SetupError, true},
		{"slots 0 without slot", SetupInput{Settings: restart("max_replication_slots", "0")}, "max_replication_slots", SetupInfo, true},
		{"slots 10 with slot", SetupInput{Settings: restart("max_replication_slots", "10"), SlotName: "barman"}, "max_replication_slots", SetupOK, true},
		{"archive off, streaming", SetupInput{Settings: restart("archive_mode", "off")}, "archive_mode", SetupInfo, true},
		{"archive off, archiver on", SetupInput{Settings: restart("archive_mode", "off"), ArchiverOn: true}, "archive_mode", SetupError, true},
		{"archive on, archiver on", SetupInput{Settings: restart("archive_mode", "on"), ArchiverOn: true}, "archive_mode", SetupOK, true},
		{"archive always, archiver on", SetupInput{Settings: restart("archive_mode", "always"), ArchiverOn: true}, "archive_mode", SetupOK, true},
		{"archive_timeout 0", SetupInput{Settings: set("archive_timeout", "0")}, "archive_timeout", SetupWarning, false},
		{"archive_timeout 3600", SetupInput{Settings: set("archive_timeout", "3600")}, "archive_timeout", SetupOK, false},
		{"keep size unlimited with slot", SetupInput{Settings: set("max_slot_wal_keep_size", "-1"), SlotName: "b"}, "max_slot_wal_keep_size", SetupWarning, false},
		{"keep size unlimited without slot", SetupInput{Settings: set("max_slot_wal_keep_size", "-1")}, "max_slot_wal_keep_size", SetupInfo, false},
		{"keep size 2048", SetupInput{Settings: set("max_slot_wal_keep_size", "2048"), SlotName: "b"}, "max_slot_wal_keep_size", SetupOK, false},
		{"full_page_writes off", SetupInput{Settings: set("full_page_writes", "off")}, "full_page_writes", SetupError, false},
		{"full_page_writes on", SetupInput{Settings: set("full_page_writes", "on")}, "full_page_writes", SetupOK, false},
		{"checksums off", SetupInput{Settings: set("data_checksums", "off")}, "data_checksums", SetupWarning, true},
		{"checksums on", SetupInput{Settings: set("data_checksums", "on")}, "data_checksums", SetupOK, false},
		{"wal_compression", SetupInput{Settings: set("wal_compression", "zstd")}, "wal_compression", SetupInfo, false},
		{"summarize_wal off", SetupInput{Settings: set("summarize_wal", "off")}, "summarize_wal", SetupInfo, false},
		{"summarize_wal on", SetupInput{Settings: set("summarize_wal", "on")}, "summarize_wal", SetupInfo, false},
		{"client older than server", SetupInput{Settings: merge(set(), map[string]PGSetting{"server_version_num": {Value: "180002"}}), ClientVersion: "pg_basebackup (PostgreSQL) 16.4"}, "client_version", SetupError, false},
		{"client same major", SetupInput{Settings: set(), ClientVersion: "pg_basebackup (PostgreSQL) 17.1"}, "client_version", SetupOK, false},
		{"client newer", SetupInput{Settings: set(), ClientVersion: clientTools}, "client_version", SetupOK, false},
		{"client unknown", SetupInput{Settings: set()}, "client_version", SetupInfo, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got *SetupCheck
			rows := EvaluateSetup(c.in)
			for i := range rows {
				if rows[i].Key == c.key {
					got = &rows[i]
				}
			}
			if got == nil {
				t.Fatalf("no %s line in %v", c.key, levels(rows))
			}
			if got.Level != c.level {
				t.Errorf("level %q, want %q (%+v)", got.Level, c.level, got)
			}
			if got.Restart != c.restart {
				t.Errorf("restart %v, want %v", got.Restart, c.restart)
			}
			if got.Level == SetupError || got.Level == SetupWarning {
				if got.Fix == "" || got.Why == "" {
					t.Errorf("a problem needs a why and a fix: %+v", got)
				}
			}
		})
	}
	// Settings that PostgreSQL does not have give no line.
	for _, c := range EvaluateSetup(SetupInput{Settings: set()}) {
		if c.Key == "summarize_wal" || c.Key == "max_slot_wal_keep_size" {
			t.Errorf("%s should be absent", c.Key)
		}
	}
	if w := WorstSetup([]SetupCheck{{Level: SetupInfo}, {Level: SetupOK}}); w != SetupOK {
		t.Errorf("info/ok worst = %s", w)
	}
	if w := WorstSetup([]SetupCheck{{Level: SetupWarning}, {Level: SetupOK}}); w != SetupWarning {
		t.Errorf("warning worst = %s", w)
	}
	if w := WorstSetup([]SetupCheck{{Level: SetupWarning}, {Level: SetupError}}); w != SetupError {
		t.Errorf("error worst = %s", w)
	}
}

func TestDerivedSetup(t *testing.T) {
	srv := Server{StreamingConnInfo: "host=db user=streaming_barman dbname=postgres"}
	rows := DerivedSetup([]CheckResult{
		{Name: "PostgreSQL streaming", OK: false, Hint: "FATAL: no pg_hba.conf entry"},
		{Name: "superuser or standard user with backup privileges", OK: true},
		{Name: "replication slot", OK: false},
		{Name: "directories", OK: false},
	}, srv)
	got := levels(rows)
	if len(rows) != 3 || got[SetupKeyStreaming] != SetupError || got[SetupKeyPrivileges] != SetupOK || got[SetupKeySlot] != SetupError {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Key == SetupKeyStreaming && !strings.Contains(r.Fix, "host replication streaming_barman <barman host>/32 scram-sha-256") {
			t.Errorf("streaming fix lacks the pg_hba line: %s", r.Fix)
		}
		if r.Key == SetupKeySlot && !strings.Contains(r.Fix, "--create-slot") {
			t.Errorf("slot fix: %s", r.Fix)
		}
	}
	if got := DerivedSetup([]CheckResult{{Name: "directories", OK: true}}, srv); len(got) != 0 {
		t.Errorf("no derived line expected, got %v", got)
	}
}

func TestPsqlConnInfoNeverCarriesAPassword(t *testing.T) {
	for in, want := range map[string]string{
		"host=db user=barman dbname=postgres":                 "host=db user=barman dbname=postgres connect_timeout=10",
		"host=db user=barman password=s3cret dbname=postgres": "host=db user=barman dbname=postgres connect_timeout=10",
		"host=db password='a b' user=barman":                  "host=db user=barman connect_timeout=10",
		"host=db user=barman connect_timeout=3 password=x":    "host=db user=barman connect_timeout=3",
		"postgresql://barman:pw@db/postgres":                  "postgresql://barman:pw@db/postgres",
	} {
		got := PsqlConnInfo(in)
		if got != want {
			t.Errorf("PsqlConnInfo(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "s3cret") || strings.Contains(got, "a b") {
			t.Errorf("password left in %q", got)
		}
	}
	args := PsqlArgs("host=db user=barman")
	if args[0] == "" || !strings.Contains(strings.Join(args, " "), "-X -At -F \t -v ON_ERROR_STOP=1 -c select") {
		t.Errorf("args = %q", args)
	}
	if q := SetupQuery(); !strings.HasPrefix(q, "select ") || strings.Contains(strings.ToLower(q), "update") {
		t.Errorf("query = %s", q)
	}
}

func TestReadSetupThroughTheAuditedRunner(t *testing.T) {
	useFakeBarman(t)
	var entries []AuditEntry
	r := CLIRunner{Audit: func(e AuditEntry) { entries = append(entries, e) }}
	tools := SetupTools{Psql: "fake-psql", Basebackup: "fake-pg_basebackup"}

	rows := ReadSetup(WithActor(context.Background(), "tester"), r, tools, Server{Name: "pg-main", ConnInfo: "host=pg-main.example.com user=barman password=nope dbname=postgres"})
	got := levels(rows)
	if got["wal_level"] != SetupOK || got["archive_timeout"] != SetupWarning || got["client_version"] != SetupOK || got["summarize_wal"] != SetupInfo {
		t.Fatalf("rows = %v", got)
	}
	if len(entries) != 2 {
		t.Fatalf("%d audit entries, want psql and pg_basebackup", len(entries))
	}
	e := entries[0]
	if e.Binary != "fake-psql" || e.Target != "pg-main" || e.Actor != "tester" || e.ExitCode != 0 {
		t.Errorf("entry = %+v", e)
	}
	if strings.Contains(e.Command, "nope") || !strings.Contains(e.Command, "ON_ERROR_STOP=1") {
		t.Errorf("command = %q", e.Command)
	}

	// An unreachable server gives one error line and nothing else.
	rows = ReadSetup(context.Background(), r, tools, Server{Name: "x", ConnInfo: "host=nowhere.example.com user=barman"})
	if len(rows) != 1 || rows[0].Key != SetupKeyUnreadable || rows[0].Level != SetupError || !strings.HasPrefix(rows[0].Why, "Could not read the server settings: psql: error: connection to server") {
		t.Errorf("rows = %+v", rows)
	}
}

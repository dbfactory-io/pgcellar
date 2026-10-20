package barman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const realisticServer = `; Managed by hand, keep this header.
# Main production database

[pg-main]
description =  "Main database"
active = true
conninfo = host=db1.example.com user=barman dbname=postgres
streaming_conninfo=host=db1.example.com user=streaming_barman
backup_method = postgres
streaming_archiver = on
# one slot per server
slot_name = barman_pg_main
retention_policy = RECOVERY WINDOW OF 7 DAYS
last_backup_maximum_age = 1 DAYS
path_prefix = /usr/pgsql-16/bin
  /opt/extra/bin

[pg-second]
# second section in the same file
ssh_command = ssh postgres@db2
conninfo = host=db2 user=barman
archiver = on
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func findServer(t *testing.T, list []Server, name string) Server {
	t.Helper()
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("server %q not found", name)
	return Server{}
}

func TestLoadServers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg-main.conf"), realisticServer)
	writeFile(t, filepath.Join(dir, "ignored.txt"), "[x]\na=b\n")

	list, err := LoadServers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 servers, got %d", len(list))
	}
	s := findServer(t, list, "pg-main")
	if s.ConnInfo != "host=db1.example.com user=barman dbname=postgres" {
		t.Errorf("conninfo = %q", s.ConnInfo)
	}
	if s.StreamingConnInfo != "host=db1.example.com user=streaming_barman" {
		t.Errorf("streaming_conninfo = %q", s.StreamingConnInfo)
	}
	if s.Slot != "barman_pg_main" || s.BackupMethod != "postgres" || s.StreamingArchiver != "on" {
		t.Errorf("unexpected typed fields: %+v", s)
	}
	if s.Extra["last_backup_maximum_age"] != "1 DAYS" || s.Extra["active"] != "true" {
		t.Errorf("extra = %v", s.Extra)
	}
	if !strings.HasPrefix(s.Extra["path_prefix"], "/usr/pgsql-16/bin") {
		t.Errorf("continuation line lost: %q", s.Extra["path_prefix"])
	}
	if s.Order[0] != "description" || s.File != filepath.Join(dir, "pg-main.conf") {
		t.Errorf("order/file = %v %q", s.Order, s.File)
	}
	if findServer(t, list, "pg-second").SSHCommand != "ssh postgres@db2" {
		t.Error("second section not read")
	}
}

func TestLoadServersMissingDir(t *testing.T) {
	list, err := LoadServers(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(list) != 0 {
		t.Fatalf("got %v, %v", list, err)
	}
}

func TestLoadGlobal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "barman.conf")
	writeFile(t, p, `# global
[barman]
barman_user = barman
barman_home = /var/lib/barman
log_file = /var/log/barman/barman.log
configuration_files_directory = /etc/barman.d
compression = gzip
`)
	g, err := LoadGlobal(p)
	if err != nil {
		t.Fatal(err)
	}
	if g.BarmanUser != "barman" || g.BarmanHome != "/var/lib/barman" || g.ConfigurationFilesDirectory != "/etc/barman.d" {
		t.Errorf("global = %+v", g)
	}
	if g.Extra["compression"] != "gzip" {
		t.Errorf("extra = %v", g.Extra)
	}
	writeFile(t, p, "[other]\na=b\n")
	if _, err := LoadGlobal(p); err == nil {
		t.Error("missing [barman] section must be an error")
	}
}

func TestSaveRoundTripIdentical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg-main.conf")
	writeFile(t, path, realisticServer)
	list, _ := LoadServers(dir)
	s := findServer(t, list, "pg-main")

	if err := SaveServer(s); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != realisticServer {
		t.Fatalf("unchanged save altered the file:\n%s", got)
	}

	s.RetentionPolicy = "RECOVERY WINDOW OF 14 DAYS"
	if err := SaveServer(s); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	want := strings.Replace(realisticServer, "RECOVERY WINDOW OF 7 DAYS", "RECOVERY WINDOW OF 14 DAYS", 1)
	if string(got) != want {
		t.Fatalf("only retention_policy should change:\n%s", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}
}

func TestSaveSecondSectionUntouchedFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg-main.conf")
	writeFile(t, path, realisticServer)
	list, _ := LoadServers(dir)
	s := findServer(t, list, "pg-second")
	s.MinimumRedundancy = "2"
	s.Archiver = "" // removed
	if err := SaveServer(s); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	cut := strings.Index(realisticServer, "[pg-second]")
	if !strings.HasPrefix(string(got), realisticServer[:cut]) {
		t.Errorf("first section changed:\n%s", got)
	}
	second := string(got)[cut:]
	want := "[pg-second]\n# second section in the same file\nssh_command = ssh postgres@db2\nconninfo = host=db2 user=barman\nminimum_redundancy = 2\n"
	if second != want {
		t.Errorf("second section:\n%s", second)
	}
}

func TestSaveNewServer(t *testing.T) {
	dir := t.TempDir()
	s := Server{
		Name:              "pg-new",
		Description:       "New one",
		BackupMethod:      "postgres",
		ConnInfo:          "host=n user=barman",
		StreamingConnInfo: "host=n user=streaming_barman",
		StreamingArchiver: "on",
		Slot:              "barman_new",
		Extra:             map[string]string{"path_prefix": "/usr/bin"},
	}
	if err := SaveNewServer(dir, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pg-new.conf"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[pg-new]\ndescription = New one\nbackup_method = postgres\nconninfo = host=n user=barman\nstreaming_conninfo = host=n user=streaming_barman\nstreaming_archiver = on\nslot_name = barman_new\npath_prefix = /usr/bin\n"
	if string(data) != want {
		t.Errorf("new file:\n%s", data)
	}
	if err := SaveNewServer(dir, s); err == nil {
		t.Error("second creation must refuse to overwrite")
	}
	if err := SaveNewServer(dir, Server{Name: "../evil"}); err == nil {
		t.Error("path traversal in name must be refused")
	}
	if err := SaveServer(Server{Name: "x"}); err == nil {
		t.Error("SaveServer without File must fail")
	}
	back, _ := LoadServers(dir)
	if len(back) != 1 || back[0].Slot != "barman_new" {
		t.Errorf("reload = %+v", back)
	}
}

func TestValidate(t *testing.T) {
	good := Server{Name: "a", BackupMethod: "postgres", ConnInfo: "x", StreamingConnInfo: "y", StreamingArchiver: "on", Slot: "s"}
	if p := Validate(good); len(p) != 0 {
		t.Errorf("good postgres server: %v", p)
	}
	goodR := Server{Name: "a", BackupMethod: "rsync", SSHCommand: "ssh x", ConnInfo: "x", Archiver: "on"}
	if p := Validate(goodR); len(p) != 0 {
		t.Errorf("good rsync server: %v", p)
	}
	// Barman's default method is rsync.
	if p := Validate(Server{Name: "a"}); len(p) != 3 {
		t.Errorf("empty server (rsync default): %v", p)
	}
	p := Validate(Server{Name: "a", BackupMethod: "postgres"})
	if len(p) != 4 {
		t.Fatalf("empty postgres server: %v", p)
	}
	for _, want := range []string{"conninfo", "streaming_conninfo", "streaming_archiver = on", "slot_name"} {
		found := false
		for _, line := range p {
			if strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no problem mentions %q: %v", want, p)
		}
	}
	if p := Validate(Server{Name: "a", BackupMethod: "postgres", ConnInfo: "x", StreamingConnInfo: "y", StreamingArchiver: "off", Slot: "s"}); len(p) != 1 {
		t.Errorf("streaming_archiver off: %v", p)
	}
	if p := Validate(Server{Name: "a b", BackupMethod: "tape"}); len(p) != 2 {
		t.Errorf("bad name and method: %v", p)
	}
}

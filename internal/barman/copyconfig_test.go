package barman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCopyConfig(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	_ = os.MkdirAll(src, 0o750)
	conf := "[pg-main]\ndescription = Main\nconninfo = host=db1 user=barman\nbackup_method = postgres\nwals_directory = /mnt/fast/wals\npost_backup_script = /usr/local/bin/notify\nstreaming_archiver = on\n"
	if err := os.WriteFile(filepath.Join(src, "pg-main.conf"), []byte(conf), 0o640); err != nil {
		t.Fatal(err)
	}
	servers, err := LoadServers(src)
	if err != nil || len(servers) != 1 {
		t.Fatal(servers, err)
	}
	global := &GlobalConfig{Extra: map[string]string{"compression": "gzip", "pre_recovery_script": "/bin/false", "path_prefix": "/usr/lib/postgresql/16/bin"}}
	out := filepath.Join(dir, "copy")
	path, err := WriteCopyConfig(out, "/restore/var/lib/barman", "barman", global, &servers[0], "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := os.ReadFile(path)
	s, _ := os.ReadFile(filepath.Join(out, "barman.d", "pg-main.conf"))
	for _, want := range []string{"barman_home = /restore/var/lib/barman", "barman_user = barman", "configuration_files_directory = " + filepath.Join(out, "barman.d"), "compression = gzip", "path_prefix = "} {
		if !strings.Contains(string(g), want) {
			t.Errorf("barman.conf lacks %q:\n%s", want, g)
		}
	}
	if strings.Contains(string(g), "pre_recovery_script") {
		t.Error("a hook reached barman.conf")
	}
	for _, want := range []string{"[pg-main]", "description = Main", "conninfo = host=db1 user=barman", "streaming_archiver = on"} {
		if !strings.Contains(string(s), want) {
			t.Errorf("server file lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(string(s), "wals_directory") || strings.Contains(string(s), "post_backup_script") {
		t.Errorf("a live path or a hook reached the server file:\n%s", s)
	}
	// Without a copied barman.d file, a minimal section.
	if _, err := WriteCopyConfig(filepath.Join(dir, "bare"), "/h", "", nil, nil, "pg-x"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bare", "barman.d", "pg-x.conf")); !strings.Contains(string(b), "conninfo = ") {
		t.Errorf("minimal section: %s", b)
	}
}

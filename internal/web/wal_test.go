package web

import (
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stranix79/pgcellar/internal/sshkeys"
)

func serverForm(csrf, name, method, wal, ssh string) url.Values {
	v := url.Values{
		"csrf": {csrf}, "name": {name}, "backup_method": {method}, "wal_method": {wal},
		"conninfo":           {"host=" + name + " user=barman dbname=postgres"},
		"streaming_conninfo": {"host=" + name + " user=streaming_barman dbname=postgres"},
		"slot_name":          {"barman"}, "ssh_command": {ssh},
		"recovery_window_days": {"7"}, "minimum_redundancy": {"1"},
	}
	return v
}

func readConf(t *testing.T, f *fixture, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.env.BarmanDir, name+".conf"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWALMethodsWriteTheRightKeys(t *testing.T) {
	f := newFixture(t)
	admin, csrf := f.login(t, "admin")

	cases := []struct {
		name, method, wal, ssh string
		want, absent           []string
	}{
		{"w-stream", "postgres", "streaming", "", []string{"streaming_archiver = on", "archiver = off"}, nil},
		{"w-archive", "postgres", "archive_command", "", []string{"streaming_archiver = off", "archiver = on"}, []string{"create_slot"}},
		{"w-both", "postgres", "both", "", []string{"streaming_archiver = on", "archiver = on", "create_slot = auto"}, nil},
		{"w-rsync-both", "rsync", "both", "ssh postgres@db1", []string{"streaming_archiver = on", "archiver = on", "ssh_command = ssh postgres@db1"}, nil},
		{"w-rsync-archive", "rsync", "archive_command", "ssh postgres@db1", []string{"streaming_archiver = off", "archiver = on"}, nil},
	}
	for _, c := range cases {
		rec := f.post("/servers", admin, serverForm(csrf, c.name, c.method, c.wal, c.ssh))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
		conf := readConf(t, f, c.name)
		for _, w := range c.want {
			if !strings.Contains(conf, w+"\n") {
				t.Errorf("%s: missing %q in\n%s", c.name, w, conf)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(conf, a) {
				t.Errorf("%s: unexpected %q in\n%s", c.name, a, conf)
			}
		}
		// The edit form shows the method back.
		page := f.get("/servers/"+c.name, admin).Body.String()
		if !strings.Contains(page, `value="`+c.wal+`" selected`) {
			t.Errorf("%s: the form does not select %s", c.name, c.wal)
		}
	}

	// Switching an existing server rewrites only the two keys.
	rec := f.post("/servers/w-stream", admin, serverForm(csrf, "w-stream", "postgres", "both", ""))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("switch to both: %d %s", rec.Code, rec.Body.String())
	}
	if conf := readConf(t, f, "w-stream"); !strings.Contains(conf, "archiver = on\n") || !strings.Contains(conf, "streaming_archiver = on\n") {
		t.Errorf("after switch:\n%s", conf)
	}
	// The form of a pre-WAL-method client (no wal_method) keeps the file as it is.
	old := serverForm(csrf, "w-stream", "postgres", "", "")
	old.Del("wal_method")
	if rec := f.post("/servers/w-stream", admin, old); rec.Code != http.StatusSeeOther {
		t.Fatalf("no wal_method: %d", rec.Code)
	}
	if conf := readConf(t, f, "w-stream"); !strings.Contains(conf, "archiver = on\n") || !strings.Contains(conf, "streaming_archiver = on\n") {
		t.Errorf("an omitted wal_method must keep the file:\n%s", conf)
	}
}

func TestWALMethodRefusals(t *testing.T) {
	f := newFixture(t)
	admin, csrf := f.login(t, "admin")
	for name, form := range map[string]url.Values{
		"r-archive-nossh": serverForm(csrf, "r-archive-nossh", "rsync", "archive_command", ""),
		"r-both-nossh":    serverForm(csrf, "r-both-nossh", "rsync", "both", ""),
		"r-streaming":     serverForm(csrf, "r-streaming", "rsync", "streaming", "ssh postgres@db1"),
		"p-nonsense":      serverForm(csrf, "p-nonsense", "postgres", "carrier-pigeon", ""),
	} {
		rec := f.post("/servers", admin, form)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, rec.Code)
		}
		if _, err := os.Stat(filepath.Join(f.env.BarmanDir, name+".conf")); err == nil {
			t.Errorf("%s: a refused form must not write a file", name)
		}
	}
	rec := f.post("/servers", admin, serverForm(csrf, "r-archive-nossh", "rsync", "archive_command", ""))
	if !strings.Contains(rec.Body.String(), "ssh_command") {
		t.Errorf("the refusal must name ssh_command: %s", rec.Body.String())
	}
	// pg_basebackup + archive_command is a valid combination, with no slot.
	ok := serverForm(csrf, "p-archive-noslot", "postgres", "archive_command", "")
	ok.Set("slot_name", "")
	if rec := f.post("/servers", admin, ok); rec.Code != http.StatusSeeOther {
		t.Errorf("postgres + archive_command without slot: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSSHKeyPage(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	op, _ := f.login(t, "operator")
	if rec := f.get("/ssh-key", op); rec.Code != http.StatusForbidden {
		t.Errorf("operator on /ssh-key: %d", rec.Code)
	}
	if rec := f.get("/ssh-key", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous on /ssh-key: %d", rec.Code)
	}
	rec := f.get("/ssh-key", admin)
	if rec.Code != 200 {
		t.Fatalf("/ssh-key: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	pub, err := sshkeys.PublicKey(f.env.SSHHome)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, strings.ReplaceAll(html.EscapeString(pub), "+", "&#43;")) {
		t.Error("the public key is not on the page")
	}
	host, _ := os.Hostname()
	for _, w := range []string{"~postgres/.ssh/authorized_keys", "~barman/.ssh/authorized_keys", "barman-cli", "archive_mode = on", "archive_command = 'barman-wal-archive " + host + " &lt;server&gt; %p'", "restart", "pg_reload_conf", "barman switch-wal --archive", "barman check"} {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
	priv, err := os.ReadFile(filepath.Join(f.env.SSHHome, ".ssh", "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing of the private key, in any response, with any role.
	secret := strings.Split(strings.TrimSpace(string(priv)), "\n")[1]
	for _, path := range []string{"/ssh-key", "/servers/pg-legacy", "/audit", "/status"} {
		b := f.get(path, admin).Body.String()
		if strings.Contains(b, secret) || strings.Contains(b, "PRIVATE KEY") {
			t.Errorf("%s leaks the private key", path)
		}
	}
	// The pair was created once, with the right modes, and the creation is audited.
	st, _ := os.Stat(filepath.Join(f.env.SSHHome, ".ssh", "id_ed25519"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("private key mode %o", st.Mode().Perm())
	}
	f.get("/ssh-key", admin)
	if b, _ := os.ReadFile(filepath.Join(f.env.SSHHome, ".ssh", "id_ed25519")); string(b) != string(priv) {
		t.Error("the key was replaced by a second visit")
	}
	if !strings.Contains(f.get("/audit", admin).Body.String(), "sshkey.create") {
		t.Error("key creation is not in the audit log")
	}
}

func TestSSHConnectionTest(t *testing.T) {
	f := newFixture(t)
	admin, csrf := f.login(t, "admin")
	op, ocsrf := f.login(t, "operator")

	// pg-legacy has an ssh_command: the button is on its page for an admin only.
	if page := f.get("/servers/pg-legacy", admin).Body.String(); !strings.Contains(page, "/servers/pg-legacy/ssh-test") {
		t.Error("no test button for the admin")
	}
	if page := f.get("/servers/pg-legacy", op).Body.String(); strings.Contains(page, "ssh-test") {
		t.Error("the operator must not see the test button")
	}
	if page := f.get("/servers/pg-main", admin).Body.String(); strings.Contains(page, "ssh-test") {
		t.Error("pg-main has no ssh_command: no test button")
	}
	if rec := f.post("/servers/pg-legacy/ssh-test", op, url.Values{}, "HX-Request", "true", "X-CSRF-Token", ocsrf); rec.Code != http.StatusForbidden {
		t.Errorf("operator: %d", rec.Code)
	}
	if rec := f.post("/servers/pg-main/ssh-test", admin, url.Values{}, "HX-Request", "true", "X-CSRF-Token", csrf); rec.Code != http.StatusConflict {
		t.Errorf("server without ssh_command: %d", rec.Code)
	}

	// pg-legacy: ssh postgres@pg-legacy.example.com, answered by the fake ssh.
	rec := f.post("/servers/pg-legacy/ssh-test", admin, url.Values{}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "OK") || strings.Contains(rec.Body.String(), "failed") {
		t.Fatalf("test: %d %s", rec.Code, rec.Body.String())
	}
	log, _ := os.ReadFile(f.env.SSHLog)
	if !strings.Contains(string(log), "-o BatchMode=yes -o ConnectTimeout=10 postgres@pg-legacy.example.com true") {
		t.Errorf("ssh was called with %q", log)
	}
	if !strings.Contains(f.get("/audit", admin).Body.String(), "ssh -o BatchMode=yes") {
		t.Error("the connection test is not in the audit log")
	}

	// A failing host shows ssh's own message.
	conf := readConf(t, f, "pg-legacy")
	if err := os.WriteFile(filepath.Join(f.env.BarmanDir, "pg-legacy.conf"), []byte(strings.Replace(conf, "ssh postgres@pg-legacy.example.com", "ssh postgres@down", 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	rec = f.post("/servers/pg-legacy/ssh-test", admin, url.Values{}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "failed") || !strings.Contains(rec.Body.String(), "Connection refused") {
		t.Errorf("failing test: %d %s", rec.Code, rec.Body.String())
	}
	if priv, err := os.ReadFile(filepath.Join(f.env.SSHHome, ".ssh", "id_ed25519")); err == nil && strings.Contains(rec.Body.String(), strings.Split(string(priv), "\n")[1]) {
		t.Error("private key leaked")
	}
}

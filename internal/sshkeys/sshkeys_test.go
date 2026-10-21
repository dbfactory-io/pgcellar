package sshkeys

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/dbfactory-io/pgcellar/internal/barman"
)

func TestEnsureCreatesPairOnce(t *testing.T) {
	home := t.TempDir()
	created, err := Ensure(home)
	if err != nil || !created {
		t.Fatalf("first Ensure = %v, %v", created, err)
	}
	priv := filepath.Join(home, ".ssh", "id_ed25519")
	pub := priv + ".pub"
	for path, want := range map[string]os.FileMode{filepath.Join(home, ".ssh"): 0o700, priv: 0o600, pub: 0o644} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", path, st.Mode().Perm(), want)
		}
	}
	before, _ := os.ReadFile(priv)
	created, err = Ensure(home)
	if err != nil || created {
		t.Fatalf("second Ensure = %v, %v", created, err)
	}
	after, _ := os.ReadFile(priv)
	if !bytes.Equal(before, after) {
		t.Error("the private key was replaced")
	}
	if _, err := ssh.ParseRawPrivateKey(after); err != nil {
		t.Errorf("private key does not parse: %v", err)
	}
}

func TestEnsureRebuildsMissingPublicFile(t *testing.T) {
	home := t.TempDir()
	if _, err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	first, _ := PublicKey(home)
	if err := os.Remove(filepath.Join(home, ".ssh", "id_ed25519.pub")); err != nil {
		t.Fatal(err)
	}
	if created, err := Ensure(home); err != nil || created {
		t.Fatalf("Ensure = %v, %v", created, err)
	}
	second, err := PublicKey(home)
	if err != nil || first != second {
		t.Errorf("public key changed: %q vs %q (%v)", first, second, err)
	}
}

func TestPublicKeyFormat(t *testing.T) {
	home := t.TempDir()
	if _, err := PublicKey(home); err == nil {
		t.Error("PublicKey without a key must fail")
	}
	if _, err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	line, err := PublicKey(home)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if !strings.HasPrefix(line, "ssh-ed25519 AAAA") || !strings.HasSuffix(line, " pgcellar@"+host) || strings.Contains(line, "\n") {
		t.Errorf("unexpected line %q", line)
	}
	if _, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil || comment != "pgcellar@"+host {
		t.Errorf("not a valid authorized_keys line: %v, comment %q", err, comment)
	}
	priv, _ := os.ReadFile(filepath.Join(home, ".ssh", "id_ed25519"))
	if strings.Contains(line, "PRIVATE") || bytes.Contains(priv, []byte(strings.Fields(line)[1])) {
		t.Error("the public line leaks private material")
	}
}

type recorder struct {
	binary string
	args   []string
	err    error
	stderr string
}

func (r *recorder) RunBinary(_ context.Context, binary string, args ...string) ([]byte, []byte, error) {
	r.binary, r.args = binary, args
	return nil, []byte(r.stderr), r.err
}

var _ barman.BinaryRunner = (*recorder)(nil)

func TestTestConnectionCommandLine(t *testing.T) {
	r := &recorder{}
	res := TestConnection(context.Background(), r, "ssh -p 2222 postgres@db1")
	if !res.OK || res.Message != "OK" {
		t.Fatalf("result = %+v", res)
	}
	got := r.binary + " " + strings.Join(r.args, " ")
	want := "ssh -o BatchMode=yes -o ConnectTimeout=10 -p 2222 postgres@db1 true"
	if got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestTestConnectionErrorsAndRefusals(t *testing.T) {
	r := &recorder{err: os.ErrPermission, stderr: "Permission denied (publickey).\n"}
	res := TestConnection(context.Background(), r, "ssh postgres@db1")
	if res.OK || res.Message != "Permission denied (publickey)." {
		t.Errorf("result = %+v", res)
	}
	for _, bad := range []string{"", "ssh", "rm -rf /", "bash -c x"} {
		r2 := &recorder{}
		if res := TestConnection(context.Background(), r2, bad); res.OK || r2.binary != "" {
			t.Errorf("%q must be refused without running anything: %+v", bad, res)
		}
	}
}

func TestTestConnectionWithFakeSSH(t *testing.T) {
	abs, _ := filepath.Abs(filepath.Join("..", "barman", "testdata"))
	t.Setenv("PATH", abs+string(os.PathListSeparator)+os.Getenv("PATH"))
	var entries []barman.AuditEntry
	r := barman.CLIRunner{Audit: func(e barman.AuditEntry) { entries = append(entries, e) }}
	if res := TestConnection(context.Background(), r, "fake-ssh postgres@db1"); res.OK {
		t.Error("a command that is not ssh must be refused")
	}
	// fake-ssh is named like ssh through a symlink so the base-name rule holds.
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(abs, "fake-ssh"), filepath.Join(dir, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if res := TestConnection(context.Background(), r, "ssh postgres@db1"); !res.OK {
		t.Errorf("good host: %+v", res)
	}
	if res := TestConnection(context.Background(), r, "ssh postgres@down"); res.OK || !strings.Contains(res.Message, "Connection refused") {
		t.Errorf("down host: %+v", res)
	}
	if len(entries) != 2 || entries[0].Binary != "ssh" {
		t.Errorf("audit entries = %+v", entries)
	}
}

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func TestHistory(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	if err := EnsureRepo(dir); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRepo(dir); err != nil { // idempotent
		t.Fatal(err)
	}
	if entries, err := Log(dir, 10); err != nil || len(entries) != 0 {
		t.Fatalf("empty repo log = %v, %v", entries, err)
	}

	cfgPath := filepath.Join(dir, "pgcellar.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: ':8080'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h1, err := Commit(dir, "feat: initial config", "gilles", "gilles@example.com")
	if err != nil || h1 == "" {
		t.Fatalf("first commit: %q, %v", h1, err)
	}
	if h, err := Commit(dir, "nothing", "gilles", "gilles@example.com"); err != nil || h != "" {
		t.Fatalf("no-change commit = %q, %v", h, err)
	}
	if err := os.WriteFile(cfgPath, []byte("listen: ':9090'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h2, err := Commit(dir, "change port", "alice", "alice@example.com")
	if err != nil || h2 == "" || h2 == h1 {
		t.Fatalf("second commit: %q, %v", h2, err)
	}

	entries, err := Log(dir, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("log = %v, %v", entries, err)
	}
	if entries[0].Hash != h2 || entries[0].Author != "alice" || entries[0].Message != "change port" || entries[0].Date == "" {
		t.Errorf("newest entry wrong: %+v", entries[0])
	}
	if limited, _ := Log(dir, 1); len(limited) != 1 {
		t.Errorf("limit ignored: %d", len(limited))
	}
	d, err := Diff(dir, h2)
	if err != nil || !strings.Contains(d, "-listen: ':8080'") || !strings.Contains(d, "+listen: ':9090'") {
		t.Errorf("diff = %q, %v", d, err)
	}
	if _, err := Diff(dir, "--output=/tmp/x"); err == nil {
		t.Error("invalid hash must be rejected")
	}
}

func TestSecretsNeverCommitted(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	if err := EnsureRepo(dir); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"server.key", "id_ed25519", "id_rsa.pub", "cert.pem", "pgcellar.db"}
	for _, f := range secrets {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pgcellar.yaml"), []byte("listen: ':8080'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(dir, "init", "a", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	out, err := git(dir, nil, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != ".gitignore\npgcellar.yaml" {
		t.Errorf("tracked files = %q", out)
	}
	for _, f := range secrets {
		if _, err := git(dir, nil, "check-ignore", "-q", f); err != nil {
			t.Errorf("%s is not ignored", f)
		}
	}
}

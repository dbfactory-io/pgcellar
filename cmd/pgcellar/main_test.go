package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "pgcellar ") {
		t.Errorf("version: %d %q", code, out.String())
	}
	if code := run(nil, &out, &errb); code != 2 {
		t.Errorf("no args: %d", code)
	}
}

func TestCheckExampleConfig(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"check", "--config", "../../deploy/pgcellar.example.yaml", "--barman-dir", "../../deploy/barman.d.example"}, &out, &errb)
	if code != 0 {
		t.Fatalf("check: %d\n%s%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"pg-main", "pg-legacy", "postgres", "rsync", "all good"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestCheckFindsProblems(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.conf"), []byte("[broken]\nbackup_method = postgres\nconninfo = host=x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	yml := filepath.Join(dir, "pgcellar.yaml")
	if err := os.WriteFile(yml, []byte("listen: \":8080\"\ndata_dir: /tmp/x\nbarman:\n  binary: barman\n  config_dir: "+dir+"\nservers:\n  - name: ghost\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"check", "--config", yml}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit code %d, want 1\n%s", code, out.String())
	}
	for _, want := range []string{"at least one user with the admin role", "needs streaming_conninfo", "ghost", "problem(s) found"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if code := run([]string{"check", "--config", filepath.Join(dir, "missing.yaml")}, &out, &errb); code != 1 {
		t.Errorf("missing file: %d", code)
	}
}

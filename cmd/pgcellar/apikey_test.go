package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

func TestAPIKeyCreateAndList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pgcellar.yaml")
	cfg := config.Defaults()
	cfg.Users = []config.User{{Name: "admin", Role: "admin", PasswordHash: "x"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"apikey", "create", "--config", path, "--name", "cron", "--role", "operator"}, &out, &errb); code != 0 {
		t.Fatalf("create: %d %s", code, errb.String())
	}
	key := strings.TrimSpace(out.String())
	if !strings.HasPrefix(key, "pgc_") {
		t.Fatalf("unexpected key output %q", key)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := config.MatchAPIKey(loaded, key); !ok || k.Name != "cron" || k.Role != "operator" {
		t.Fatalf("stored key does not match: %+v", loaded.APIKeys)
	}
	if strings.Contains(strings.Join([]string{loaded.APIKeys[0].KeyHash}, ""), key) {
		t.Fatal("clear key stored")
	}
	if code := run([]string{"apikey", "create", "--config", path, "--name", "cron", "--role", "viewer"}, &out, &errb); code == 0 {
		t.Fatal("duplicate name accepted")
	}
	if code := run([]string{"apikey", "create", "--config", path, "--name", "x", "--role", "root"}, &out, &errb); code == 0 {
		t.Fatal("bad role accepted")
	}
	out.Reset()
	if code := run([]string{"apikey", "list", "--config", path}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != "cron\toperator" {
		t.Fatalf("list: %d %q", code, out.String())
	}
}

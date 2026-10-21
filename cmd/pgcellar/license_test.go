package main

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/license"
)

func TestLicenseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run([]string{"license", "keygen", "--out", dir}, &out, &errb); code != 0 {
		t.Fatalf("keygen: %d %s", code, errb.String())
	}
	privPath := filepath.Join(dir, "license_signing.key")
	fi, err := os.Stat(privPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the private key must be 0600: %v %v", fi, err)
	}
	pubText, err := os.ReadFile(privPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), strings.TrimSpace(string(pubText))) {
		t.Error("keygen must print the public key")
	}
	if code := run([]string{"license", "keygen", "--out", dir}, &out, &errb); code == 0 {
		t.Error("keygen must not overwrite a signing key")
	}
	pub, err := license.DecodePublic(string(pubText))
	if err != nil {
		t.Fatal(err)
	}
	old := verifyKey
	verifyKey = func() ed25519.PublicKey { return pub }
	defer func() { verifyKey = old }()

	out.Reset()
	args := []string{"license", "issue", "--key", privPath, "--edition", "pro", "--licensee", "ACME SA", "--servers", "10", "--expires", "2099-12-31", "--id", "lic-test"}
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("issue: %d %s", code, errb.String())
	}
	key := strings.TrimSpace(out.String())
	out.Reset()
	if code := run([]string{"license", "verify", key}, &out, &errb); code != 0 {
		t.Fatalf("verify: %d %s", code, out.String())
	}
	for _, want := range []string{`"licensee": "ACME SA"`, `"servers": 10`, "lic-test", "licence valid"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("verify output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := run([]string{"license", "verify", key + "x"}, &out, &errb); code == 0 || !strings.Contains(out.String(), "INVALID") {
		t.Errorf("an altered key must be refused: %d %s", code, out.String())
	}

	// show --config
	cfg := filepath.Join(dir, "pgcellar.yaml")
	if err := os.WriteFile(cfg, []byte("license:\n  key: \""+key+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"license", "show", "--config", cfg}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Pro edition · ACME SA · 0 of 10 servers") {
		t.Errorf("show: %d\n%s", code, out.String())
	}
	// missing arguments
	if code := run([]string{"license", "issue", "--key", privPath}, &out, &errb); code != 2 {
		t.Errorf("issue without options: %d", code)
	}
}

func TestHashPassword(t *testing.T) {
	var out, errb bytes.Buffer
	if code := hashPassword(strings.NewReader("correct horse battery\n"), &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "$2") {
		t.Fatalf("hash: %d %q %q", code, out.String(), errb.String())
	}
	if code := hashPassword(strings.NewReader("short\n"), &out, &errb); code == 0 {
		t.Error("a short password is refused")
	}
}

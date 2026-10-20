package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testKey(t *testing.T, comment string) string {
	t.Helper()
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment
}

func TestAllowWritesRestrictedLine(t *testing.T) {
	home := t.TempDir()
	pub := testKey(t, "postgres@db1")
	fp, err := Allow(home, "pg-main", pub)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(AuthorizedPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys mode %o", st.Mode().Perm())
	}
	if d, _ := os.Stat(Dir(home)); d.Mode().Perm() != 0o700 {
		t.Errorf(".ssh mode %o", d.Mode().Perm())
	}
	b, _ := os.ReadFile(AuthorizedPath(home))
	base := strings.Join(strings.Fields(pub)[:2], " ")
	want := `restrict,command="barman put-wal pg-main" ` + base + " pgcellar:pg-main\n"
	if string(b) != want {
		t.Errorf("line =\n%q\nwant\n%q", b, want)
	}
	keys, err := ListAuthorized(home)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list = %v, %v", keys, err)
	}
	k := keys[0]
	if k.Server != "pg-main" || k.Fingerprint != fp || k.Type != "ssh-ed25519" || !k.Managed {
		t.Errorf("key = %+v", k)
	}
	// The pasted comment is dropped: nothing of the person's text lands in the options.
	if strings.Contains(string(b), "postgres@db1") {
		t.Error("the pasted comment was kept")
	}
}

func TestAllowRefusesDuplicatesAndKeepsOtherLines(t *testing.T) {
	home := t.TempDir()
	other := testKey(t, "admin@laptop")
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := "# keys of the team\n" + other + "\n"
	if err := os.WriteFile(AuthorizedPath(home), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	a := testKey(t, "")
	b := testKey(t, "")
	if _, err := Allow(home, "pg-a", a); err != nil {
		t.Fatal(err)
	}
	if _, err := Allow(home, "pg-b", b); err != nil {
		t.Fatal(err)
	}
	// Same key again, for the same or another server: refused.
	for _, srv := range []string{"pg-a", "pg-b"} {
		if _, err := Allow(home, srv, a); err != ErrDuplicate {
			t.Errorf("duplicate for %s: %v", srv, err)
		}
	}
	// A key that is already in the file but was not written by pgCellar counts too.
	if _, err := Allow(home, "pg-a", other); err != ErrDuplicate {
		t.Errorf("duplicate of a foreign line: %v", err)
	}
	keys, _ := ListAuthorized(home)
	if len(keys) != 3 {
		t.Fatalf("keys = %+v", keys)
	}
	if keys[0].Managed || keys[0].Server != "" {
		t.Errorf("the foreign line must not look managed: %+v", keys[0])
	}

	// Revoke one: the comment and the other lines are untouched.
	if _, err := Revoke(home, keys[1].Fingerprint); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(AuthorizedPath(home))
	if !strings.HasPrefix(string(raw), seed) {
		t.Errorf("seed lines changed:\n%s", raw)
	}
	left, _ := ListAuthorized(home)
	if len(left) != 2 || left[1].Server != "pg-b" {
		t.Errorf("left = %+v", left)
	}
	if _, err := Revoke(home, keys[1].Fingerprint); err != ErrNoSuchKey {
		t.Errorf("second revoke: %v", err)
	}
	if st, _ := os.Stat(AuthorizedPath(home)); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after rewrite %o", st.Mode().Perm())
	}
	// The directory holds no temporary file.
	ents, _ := os.ReadDir(Dir(home))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestListMissingFile(t *testing.T) {
	keys, err := ListAuthorized(t.TempDir())
	if err != nil || len(keys) != 0 {
		t.Errorf("missing file: %v, %v", keys, err)
	}
}

func TestAllowRefusesBadServerName(t *testing.T) {
	home := t.TempDir()
	for _, srv := range []string{"", "a b", `x" ; rm -rf / #`, "a\nb", "barman"} {
		if _, err := Allow(home, srv, testKey(t, "")); err == nil {
			t.Errorf("server %q accepted", srv)
		}
	}
	if _, err := os.Stat(AuthorizedPath(home)); err == nil {
		t.Error("a file was written for a refused request")
	}
}

func TestParsePublicKey(t *testing.T) {
	good := testKey(t, "postgres@db1")
	if line, fp, err := ParsePublicKey("  " + good + "\n"); err != nil || strings.Contains(line, "postgres@db1") || !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("good key: %q %q %v", line, fp, err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := ssh.NewPublicKey(&rsaKey.PublicKey)
	if _, _, err := ParsePublicKey(string(ssh.MarshalAuthorizedKey(rk))); err != nil {
		t.Errorf("rsa key refused: %v", err)
	}
	base := strings.Join(strings.Fields(good)[:2], " ")
	bad := map[string]string{
		"empty":         "",
		"two lines":     good + "\n" + good,
		"carriage":      good + "\rcommand=\"evil\"",
		"garbage":       "ssh-ed25519 not-base64!!",
		"words":         "hello world",
		"private key":   "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
		"with options":  `command="rm -rf /" ` + base,
		"with restrict": "restrict " + base,
		"type only":     "ssh-ed25519",
	}
	for name, in := range bad {
		if _, _, err := ParsePublicKey(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAllowedNeverContainsPrivateKey(t *testing.T) {
	home := t.TempDir()
	if _, err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if _, err := Allow(home, "pg-main", testKey(t, "")); err != nil {
		t.Fatal(err)
	}
	priv, _ := os.ReadFile(Dir(home) + "/id_ed25519")
	secret := strings.Split(strings.TrimSpace(string(priv)), "\n")[1]
	auth, _ := os.ReadFile(AuthorizedPath(home))
	if strings.Contains(string(auth), secret) || strings.Contains(string(auth), "PRIVATE KEY") {
		t.Error("authorized_keys holds private key material")
	}
}

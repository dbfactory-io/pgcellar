package web

import (
	"crypto/ed25519"
	"crypto/rand"
	"html"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/dbfactory-io/pgcellar/internal/sshkeys"
)

func newPub(t *testing.T) string {
	t.Helper()
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ssh.NewPublicKey(pk)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " postgres@db1"
}

func TestAllowedKeysSection(t *testing.T) {
	f := newFixture(t)
	admin, csrf := f.login(t, "admin")
	op, ocsrf := f.login(t, "operator")
	pub := newPub(t)
	form := func(server, key string) url.Values {
		return url.Values{"csrf": {csrf}, "server": {server}, "public_key": {key}}
	}

	// Admin only.
	if rec := f.post("/ssh-key/authorized-keys", op, url.Values{"csrf": {ocsrf}, "server": {"pg-main"}, "public_key": {pub}}); rec.Code != http.StatusForbidden {
		t.Errorf("operator: %d", rec.Code)
	}
	if rec := f.post("/ssh-key/authorized-keys", nil, form("pg-main", pub)); rec.Code == http.StatusSeeOther && !strings.Contains(rec.Header().Get("Location"), "login") {
		t.Errorf("anonymous: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	body := f.get("/ssh-key", admin).Body.String()
	if !strings.Contains(body, "PostgreSQL servers allowed to push WAL") || !strings.Contains(body, "No PostgreSQL server is allowed") {
		t.Error("section or empty text missing")
	}
	if !strings.Contains(body, `<option value="pg-main"`) || !strings.Contains(body, `name="public_key"`) {
		t.Error("form missing")
	}

	// Allow.
	rec := f.post("/ssh-key/authorized-keys", admin, form("pg-main", pub))
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=allowed") {
		t.Fatalf("allow: %d %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(sshkeys.AuthorizedPath(f.env.SSHHome))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `restrict,command="barman put-wal pg-main" ssh-ed25519 `) || !strings.HasSuffix(strings.TrimSpace(string(raw)), " pgcellar:pg-main") {
		t.Errorf("file = %q", raw)
	}
	keys, _ := sshkeys.ListAuthorized(f.env.SSHHome)
	fp := keys[0].Fingerprint
	page := f.get("/ssh-key", admin)
	if !strings.Contains(page.Body.String(), strings.ReplaceAll(html.EscapeString(fp), "+", "&#43;")) || strings.Contains(page.Body.String(), "No PostgreSQL server is allowed") {
		t.Error("the key is not listed")
	}
	// The page follows the file: a line added by hand shows up, no cache.
	extra := newPub(t)
	f2, _ := os.OpenFile(sshkeys.AuthorizedPath(f.env.SSHHome), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f2.WriteString(extra + "\n")
	_ = f2.Close()
	if !strings.Contains(f.get("/ssh-key", admin).Body.String(), "not restricted to put-wal") {
		t.Error("a line added by hand is not shown as unrestricted")
	}

	// Duplicate, bad key, unknown server: nothing written.
	before, _ := os.ReadFile(sshkeys.AuthorizedPath(f.env.SSHHome))
	if rec := f.post("/ssh-key/authorized-keys", admin, form("pg-legacy", pub)); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already authorised") {
		t.Errorf("duplicate: %d", rec.Code)
	}
	if rec := f.post("/ssh-key/authorized-keys", admin, form("pg-main", "ssh-ed25519 nonsense")); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not a valid public key") {
		t.Errorf("bad key: %d", rec.Code)
	}
	if rec := f.post("/ssh-key/authorized-keys", admin, form("pg-main", newPub(t)+"\n"+newPub(t))); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("two lines: %d", rec.Code)
	}
	if rec := f.post("/ssh-key/authorized-keys", admin, form("nope", newPub(t))); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown server: %d", rec.Code)
	}
	if after, _ := os.ReadFile(sshkeys.AuthorizedPath(f.env.SSHHome)); string(after) != string(before) {
		t.Error("a refused request changed the file")
	}

	// Remove asks for confirmation on the page first.
	confirm := f.get("/ssh-key?confirm="+url.QueryEscape(fp), admin).Body.String()
	if !strings.Contains(confirm, "Yes, remove") {
		t.Error("no confirmation step")
	}
	if strings.Contains(f.get("/ssh-key", admin).Body.String(), "Yes, remove") {
		t.Error("confirmation shown without being asked")
	}
	if rec := f.post("/ssh-key/authorized-keys/revoke", op, url.Values{"csrf": {ocsrf}, "fingerprint": {fp}}); rec.Code != http.StatusForbidden {
		t.Errorf("operator revoke: %d", rec.Code)
	}
	if rec := f.post("/ssh-key/authorized-keys/revoke", admin, url.Values{"csrf": {csrf}, "fingerprint": {fp}}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=revoked") {
		t.Errorf("revoke: %d", rec.Code)
	}
	if left, _ := sshkeys.ListAuthorized(f.env.SSHHome); len(left) != 1 || left[0].Fingerprint == fp {
		t.Errorf("left = %+v", left)
	}

	// Audit trail, and the private key stays out of every answer.
	audit := f.get("/audit", admin).Body.String()
	for _, w := range []string{"sshkey.allow", "sshkey.revoke"} {
		if !strings.Contains(audit, w) {
			t.Errorf("audit lacks %s", w)
		}
	}
	priv, _ := os.ReadFile(f.env.SSHHome + "/.ssh/id_ed25519")
	secret := strings.Split(strings.TrimSpace(string(priv)), "\n")[1]
	for _, p := range []string{"/ssh-key", "/ssh-key?confirm=" + url.QueryEscape(fp), "/audit"} {
		if b := f.get(p, admin).Body.String(); strings.Contains(b, secret) || strings.Contains(b, "PRIVATE KEY") {
			t.Errorf("%s leaks the private key", p)
		}
	}
}

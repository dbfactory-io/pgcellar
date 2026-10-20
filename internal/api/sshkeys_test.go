package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/stranix79/pgcellar/internal/sshkeys"
)

func TestAuthorizedKeysAPI(t *testing.T) {
	f := newFixture(t)
	admin := f.env.Keys["admin"]
	pk, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pk)
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " postgres@db1"
	body, _ := json.Marshal(map[string]string{"server": "pg-main", "public_key": pub})

	for _, role := range []string{"viewer", "operator"} {
		if code, _ := f.call(t, "GET", "/ssh/authorized-keys", f.env.Keys[role], ""); code != 403 {
			t.Errorf("%s GET: %d", role, code)
		}
		if code, _ := f.call(t, "POST", "/ssh/authorized-keys", f.env.Keys[role], string(body)); code != 403 {
			t.Errorf("%s POST: %d", role, code)
		}
	}
	if code, _ := f.call(t, "GET", "/ssh/authorized-keys", "", ""); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
	if code, out := f.call(t, "GET", "/ssh/authorized-keys", admin, ""); code != 200 || len(out["data"].([]any)) != 0 {
		t.Errorf("empty list: %d %v", code, out)
	}

	code, out := f.call(t, "POST", "/ssh/authorized-keys", admin, string(body))
	if code != 201 {
		t.Fatalf("POST: %d %v", code, out)
	}
	created := out["data"].(map[string]any)
	fp, _ := created["fingerprint"].(string)
	if created["server"] != "pg-main" || created["managed"] != true || !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("created = %v", created)
	}
	raw, _ := os.ReadFile(sshkeys.AuthorizedPath(f.env.SSHHome))
	if !strings.HasPrefix(string(raw), `restrict,command="barman put-wal pg-main" `) {
		t.Errorf("file = %q", raw)
	}
	if code, out := f.call(t, "GET", "/ssh/authorized-keys", admin, ""); code != 200 || len(out["data"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, out)
	}

	if code, _ := f.call(t, "POST", "/ssh/authorized-keys", admin, string(body)); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
	bad, _ := json.Marshal(map[string]string{"server": "pg-main", "public_key": "ssh-ed25519 nope"})
	if code, _ := f.call(t, "POST", "/ssh/authorized-keys", admin, string(bad)); code != 422 {
		t.Errorf("bad key: %d", code)
	}
	unk, _ := json.Marshal(map[string]string{"server": "nope", "public_key": pub})
	if code, _ := f.call(t, "POST", "/ssh/authorized-keys", admin, string(unk)); code != 404 {
		t.Errorf("unknown server: %d", code)
	}
	if code, _ := f.call(t, "POST", "/ssh/authorized-keys", admin, `{"server":"pg-main"`); code != 400 {
		t.Errorf("broken JSON: %d", code)
	}

	del := func(key, fingerprint string) int {
		req := httptest.NewRequest("DELETE", "/api/v1/ssh/authorized-keys?fingerprint="+url.QueryEscape(fingerprint), nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := del(f.env.Keys["operator"], fp); c != http.StatusForbidden {
		t.Errorf("operator DELETE: %d", c)
	}
	if c := del(admin, ""); c != 400 {
		t.Errorf("no fingerprint: %d", c)
	}
	if c := del(admin, fp); c != 204 {
		t.Errorf("DELETE: %d", c)
	}
	if c := del(admin, fp); c != 404 {
		t.Errorf("second DELETE: %d", c)
	}
	if left, _ := sshkeys.ListAuthorized(f.env.SSHHome); len(left) != 0 {
		t.Errorf("left = %+v", left)
	}
	_, audit := f.call(t, "GET", "/audit", admin, "")
	a, _ := json.Marshal(audit)
	for _, w := range []string{"sshkey.allow", "sshkey.revoke"} {
		if !strings.Contains(string(a), w) {
			t.Errorf("audit lacks %s", w)
		}
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/vault/s3test"
)

func TestVaultAPI(t *testing.T) {
	s3 := s3test.New()
	defer s3.Close()
	env := apptest.NewWith(t, func(c *config.Config) {
		c.Vault = config.Vault{Endpoint: s3.URL, Region: s3.Region, Bucket: s3.Bucket, AccessKey: s3.AccessKey}
	})
	f := &fixture{env: env, h: New(env.App).Handler()}
	admin, op, viewer := env.Keys["admin"], env.Keys["operator"], env.Keys["viewer"]

	if code, out := f.call(t, "GET", "/vault", viewer, ""); code != 200 || out["data"].(map[string]any)["enabled"] != false {
		t.Fatalf("vault off: %d %v", code, out)
	}
	if code, _ := f.call(t, "POST", "/vault/copy", op, `{"server":"pg-main"}`); code != http.StatusConflict {
		t.Errorf("copy while off: %d", code)
	}
	if code, _ := f.call(t, "PUT", "/vault", op, `{"enabled":true}`); code != http.StatusForbidden {
		t.Errorf("operator PUT: %d", code)
	}
	if code, out := f.call(t, "PUT", "/vault", admin, `{"enabled":true}`); code != 422 || !strings.Contains(out["error"].(string), "secret key is missing") {
		t.Errorf("enable without secret: %d %v", code, out)
	}
	code, out := f.call(t, "PUT", "/vault", admin, `{"enabled":true,"secret_key":"`+s3.SecretKey+`","quota":"1TB"}`)
	if code != 200 {
		t.Fatalf("PUT: %d %v", code, out)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), s3.SecretKey) {
		t.Fatal("the secret key came back in the answer")
	}

	s3.SetLock(7)
	code, out = f.call(t, "POST", "/vault/test", admin, "")
	res := out["data"].(map[string]any)
	if code != 200 || res["ok"] != true || res["lock"].(map[string]any)["days"].(float64) != 7 {
		t.Fatalf("test: %d %v", code, out)
	}
	if code, out := f.call(t, "POST", "/vault/test", admin, `{"bucket":"missing-bucket"}`); code != http.StatusBadGateway || out["data"].(map[string]any)["ok"] != false {
		t.Errorf("test of another bucket: %d %v", code, out)
	}

	code, out = f.call(t, "POST", "/vault/copy", op, `{"server":"pg-main"}`)
	if code != http.StatusAccepted || out["data"].(map[string]any)["position"].(float64) != 1 {
		t.Fatalf("copy: %d %v", code, out)
	}
	if code, _ := f.call(t, "POST", "/vault/copy", op, `{"server":"nope"}`); code != 404 {
		t.Errorf("copy of an unknown server: %d", code)
	}
	env.App.VaultTick(context.Background(), env.App.Now())
	code, out = f.call(t, "GET", "/vault/copies?server=pg-main", viewer, "")
	list := out["data"].([]any)
	if code != 200 || len(list) != 1 || list[0].(map[string]any)["ok"] != true || list[0].(map[string]any)["journal"] != nil {
		t.Fatalf("copies: %d %v", code, out)
	}
	id := int64(list[0].(map[string]any)["id"].(float64))
	_, out = f.call(t, "GET", "/vault/copies/"+strconv.FormatInt(id, 10), viewer, "")
	if j, _ := out["data"].(map[string]any)["journal"].(string); !strings.Contains(j, "backup --json") {
		t.Errorf("one copy has its journal: %v", out)
	}
	_, out = f.call(t, "GET", "/vault", viewer, "")
	v := out["data"].(map[string]any)
	srv := v["servers"].([]any)[0].(map[string]any)
	if v["lock_text"] != "Locked for 7 days (governance)" || v["quota_bytes"].(float64) != 1e12 || srv["status"].(map[string]any)["copied"] != true {
		t.Errorf("vault: %v", v)
	}
	_, out = f.call(t, "GET", "/status", viewer, "")
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"vault":{"copied":true`) {
		t.Errorf("status lacks the vault of pg-main: %s", b)
	}

	// The recovery key is text, for admins only, and audited.
	req := httptest.NewRequest("GET", "/api/v1/vault/recovery-key", nil)
	req.Header.Set("Authorization", "Bearer "+op)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("operator key: %d", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer "+admin)
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	cfg := env.App.Config()
	pw, _ := os.ReadFile(cfg.Vault.EffectivePasswordFile(cfg.DataDir))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), strings.TrimSpace(string(pw))) {
		t.Errorf("recovery key: %d %s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(filepath.Join(env.Dir, "pgcellar.yaml"))
	if strings.Contains(string(raw), s3.SecretKey) {
		t.Error("the secret key reached pgcellar.yaml")
	}
}

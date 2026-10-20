package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stranix79/pgcellar/internal/app/apptest"
)

type fixture struct {
	env *apptest.Env
	h   http.Handler
}

func newFixture(t *testing.T) *fixture {
	env := apptest.New(t)
	return &fixture{env: env, h: New(env.App).Handler()}
}

func (f *fixture) call(t *testing.T, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s: content type %q, body %q", method, path, ct, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: not JSON: %v: %s", method, path, err, rec.Body.String())
	}
	if _, hasData := out["data"]; !hasData && out["error"] == nil {
		t.Fatalf("%s %s: neither data nor error: %s", method, path, rec.Body.String())
	}
	return rec.Code, out
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	if code, out := f.call(t, "GET", "/servers", "", ""); code != 401 || out["error"] == nil {
		t.Errorf("no key: %d %v", code, out)
	}
	if code, _ := f.call(t, "GET", "/servers", "pgc_wrong", ""); code != 401 {
		t.Errorf("wrong key: %d", code)
	}
	if code, _ := f.call(t, "GET", "/servers", f.env.Keys["viewer"], ""); code != 200 {
		t.Errorf("viewer GET: %d", code)
	}
	if code, out := f.call(t, "PUT", "/servers/pg-main", f.env.Keys["viewer"], `{}`); code != 403 {
		t.Errorf("viewer PUT: %d %v", code, out)
	}
	if code, _ := f.call(t, "PUT", "/servers/pg-main", f.env.Keys["operator"], `{}`); code != 403 {
		t.Errorf("operator PUT: %d", code)
	}
	if code, _ := f.call(t, "POST", "/servers/pg-main/backup", f.env.Keys["viewer"], ""); code != 403 {
		t.Errorf("viewer backup: %d", code)
	}
	if code, _ := f.call(t, "GET", "/audit", f.env.Keys["operator"], ""); code != 403 {
		t.Errorf("operator audit: %d", code)
	}
	if code, _ := f.call(t, "GET", "/nothing", f.env.Keys["admin"], ""); code != 404 {
		t.Errorf("unknown endpoint: %d", code)
	}
}

func TestServersAndStatus(t *testing.T) {
	f := newFixture(t)
	k := f.env.Keys["viewer"]

	code, out := f.call(t, "GET", "/servers", k, "")
	list, _ := out["data"].([]any)
	if code != 200 || len(list) != 4 {
		t.Fatalf("servers: %d %v", code, out)
	}

	code, out = f.call(t, "GET", "/servers/pg-main", k, "")
	d := out["data"].(map[string]any)
	if code != 200 || d["backup_method"] != "postgres" || d["name"] != "pg-main" || d["in_barman"] != true {
		t.Fatalf("server: %d %v", code, out)
	}
	if ret := d["retention"].(map[string]any); ret["recovery_window_days"] != float64(14) || ret["minimum_redundancy"] != float64(2) {
		t.Errorf("retention: %v", ret)
	}
	if code, _ := f.call(t, "GET", "/servers/nope", k, ""); code != 404 {
		t.Errorf("unknown server: %d", code)
	}

	code, out = f.call(t, "GET", "/servers/pg-main/backups", k, "")
	bl := out["data"].([]any)
	if code != 200 || len(bl) != 3 || bl[0].(map[string]any)["id"] != "20261020T020001" {
		t.Errorf("backups: %d %v", code, out)
	}

	code, out = f.call(t, "GET", "/status", k, "")
	servers := out["data"].(map[string]any)["servers"].([]any)
	levels := map[string]string{}
	for _, s := range servers {
		m := s.(map[string]any)
		levels[m["name"].(string)] = m["level"].(string)
	}
	if code != 200 || levels["pg-main"] != "ok" || levels["pg-rsync"] != "warn" || levels["broken"] != "fail" {
		t.Errorf("status: %d %v", code, levels)
	}
}

func TestPutServerWritesAndCommits(t *testing.T) {
	f := newFixture(t)
	k := f.env.Keys["admin"]

	code, out := f.call(t, "PUT", "/servers/pg-main", k, `{"retention":{"recovery_window_days":21},"label":"Via API"}`)
	if code != 200 {
		t.Fatalf("PUT: %d %v", code, out)
	}
	conf, _ := os.ReadFile(filepath.Join(f.env.BarmanDir, "pg-main.conf"))
	if !strings.Contains(string(conf), "RECOVERY WINDOW OF 21 DAYS") || !strings.Contains(string(conf), "minimum_redundancy = 2") || !strings.Contains(string(conf), "backup_compression = gzip") {
		t.Errorf("file after PUT:\n%s", conf)
	}
	if !strings.Contains(string(conf), "; Keep 14 days of recovery") {
		t.Errorf("comments must survive:\n%s", conf)
	}
	res := out["data"].(map[string]any)
	if res["commit"] == nil || res["commit"] == "" {
		t.Errorf("no commit in %v", res)
	}
	code, out = f.call(t, "GET", "/history", f.env.Keys["viewer"], "")
	h := out["data"].([]any)
	if code != 200 || len(h) == 0 || h[0].(map[string]any)["author"] != "api:admin-key" {
		t.Errorf("history: %d %v", code, out)
	}

	if code, out = f.call(t, "PUT", "/servers/pg-main", k, `{"retention":{"recovery_window_days":5,"redundancy":3}}`); code != 422 || out["error"] == nil {
		t.Errorf("both retention kinds: %d %v", code, out)
	}
	if code, _ = f.call(t, "PUT", "/servers/pg-main", k, `{"colour":"red"}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	if code, _ = f.call(t, "PUT", "/servers/pg-main", k, `{"name":"other"}`); code != 400 {
		t.Errorf("rename: %d", code)
	}
	if code, _ = f.call(t, "PUT", "/servers/pg-rsync", k, `{}`); code != 404 && code != 409 {
		t.Errorf("server without file: %d", code)
	}

	code, out = f.call(t, "POST", "/servers", k, `{"name":"pg-api","backup_method":"rsync","conninfo":"host=x user=barman dbname=postgres","ssh_command":"ssh postgres@x","schedule":{"backup":"hourly","enabled":true}}`)
	if code != 201 {
		t.Fatalf("POST /servers: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.env.BarmanDir, "pg-api.conf")); err != nil {
		t.Error(err)
	}
}

func TestCheckBackupAndRuns(t *testing.T) {
	f := newFixture(t)
	op := f.env.Keys["operator"]

	code, out := f.call(t, "POST", "/servers/broken/check", op, "")
	d := out["data"].(map[string]any)
	if code != 200 || d["ok"] != false || len(d["checks"].([]any)) < 5 {
		t.Errorf("check: %d %v", code, out)
	}

	code, out = f.call(t, "POST", "/servers/pg-main/backup", op, "")
	if code != 202 {
		t.Fatalf("backup: %d %v", code, out)
	}
	id := int64(out["data"].(map[string]any)["run_id"].(float64))
	apptest.WaitRun(t, f.env.App, id)
	code, out = f.call(t, "GET", "/runs/"+itoa(id), f.env.Keys["viewer"], "")
	run := out["data"].(map[string]any)
	if code != 200 || run["status"] != "ok" || run["server"] != "pg-main" || run["actor"] != "api:operator-key" {
		t.Errorf("run: %d %v", code, run)
	}
	code, out = f.call(t, "GET", "/runs?server=pg-main", f.env.Keys["viewer"], "")
	if code != 200 || len(out["data"].([]any)) != 1 {
		t.Errorf("runs: %d %v", code, out)
	}
	if code, _ = f.call(t, "GET", "/runs/99999", f.env.Keys["viewer"], ""); code != 404 {
		t.Errorf("unknown run: %d", code)
	}
	if code, _ = f.call(t, "GET", "/runs/abc", f.env.Keys["viewer"], ""); code != 400 {
		t.Errorf("bad run id: %d", code)
	}
	if code, _ = f.call(t, "POST", "/servers/nope/backup", op, ""); code != 404 {
		t.Errorf("backup of unknown server: %d", code)
	}

	code, out = f.call(t, "GET", "/audit", f.env.Keys["admin"], "")
	found := false
	for _, e := range out["data"].([]any) {
		if strings.Contains(e.(map[string]any)["action"].(string), "backup pg-main") && e.(map[string]any)["actor"] == "api:operator-key" {
			found = true
		}
	}
	if code != 200 || !found {
		t.Errorf("the backup command must be in the audit log with its actor: %d", code)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/openapi.json", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out["openapi"] != "3.0.3" {
		t.Fatalf("openapi: %d %v", rec.Code, err)
	}
	paths := out["paths"].(map[string]any)
	for path, methods := range map[string][]string{
		"/servers":                   {"get", "post"},
		"/servers/{name}":            {"get", "put"},
		"/servers/{name}/check":      {"post"},
		"/servers/{name}/backup":     {"post"},
		"/servers/{name}/backups":    {"get"},
		"/servers/{name}/switch-wal": {"post"},
		"/status":                    {"get"},
		"/runs":                      {"get"},
		"/runs/{id}":                 {"get"},
		"/history":                   {"get"},
		"/audit":                     {"get"},
	} {
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Errorf("openapi lacks %s", path)
			continue
		}
		for _, m := range methods {
			if item[m] == nil {
				t.Errorf("openapi lacks %s %s", m, path)
			}
		}
	}
}

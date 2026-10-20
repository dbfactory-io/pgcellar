package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if st, _ := d["state"].(map[string]any); st == nil || st["streaming_live"] != true || st["last_data_received"] == nil || st["last_filled_wal_time"] == nil || st["last_wal"] == nil || st["stale"] != false {
		t.Errorf("state fields: %v", d["state"])
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
		"/servers":                     {"get", "post"},
		"/servers/{name}":              {"get", "put"},
		"/servers/{name}/check":        {"post"},
		"/servers/{name}/setup":        {"get"},
		"/servers/{name}/backup":       {"post"},
		"/servers/{name}/backups":      {"get"},
		"/servers/{name}/backups/{id}": {"delete"},
		"/servers/{name}/recoveries":   {"get", "post"},
		"/recoveries/{id}":             {"get", "delete"},
		"/recoveries/{id}/download":    {"get"},
		"/servers/{name}/switch-wal":   {"post"},
		"/servers/{name}/drills":       {"get", "post"},
		"/drills":                      {"get"},
		"/drills/{id}":                 {"get"},
		"/drills/{id}/report":          {"get"},
		"/status":                      {"get"},
		"/runs":                        {"get"},
		"/runs/{id}":                   {"get"},
		"/history":                     {"get"},
		"/audit":                       {"get"},
		"/ssh/authorized-keys":         {"get", "post", "delete"},
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

func TestDeleteBackup(t *testing.T) {
	f := newFixture(t)
	path := "/servers/pg-main/backups/20261018T020001"
	if code, _ := f.call(t, "DELETE", path, f.env.Keys["operator"], ""); code != 403 {
		t.Errorf("operator delete: %d", code)
	}
	if code, _ := f.call(t, "DELETE", path, "", ""); code != 401 {
		t.Errorf("anonymous delete: %d", code)
	}
	req := httptest.NewRequest("DELETE", "/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.env.Keys["admin"])
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Errorf("admin delete: %d %q", rec.Code, rec.Body.String())
	}
	if code, out := f.call(t, "DELETE", "/servers/pg-rsync/backups/20261019T230001", f.env.Keys["admin"], ""); code != 409 || !strings.Contains(out["error"].(string), "Minimum redundancy") {
		t.Errorf("minimum redundancy: %d %v", code, out)
	}
	if code, _ := f.call(t, "DELETE", "/servers/pg-main/backups/19990101T000000", f.env.Keys["admin"], ""); code != 404 {
		t.Errorf("unknown backup: %d", code)
	}
	if code, _ := f.call(t, "DELETE", "/servers/nope/backups/20261018T020001", f.env.Keys["admin"], ""); code != 404 {
		t.Errorf("unknown server: %d", code)
	}
	if code, _ := f.call(t, "DELETE", "/servers/pg-main/backups/latest", f.env.Keys["admin"], ""); code != 422 {
		t.Errorf("malformed id: %d", code)
	}
}

// recover starts a restore through the API and waits for its run.
func (f *fixture) recoverOK(t *testing.T, body string) map[string]any {
	t.Helper()
	code, out := f.call(t, "POST", "/servers/pg-main/recoveries", f.env.Keys["operator"], body)
	if code != 202 {
		t.Fatalf("start: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	id := int64(data["run_id"].(float64))
	if data["location"] != "/api/v1/runs/"+strconv.FormatInt(id, 10) {
		t.Errorf("location: %v", data)
	}
	if r := apptest.WaitRun(t, f.env.App, id); r.Status != "ok" {
		t.Fatalf("run: %+v", r)
	}
	code, out = f.call(t, "GET", "/servers/pg-main/recoveries", f.env.Keys["viewer"], "")
	list, _ := out["data"].([]any)
	if code != 200 || len(list) == 0 {
		t.Fatalf("list: %d %v", code, out)
	}
	return list[0].(map[string]any)
}

func TestRecoveriesAPI(t *testing.T) {
	f := newFixture(t)
	op, viewer, admin := f.env.Keys["operator"], f.env.Keys["viewer"], f.env.Keys["admin"]

	if code, _ := f.call(t, "POST", "/servers/pg-main/recoveries", viewer, `{"backup_id":"20261019T020001"}`); code != 403 {
		t.Errorf("viewer start: %d", code)
	}
	if code, _ := f.call(t, "POST", "/servers/pg-main/recoveries", op, `{"backup_id":"20261019T020001","bogus":1}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	if code, out := f.call(t, "POST", "/servers/pg-main/recoveries", op, `{"backup_id":"20261019T020001","target":"time","target_time":"2026-01-01T00:00:00Z"}`); code != 422 || !strings.Contains(out["error"].(string), "before the end of backup") {
		t.Errorf("early target: %d %v", code, out)
	}
	if code, _ := f.call(t, "POST", "/servers/nope/recoveries", op, `{"backup_id":"20261019T020001"}`); code != 404 {
		t.Errorf("unknown server: %d", code)
	}
	if code, _ := f.call(t, "GET", "/servers/nope/recoveries", viewer, ""); code != 404 {
		t.Errorf("list of unknown server: %d", code)
	}

	rec := f.recoverOK(t, `{"backup_id":"20261019T020001","target":"latest","archive":true}`)
	id := strconv.Itoa(int(rec["id"].(float64)))
	if rec["downloadable"] != true || rec["backup_id"] != "20261019T020001" || rec["archive_path"] == "" || !strings.HasPrefix(rec["path"].(string), f.env.Dir) {
		t.Errorf("recovery: %v", rec)
	}
	if code, out := f.call(t, "GET", "/recoveries/"+id, viewer, ""); code != 200 || out["data"].(map[string]any)["path"] != rec["path"] {
		t.Errorf("get: %d %v", code, out)
	}
	if code, _ := f.call(t, "GET", "/recoveries/9999", viewer, ""); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
	if code, _ := f.call(t, "GET", "/recoveries/abc", viewer, ""); code != 400 {
		t.Errorf("bad id: %d", code)
	}

	// Download: operator, attachment, byte ranges.
	if code, _ := f.call(t, "GET", "/recoveries/"+id+"/download", viewer, ""); code != 403 {
		t.Errorf("viewer download: %d", code)
	}
	get := func(hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/recoveries/"+id+"/download", nil)
		req.Header.Set("Authorization", "Bearer "+op)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, req)
		return w
	}
	dl := get()
	if dl.Code != 200 || !strings.HasPrefix(dl.Header().Get("Content-Disposition"), "attachment; filename=\"20261019T020001-") || !strings.HasSuffix(dl.Header().Get("Content-Disposition"), `.tar.gz"`) {
		t.Fatalf("download: %d %v", dl.Code, dl.Header())
	}
	if b := dl.Body.Bytes(); len(b) < 20 || b[0] != 0x1f || b[1] != 0x8b {
		t.Error("not a gzip stream")
	}
	if part := get("Range", "bytes=0-9"); part.Code != http.StatusPartialContent || part.Body.Len() != 10 {
		t.Errorf("range: %d, %d bytes", part.Code, part.Body.Len())
	}

	// A restore without archive: nothing to download.
	rec2 := f.recoverOK(t, `{"backup_id":"20261019T020001","target":"backup_end"}`)
	id2 := strconv.Itoa(int(rec2["id"].(float64)))
	if rec2["target"] != "backup_end" || rec2["downloadable"] == true {
		t.Errorf("no archive: %v", rec2)
	}
	if code, _ := f.call(t, "GET", "/recoveries/"+id2+"/download", op, ""); code != 404 {
		t.Errorf("download without archive: %d", code)
	}

	// Delete: admin only.
	if code, _ := f.call(t, "DELETE", "/recoveries/"+id, op, ""); code != 403 {
		t.Errorf("operator delete: %d", code)
	}
	req := httptest.NewRequest("DELETE", "/api/v1/recoveries/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Errorf("admin delete: %d", w.Code)
	}
	if _, err := os.Stat(rec["path"].(string)); !os.IsNotExist(err) {
		t.Error("directory must be gone")
	}
	if code, _ := f.call(t, "GET", "/recoveries/"+id, viewer, ""); code != 404 {
		t.Errorf("deleted: %d", code)
	}
}

func TestRecoveryDownloadTooLarge(t *testing.T) {
	f := newFixture(t)
	rec := f.recoverOK(t, `{"backup_id":"20261019T020001","archive":true}`)
	id := strconv.Itoa(int(rec["id"].(float64)))

	path := filepath.Join(f.env.Dir, "pgcellar.yaml")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "max_download_bytes: 2147483648", "max_download_bytes: 1", 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)
	deadline := time.Now().Add(10 * time.Second)
	for f.env.App.Config().Recoveries.MaxDownloadBytes != 1 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	code, out := f.call(t, "GET", "/recoveries/"+id+"/download", f.env.Keys["operator"], "")
	if code != 413 || !strings.Contains(out["error"].(string), "max_download_bytes") {
		t.Errorf("too large: %d %v", code, out)
	}
	if _, out := f.call(t, "GET", "/recoveries/"+id, f.env.Keys["viewer"], ""); out["data"].(map[string]any)["downloadable"] == true {
		t.Error("downloadable must be false above the limit")
	}
}

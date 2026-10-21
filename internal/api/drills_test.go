package api

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/report"
)

func TestDrillEndpoints(t *testing.T) {
	f := newFixture(t)
	op, viewer := f.env.Keys["operator"], f.env.Keys["viewer"]

	if code, _ := f.call(t, "POST", "/servers/pg-main/drills", viewer, ""); code != 403 {
		t.Errorf("viewer starts a drill: %d", code)
	}
	if code, _ := f.call(t, "POST", "/servers/nope/drills", op, ""); code != 404 {
		t.Errorf("unknown server: %d", code)
	}
	if code, _ := f.call(t, "POST", "/servers/pg-main/drills", op, `{"target":"yesterday"}`); code != 422 {
		t.Errorf("bad target: %d", code)
	}
	// An empty body drills the latest backup.
	code, out := f.call(t, "POST", "/servers/pg-main/drills", op, "")
	if code != 202 {
		t.Fatalf("start: %d %v", code, out)
	}
	runID := int64(out["data"].(map[string]any)["run_id"].(float64))
	if code, _ := f.call(t, "POST", "/servers/pg-main/drills", op, ""); code != 409 {
		t.Errorf("a second drill while one runs: %d", code)
	}
	apptest.WaitRun(t, f.env.App, runID)

	code, out = f.call(t, "GET", "/servers/pg-main/drills", viewer, "")
	list, _ := out["data"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
	d := list[0].(map[string]any)
	if d["ok"] != true || d["backup_id"] != "20261020T020001" || d["has_report"] != true {
		t.Errorf("drill: %v", d)
	}
	det := d["details"].(map[string]any)
	if !strings.Contains(det["summary"].(string), "restored and started successfully") || len(det["steps"].([]any)) < 8 {
		t.Errorf("details: %v", det)
	}
	if _, has := d["report_path"]; has {
		t.Error("the host path of the report must not be exposed")
	}
	id := strconv.Itoa(int(d["id"].(float64)))
	if code, out := f.call(t, "GET", "/drills/"+id, viewer, ""); code != 200 || out["data"].(map[string]any)["ok"] != true {
		t.Errorf("one drill: %d %v", code, out)
	}
	if code, _ := f.call(t, "GET", "/drills/999", viewer, ""); code != 404 {
		t.Errorf("unknown drill: %d", code)
	}
	if code, out := f.call(t, "GET", "/drills?server=pg-main&limit=5", viewer, ""); code != 200 || len(out["data"].([]any)) != 1 {
		t.Errorf("all drills: %d %v", code, out)
	}

	// The report: PDF, operator role.
	req := httptest.NewRequest("GET", "/api/v1/drills/"+id+"/report", nil)
	req.Header.Set("Authorization", "Bearer "+viewer)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("viewer report: %d", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer "+op)
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("report: %d %v", rec.Code, rec.Header())
	}
	if _, err := report.Check(rec.Body.Bytes()); err != nil {
		t.Error(err)
	}

	// The state of the server carries the drill status.
	_, srv := f.call(t, "GET", "/servers/pg-main", viewer, "")
	st := srv["data"].(map[string]any)["state"].(map[string]any)["drill"].(map[string]any)
	if st["last"].(map[string]any)["ok"] != true || !strings.Contains(st["text"].(string), "Last drill: just now") {
		t.Errorf("state.drill = %v", st)
	}
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

func TestUpdatesAPI(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"tag_name":"v0.1.2","published_at":"2026-10-21T08:00:00Z","html_url":"https://example.com/notes","assets":[]}`)
	}))
	defer gh.Close()
	env := apptest.NewWith(t, func(c *config.Config) { c.Updates.Source = config.Ptr(gh.URL) })
	env.App.Version = "0.1.0"
	f := &fixture{env: env, h: New(env.App).Handler()}

	code, out := f.call(t, "GET", "/updates", f.env.Keys["viewer"], "")
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["current"] != "0.1.0" || data["available"] != false || data["window"] != "02:00-05:00" {
		t.Fatalf("before a check: %d %v", code, out)
	}
	if code, _ := f.call(t, "POST", "/updates/check", f.env.Keys["viewer"], ""); code != 403 {
		t.Errorf("viewer check: %d", code)
	}
	code, out = f.call(t, "POST", "/updates/check", f.env.Keys["operator"], "")
	data, _ = out["data"].(map[string]any)
	if code != 200 || data["latest"] != "0.1.2" || data["available"] != true || data["notes_url"] != "https://example.com/notes" {
		t.Fatalf("check: %d %v", code, out)
	}

	ready := func(key, query string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "/api/v1/updates/ready"+query, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	if code, _ := ready("", ""); code != 401 {
		t.Errorf("ready without a key: %d", code)
	}
	if code, _ := ready(f.env.Keys["viewer"], ""); code != 403 {
		t.Errorf("ready with a viewer key: %d", code)
	}
	// The test clock is 02:30, inside the default window.
	if code, m := ready(f.env.Keys["operator"], ""); code != 200 || m["ready"] != true {
		t.Errorf("ready: %d %v", code, m)
	}
	noon := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	env.App.Now = func() time.Time { return noon }
	if code, m := ready(f.env.Keys["operator"], ""); code != 409 || m["ready"] != false || m["reason"] == "" {
		t.Errorf("outside the window: %d %v", code, m)
	}
	if code, m := ready(f.env.Keys["operator"], "?now=1"); code != 200 || m["ready"] != true {
		t.Errorf("?now=1: %d %v", code, m)
	}

	// An unreachable source: 502 with the error and the status.
	env2 := apptest.NewWith(t, func(c *config.Config) { c.Updates.Source = config.Ptr("http://127.0.0.1:1/latest") })
	f2 := &fixture{env: env2, h: New(env2.App).Handler()}
	code, out = f2.call(t, "POST", "/updates/check", f2.env.Keys["operator"], "")
	if code != 502 || out["error"] == nil || out["data"] == nil {
		t.Errorf("failed check: %d %v", code, out)
	}
}

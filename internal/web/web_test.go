package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/app/apptest"
	"github.com/stranix79/pgcellar/internal/store"
)

type fixture struct {
	env *apptest.Env
	srv *Server
	h   http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := apptest.New(t)
	s, err := New(env.App)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, srv: s, h: s.Handler()}
}

func (f *fixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(path string, c *http.Cookie, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return f.do(req)
}

func (f *fixture) post(path string, c *http.Cookie, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return f.do(req)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
var tokenRe = regexp.MustCompile(`X-CSRF-Token": "([0-9a-f]+)"`)

// login opens a session directly in the store (bcrypt is slow, and the real
// login form is covered by TestLoginSessionAndCookie) and returns the cookie
// and the CSRF token.
func (f *fixture) login(t *testing.T, role string) (*http.Cookie, string) {
	t.Helper()
	tok := "test-token-" + role
	id := hashToken(tok)
	now := time.Now()
	if err := f.env.App.Store.CreateSession(store.Session{ID: id, User: role + "-user", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: cookieName, Value: tok}, f.srv.csrfFor(id)
}

func TestLoginSessionAndCookie(t *testing.T) {
	f := newFixture(t)

	if rec := f.get("/login", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Sign in") {
		t.Fatalf("login page: %d", rec.Code)
	}
	rec := f.get("/status", nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("anonymous /status: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	tok := csrfRe.FindStringSubmatch(f.get("/login", nil).Body.String())[1]
	if rec := f.post("/login", nil, url.Values{"user": {"admin-user"}, "password": {apptest.Password}}); rec.Code != http.StatusForbidden {
		t.Errorf("login without token: %d", rec.Code)
	}
	if rec := f.post("/login", nil, url.Values{"csrf": {tok}, "user": {"admin-user"}, "password": {"wrong"}}); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Wrong user name or password") {
		t.Errorf("wrong password: %d", rec.Code)
	}
	if rec := f.post("/login", nil, url.Values{"csrf": {tok}, "user": {"nobody"}, "password": {"x"}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown user: %d", rec.Code)
	}

	rec = f.post("/login", nil, url.Values{"csrf": {tok}, "user": {"admin-user"}, "password": {apptest.Password}, "next": {"//evil.example"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/status" {
		t.Fatalf("login: %d, redirect %q (open redirect must be refused)", rec.Code, rec.Header().Get("Location"))
	}
	c := rec.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Errorf("cookie flags: %+v", c)
	}
	rec = f.post("/login", nil, url.Values{"csrf": {tok}, "user": {"admin-user"}, "password": {apptest.Password}}, "X-Forwarded-Proto", "https")
	if c := rec.Result().Cookies()[0]; !c.Secure {
		t.Error("cookie must be Secure behind HTTPS")
	}

	if rec := f.get("/status", c); rec.Code != 200 || !strings.Contains(rec.Body.String(), "pg-main") {
		t.Errorf("status with session: %d", rec.Code)
	}
}

func TestLogoutAndCSRF(t *testing.T) {
	f := newFixture(t)
	cookie, csrf := f.login(t, "viewer")

	if rec := f.post("/logout", cookie, url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("logout without CSRF token: %d", rec.Code)
	}
	if rec := f.post("/logout", cookie, url.Values{"csrf": {"deadbeef"}}); rec.Code != http.StatusForbidden {
		t.Errorf("logout with wrong CSRF token: %d", rec.Code)
	}
	if rec := f.post("/logout", cookie, url.Values{"csrf": {csrf}}); rec.Code != http.StatusSeeOther {
		t.Errorf("logout: %d", rec.Code)
	}
	if rec := f.get("/status", cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("session must be dead after logout: %d", rec.Code)
	}
}

func TestRoles(t *testing.T) {
	f := newFixture(t)
	viewer, vcsrf := f.login(t, "viewer")
	operator, _ := f.login(t, "operator")

	if rec := f.get("/servers/new", viewer); rec.Code != http.StatusForbidden {
		t.Errorf("viewer on /servers/new: %d", rec.Code)
	}
	if rec := f.get("/audit", operator); rec.Code != http.StatusForbidden {
		t.Errorf("operator on /audit: %d", rec.Code)
	}
	if rec := f.post("/servers/pg-main/backup", viewer, url.Values{"csrf": {vcsrf}}); rec.Code != http.StatusForbidden {
		t.Errorf("viewer backup: %d", rec.Code)
	}
	if rec := f.post("/servers/pg-main/backup", operator, url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("operator backup without CSRF: %d", rec.Code)
	}
	if rec := f.get("/servers/pg-main/check", viewer, "HX-Request", "true"); rec.Code != http.StatusForbidden {
		t.Errorf("viewer check: %d", rec.Code)
	}
	if body := f.get("/servers/pg-main", viewer).Body.String(); strings.Contains(body, "Backup now") || strings.Contains(body, `type="submit">Save changes`) {
		t.Error("viewer must not see action buttons")
	}
	if body := f.get("/servers/pg-main", operator).Body.String(); !strings.Contains(body, "Backup now") || strings.Contains(body, `type="submit">Save changes`) {
		t.Error("operator sees backup buttons but no save button")
	}
}

func TestServersListAndDetail(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")

	list := f.get("/servers", admin).Body.String()
	for _, want := range []string{"pg-main", "pg-legacy", "pg-rsync", "broken", "Every day at 02:00"} {
		if !strings.Contains(list, want) {
			t.Errorf("/servers lacks %q", want)
		}
	}

	rec := f.get("/servers/pg-main", admin)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("detail: %d", rec.Code)
	}
	for _, want := range []string{"20261020T020001", "DONE", "1.4 GiB", "pg-main.example.com", "Run check", "Save changes", `name="backup_method"`} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	if rec := f.get("/servers/does-not-exist", admin); rec.Code != 404 {
		t.Errorf("unknown server: %d", rec.Code)
	}
	if strings.Contains(f.get("/servers/does-not-exist", admin).Body.String(), "goroutine") {
		t.Error("no stack trace on screen")
	}
}

func TestStatusLevels(t *testing.T) {
	f := newFixture(t)
	c, _ := f.login(t, "viewer")
	body := f.get("/status", c).Body.String()
	for _, want := range []string{`lv-ok`, `lv-warn`, `lv-fail`, "Healthy", "Attention", "Failing", "No completed backup exists for broken"} {
		if !strings.Contains(body, want) {
			t.Errorf("/status lacks %q", want)
		}
	}
	states, _, _ := f.env.App.States()
	got := map[string]string{}
	for _, s := range states {
		got[s.Name] = s.Level
	}
	if got["pg-main"] != "ok" || got["pg-rsync"] != "warn" || got["broken"] != "fail" {
		t.Errorf("levels: %v", got)
	}
}

func TestCheckFragment(t *testing.T) {
	f := newFixture(t)
	c, _ := f.login(t, "operator")
	rec := f.get("/servers/broken/check", c, "HX-Request", "true")
	body := rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, "<html") {
		t.Fatalf("fragment: %d", rec.Code)
	}
	for _, want := range []string{"FAILED", "How to fix:", "WAL receiver"} {
		if !strings.Contains(body, want) {
			t.Errorf("check fragment lacks %q", want)
		}
	}
}

func TestBackupRunFollowed(t *testing.T) {
	f := newFixture(t)
	c, csrf := f.login(t, "operator")

	rec := f.post("/servers/pg-main/backup", c, url.Values{}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 200 {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	m := regexp.MustCompile(`id="run-(\d+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no run box: %s", body)
	}
	run := f.env.App
	id, _ := strconv.ParseInt(m[1], 10, 64)
	apptest.WaitRun(t, run, id)

	frag := f.get("/runs/"+m[1]+"/fragment", c, "HX-Request", "true").Body.String()
	if !strings.Contains(frag, "finished") || strings.Contains(frag, "hx-trigger") {
		t.Errorf("finished fragment must stop polling:\n%s", frag)
	}
	if !strings.Contains(frag, "Backup completed") {
		t.Errorf("fragment lacks the output:\n%s", frag)
	}
	if !strings.Contains(apptest.Calls(t, f.env), "backup pg-main") {
		t.Errorf("fake barman was not called: %q", apptest.Calls(t, f.env))
	}
	if list := f.get("/runs", c).Body.String(); !strings.Contains(list, "#"+m[1]) {
		t.Error("run missing from /runs")
	}
	// A failing backup is recorded as failed, with the reason.
	rec = f.post("/servers/broken/backup", c, url.Values{}, "HX-Request", "true", "X-CSRF-Token", csrf)
	for _, r := range mustRuns(t, run) {
		if r.Server == "broken" {
			if got := apptest.WaitRun(t, run, r.ID); got.Status != app.RunFailed || !strings.Contains(got.Stderr, "Cannot connect") {
				t.Errorf("failed run: %+v", got)
			}
		}
	}
}

func mustRuns(t *testing.T, a *app.App) []app.RunView {
	t.Helper()
	runs, err := a.Runs("", 10)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestSwitchWAL(t *testing.T) {
	f := newFixture(t)
	c, csrf := f.login(t, "operator")
	rec := f.post("/servers/pg-main/switch-wal", c, url.Values{}, "HX-Request", "true", "X-CSRF-Token", csrf)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "WAL switched on pg-main") {
		t.Errorf("switch-wal: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAndEditServer(t *testing.T) {
	f := newFixture(t)
	admin, csrf := f.login(t, "admin")

	form := url.Values{
		"csrf": {csrf}, "name": {"pg-new"}, "label": {"New one"}, "backup_method": {"postgres"},
		"conninfo": {"host=new user=barman dbname=postgres"}, "streaming_conninfo": {"host=new user=streaming_barman dbname=postgres"},
		"slot_name": {"barman"}, "recovery_window_days": {"7"}, "minimum_redundancy": {"1"},
		"schedule_backup": {"daily 03:15"}, "schedule_enabled": {"1"},
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("schedule_backup", "whenever")
	rec := f.post("/servers", admin, bad)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot understand schedule") {
		t.Fatalf("bad schedule: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(f.env.BarmanDir, "pg-new.conf")); err == nil {
		t.Fatal("a rejected form must not write a file")
	}

	rec = f.post("/servers", admin, form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/servers/pg-new?ok=created" {
		t.Fatalf("create: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	conf, err := os.ReadFile(filepath.Join(f.env.BarmanDir, "pg-new.conf"))
	if err != nil || !strings.Contains(string(conf), "retention_policy = RECOVERY WINDOW OF 7 DAYS") || !strings.Contains(string(conf), "streaming_archiver = on") {
		t.Fatalf("barman.d file: %v\n%s", err, conf)
	}
	yml, _ := os.ReadFile(filepath.Join(f.env.Dir, "pgcellar.yaml"))
	if !strings.Contains(string(yml), "name: pg-new") || !strings.Contains(string(yml), "daily 03:15") {
		t.Errorf("pgcellar.yaml lacks the server:\n%s", yml)
	}
	if rec := f.post("/servers", admin, form); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusConflict {
		// the duplicate is reported as a problem in the form
		t.Errorf("duplicate: %d", rec.Code)
	}

	// The commit exists and the author is the pgCellar user.
	out, err := exec.Command("git", "-C", f.env.Dir, "log", "--format=%an|%s").Output()
	if err != nil || !strings.Contains(string(out), "admin-user|add server pg-new") {
		t.Fatalf("git log: %v %q", err, out)
	}

	// Edit through the detail form.
	edit := url.Values{
		"csrf": {csrf}, "label": {"Renamed label"}, "backup_method": {"postgres"}, "description": {"edited"},
		"conninfo": {"host=new2 user=barman dbname=postgres"}, "streaming_conninfo": {"host=new2 user=streaming_barman dbname=postgres"},
		"slot_name": {"barman"}, "recovery_window_days": {"10"}, "schedule_backup": {"daily 04:00"},
	}
	rec = f.post("/servers/pg-new", admin, edit)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	conf, _ = os.ReadFile(filepath.Join(f.env.BarmanDir, "pg-new.conf"))
	if !strings.Contains(string(conf), "10 DAYS") || !strings.Contains(string(conf), "host=new2") {
		t.Errorf("edit not written:\n%s", conf)
	}
	out, _ = exec.Command("git", "-C", f.env.Dir, "log", "--format=%an|%s").Output()
	if !strings.Contains(string(out), "admin-user|update server pg-new") {
		t.Errorf("no update commit: %q", out)
	}
	if rec := f.get("/history", admin); !strings.Contains(rec.Body.String(), "update server pg-new") {
		t.Error("/history lacks the commit")
	}
	hist, _ := f.env.App.History(5)
	if len(hist) == 0 {
		t.Fatal("no history")
	}
	if rec := f.get("/history?repo=config&diff="+hist[0].Hash, admin); !strings.Contains(rec.Body.String(), "diff --git") {
		t.Error("diff not shown")
	}

	// A rejected edit keeps what the user typed and says why.
	edit.Set("recovery_window_days", "3")
	edit.Set("redundancy", "2")
	rec = f.post("/servers/pg-new", admin, edit)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "either a recovery window or a redundancy") || !strings.Contains(rec.Body.String(), "host=new2") {
		t.Errorf("rejected edit: %d", rec.Code)
	}
}

func TestMetricsAndHealth(t *testing.T) {
	f := newFixture(t)
	if rec := f.get("/healthz", nil); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	rec := f.get("/metrics", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE pgcellar_last_backup_age_seconds gauge",
		`pgcellar_last_backup_age_seconds{server="pg-main"} 1170`,
		`pgcellar_last_wal_age_seconds{server="pg-main"} `,
		`pgcellar_last_wal_received_age_seconds{server="pg-main"} 300`,
		`pgcellar_wal_streaming_live{server="pg-main"} 1`,
		`pgcellar_wal_streaming_live{server="broken"} 0`,
		`pgcellar_backups_total{server="pg-main"} 3`,
		`pgcellar_check_ok{server="pg-main"} 1`,
		`pgcellar_check_ok{server="broken"} 0`,
		`pgcellar_build_info{version="test"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lacks %q", want)
		}
	}
	line := regexp.MustCompile(`^(#.*|[a-z_]+\{[a-z]+="[^"]*"\} [0-9]+)$`)
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if !line.MatchString(l) {
			t.Errorf("not valid exposition format: %q", l)
		}
	}
}

func TestStaticAndNoExternalResources(t *testing.T) {
	f := newFixture(t)
	if rec := f.get("/static/htmx.min.js", nil); rec.Code != 200 || rec.Body.Len() < 10000 {
		t.Errorf("htmx.min.js: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if rec := f.get("/static/app.css", nil); rec.Code != 200 {
		t.Errorf("app.css: %d", rec.Code)
	}
	for _, p := range []string{"/login", "/nope"} {
		body := f.get(p, nil).Body.String()
		if regexp.MustCompile(`(src|href)="https?://`).MatchString(body) {
			t.Errorf("%s loads an external resource", p)
		}
	}
	if rec := f.get("/nope", nil); rec.Code != 404 {
		t.Errorf("404 page: %d", rec.Code)
	}
}

func TestSessionExpires(t *testing.T) {
	f := newFixture(t)
	c, _ := f.login(t, "viewer")
	id := hashToken(c.Value)
	if err := f.env.App.Store.DeleteSession(id); err != nil {
		t.Fatal(err)
	}
	if rec := f.get("/status", c); rec.Code != http.StatusSeeOther {
		t.Errorf("deleted session: %d", rec.Code)
	}
}

// The pages read the cache: opening the list, a server page and the status
// page runs no Barman command, and the WAL wording is the agreed one.
func TestPagesNeverRunBarman(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.login(t, "admin")
	count := func() int {
		b, _ := os.ReadFile(f.env.CallLog)
		return strings.Count(string(b), "\n")
	}
	before := count()
	start := time.Now()
	list := f.get("/servers", admin)
	listTook := time.Since(start)
	page := f.get("/servers/pg-main", admin)
	status := f.get("/status", admin)
	if list.Code != 200 || page.Code != 200 || status.Code != 200 {
		t.Fatalf("codes %d %d %d", list.Code, page.Code, status.Code)
	}
	if after := count(); after != before {
		t.Fatalf("pages ran Barman: %d -> %d", before, after)
	}
	if listTook > 500*time.Millisecond {
		t.Errorf("/servers took %v", listTook)
	}
	for _, want := range []string{"Streaming:", "live", "last data 5 min ago", "Last filled WAL file: 19 min ago"} {
		if !strings.Contains(status.Body.String(), want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	for _, want := range []string{"Last filled WAL file (a WAL file is 16 MB; a quiet server fills one slowly, the nightly backup closes one)", "Cached status from"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("server page lacks %q", want)
		}
	}
}

package web

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/demo"
	"github.com/stranix79/pgcellar/internal/store"
)

// demoFixture is the public demo behind its web interface, signed in as demo.
type demoFixture struct {
	app    *app.App
	h      http.Handler
	cookie *http.Cookie
	csrf   string
}

func newDemoFixture(t *testing.T) *demoFixture {
	t.Helper()
	t.Setenv("PATH", os.Getenv("PATH"))
	for _, k := range []string{"PGCELLAR_TELEGRAM_TOKEN", "PGCELLAR_SMTP_USER", "PGCELLAR_SMTP_PASSWORD", "PGCELLAR_BASE_URL"} {
		t.Setenv(k, "")
	}
	d, err := demo.Open(demo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.App.Close() })
	s, err := New(d.App)
	if err != nil {
		t.Fatal(err)
	}
	f := &demoFixture{app: d.App, h: s.Handler()}
	tok := "demo-test-token"
	id := hashToken(tok)
	now := time.Now()
	if err := d.App.Store.CreateSession(store.Session{ID: id, User: demo.User, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.cookie, f.csrf = &http.Cookie{Name: cookieName, Value: tok}, s.csrfFor(id)
	return f
}

func (f *demoFixture) do(method, path string, signedIn bool, hdr ...string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == "GET" || method == "HEAD" {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(url.Values{"csrf": {f.csrf}}.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if method != "GET" && method != "HEAD" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if signedIn {
		req.AddCookie(f.cookie)
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// mutatingRoutes reads the routes registered in web.go: every POST, PUT,
// PATCH and DELETE pattern, so that a route added later is tested without
// anybody having to remember it.
func mutatingRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"(POST|PUT|PATCH|DELETE) (/[^"]*)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if m[2] == "/login" || m[2] == "/logout" {
			continue
		}
		p := strings.NewReplacer("{name}", "erp-prod", "{id}", "1").Replace(m[2])
		out = append(out, m[1]+" "+p)
	}
	if len(out) < 12 {
		t.Fatalf("only %d mutating routes found in web.go: %v", len(out), out)
	}
	return out
}

func TestDemoRefusesEveryMutation(t *testing.T) {
	f := newDemoFixture(t)
	runsBefore, _ := f.app.Runs("", 100)
	auditBefore, _ := f.app.Audit(500)
	routes := append(mutatingRoutes(t),
		"PUT /api/v1/servers/erp-prod", "DELETE /api/v1/servers/erp-prod", "POST /api/v1/servers/erp-prod/backup",
		"POST /api/v1/servers/erp-prod/recoveries", "PATCH /servers/erp-prod", "PUT /servers/erp-prod", "DELETE /servers/erp-prod",
		"POST /api/v1/alerts/test",
		// GET routes that run a command or hand out an archive
		"GET /servers/erp-prod/check", "GET /recoveries/1/download")
	for _, r := range routes {
		method, path, _ := strings.Cut(r, " ")
		for _, signedIn := range []bool{true, false} {
			for _, hx := range [][]string{nil, {"HX-Request", "true"}} {
				rec := f.do(method, path, signedIn, hx...)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s (signed in %v, htmx %v): status %d, want 403", r, signedIn, hx != nil, rec.Code)
					continue
				}
				if !strings.Contains(rec.Body.String(), "This is the public demo: read-only.") || !strings.Contains(rec.Body.String(), "https://dbfactory.io/pgcellar") {
					t.Errorf("%s: no demo message in %q", r, rec.Body.String())
				}
			}
		}
	}
	runsAfter, _ := f.app.Runs("", 100)
	auditAfter, _ := f.app.Audit(500)
	if len(runsAfter) != len(runsBefore) || len(auditAfter) != len(auditBefore) {
		t.Errorf("a refused request left traces: runs %d -> %d, audit %d -> %d", len(runsBefore), len(runsAfter), len(auditBefore), len(auditAfter))
	}
	// The messages of the pages themselves are fine to read.
	if rec := f.do("GET", "/status", true); rec.Code != 200 {
		t.Errorf("/status: %d", rec.Code)
	}
}

func TestDemoPagesReadableByViewer(t *testing.T) {
	f := newDemoFixture(t)
	for _, p := range []string{"/status", "/servers", "/servers/erp-prod", "/servers/shop-staging", "/runs", "/runs/1", "/history", "/ssh-key", "/alerts", "/audit",
		"/servers/erp-prod/restore", "/servers/new", "/servers/erp-prod/backups/list", "/servers/crm-prod/recoveries/list", "/status/table"} {
		rec := f.do("GET", p, true)
		if rec.Code != 200 {
			t.Errorf("GET %s: %d", p, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "Northwind") && !strings.Contains(rec.Body.String(), "erp-prod") && !strings.Contains(rec.Body.String(), "crm-prod") {
			t.Errorf("GET %s: no fictional data in the page", p)
		}
	}
	// The PDF of a drill is fictional and can be downloaded; archives cannot.
	rec := f.do("GET", "/drills/1/report", true)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(rec.Body.String(), "%PDF") {
		t.Errorf("drill report: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := f.do("GET", "/recoveries/1/download", true); rec.Code != 403 {
		t.Errorf("archive download: %d", rec.Code)
	}
	// Buttons stay on the page: they are disabled by demo.js, and the server refuses anyway.
	body := f.do("GET", "/servers/erp-prod", true).Body.String()
	for _, want := range []string{"Backup now", "Switch WAL", "demo.js", `data-demo-msg=`, "Restore"} {
		if !strings.Contains(body, want) {
			t.Errorf("server page lacks %q", want)
		}
	}
	if !strings.Contains(body, "Demo · read-only · fictional data ·") || !strings.Contains(body, `href="https://dbfactory.io/pgcellar"`) {
		t.Error("the demo banner is missing")
	}
	js := f.do("GET", "/static/demo.js", false)
	if js.Code != 200 || !strings.Contains(js.Body.String(), "disabled") {
		t.Errorf("demo.js: %d", js.Code)
	}
}

// Outside the demo the pages of the administrators stay for administrators.
func TestAdminPagesStayAdminOutsideDemo(t *testing.T) {
	f := newFixture(t)
	c, _ := f.login(t, "viewer")
	for _, p := range []string{"/ssh-key", "/alerts", "/audit", "/servers/new", "/servers/pg-main/restore"} {
		if rec := f.get(p, c); rec.Code != 403 {
			t.Errorf("viewer GET %s: %d, want 403", p, rec.Code)
		}
	}
	if body := f.get("/status", c).Body.String(); strings.Contains(body, "demobar") || strings.Contains(body, "demo.js") {
		t.Error("the demo banner shows outside the demo")
	}
	if rec := f.get("/login", nil); strings.Contains(rec.Body.String(), "Try the demo") || rec.Header().Get("X-Robots-Tag") != "" {
		t.Error("demo features outside the demo")
	}
}

func TestDemoLoginPage(t *testing.T) {
	f := newDemoFixture(t)
	rec := f.do("GET", "/login", false)
	body := rec.Body.String()
	if !strings.Contains(body, "Try the demo") || !strings.Contains(body, "demo / demo") || !strings.Contains(body, `name="user" value="demo"`) || !strings.Contains(body, `name="password" value="demo"`) {
		t.Errorf("login page of the demo:\n%s", body)
	}
	// The button's form really signs in.
	tok := csrfRe.FindStringSubmatch(body)[1]
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"csrf": {tok}, "user": {"demo"}, "password": {"demo"}, "next": {"/status"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	out := httptest.NewRecorder()
	f.h.ServeHTTP(out, req)
	if out.Code != http.StatusSeeOther || len(out.Result().Cookies()) == 0 {
		t.Errorf("demo login: %d", out.Code)
	}
	// No account other than demo, no admin.
	for _, u := range f.app.Config().Users {
		if u.Name != demo.User || u.Role != app.RoleViewer {
			t.Errorf("user %s (%s)", u.Name, u.Role)
		}
	}
	if len(f.app.Config().APIKeys) != 0 {
		t.Error("the demo has API keys")
	}
}

func TestDemoNoindex(t *testing.T) {
	f := newDemoFixture(t)
	for _, p := range []string{"/login", "/status", "/static/app.css", "/healthz", "/nothing-here"} {
		if got := f.do("GET", p, true).Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("GET %s: X-Robots-Tag %q", p, got)
		}
	}
	if got := f.do("POST", "/servers", true).Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("403 page: X-Robots-Tag %q", got)
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := newFixture(t)
	post := func(remote, realIP string) int {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"csrf": {"x"}, "user": {"nobody"}, "password": {"x"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = remote
		if realIP != "" {
			req.Header.Set("X-Real-IP", realIP)
		}
		return f.do(req).Code
	}
	for i := 1; i <= 10; i++ {
		if code := post("203.0.113.7:4000", ""); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d already limited", i)
		}
	}
	if code := post("203.0.113.7:4001", ""); code != http.StatusTooManyRequests {
		t.Errorf("11th attempt: %d, want 429", code)
	}
	// Another address is not affected, and a stranger cannot pick its address.
	if code := post("203.0.113.8:4000", ""); code == http.StatusTooManyRequests {
		t.Error("another address is limited")
	}
	if code := post("203.0.113.7:4002", "198.51.100.1"); code != http.StatusTooManyRequests {
		t.Errorf("a spoofed X-Real-IP got through: %d", code)
	}
	// Behind the reverse proxy, each visitor has his own counter.
	for i := 1; i <= 10; i++ {
		post("10.0.0.2:5000", "198.51.100.20")
	}
	if code := post("10.0.0.2:5001", "198.51.100.20"); code != http.StatusTooManyRequests {
		t.Errorf("proxied visitor, 11th attempt: %d", code)
	}
	if code := post("10.0.0.2:5002", "198.51.100.21"); code == http.StatusTooManyRequests {
		t.Error("another proxied visitor is limited")
	}
}

// The whole sign-in, as a browser or curl does it, against the demo started like
// `serve --demo` does, with the real clock and a cookie jar.
func TestDemoSignInEndToEnd(t *testing.T) {
	f := newDemoFixture(t)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, string, string) {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("Location")
	}
	// The login page holds two forms (the usual one and "Try the demo"), so two
	// csrf fields: a client must send one of them, not both glued together.
	for form := 0; form < 2; form++ {
		code, page, _ := get("/login")
		toks := csrfRe.FindAllStringSubmatch(page, -1)
		if code != 200 || len(toks) != 2 {
			t.Fatalf("login page: %d, %d tokens", code, len(toks))
		}
		resp, err := client.PostForm(ts.URL+"/login", url.Values{"csrf": {toks[form][1]}, "next": {"/status"}, "user": {"demo"}, "password": {"demo"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/status" {
			t.Fatalf("form %d: sign-in answered %d", form, resp.StatusCode)
		}
		if code, body, _ := get("/status"); code != 200 || !strings.Contains(body, "erp-prod") {
			t.Fatalf("form %d: /status answered %d", form, code)
		}
		// Sign out: the token of the page, then back to the login page.
		_, page, _ = get("/status")
		out := csrfRe.FindStringSubmatch(page)
		if out == nil {
			t.Fatal("no csrf field for the sign-out form")
		}
		resp, err = client.PostForm(ts.URL+"/logout", url.Values{"csrf": {out[1]}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("logout answered %d", resp.StatusCode)
		}
		if code, _, loc := get("/status"); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
			t.Fatalf("after logout /status answered %d %s", code, loc)
		}
	}
	// Both tokens glued together (what a careless script sends) is refused.
	_, page, _ := get("/login")
	toks := csrfRe.FindAllStringSubmatch(page, -1)
	resp, _ := client.PostForm(ts.URL+"/login", url.Values{"csrf": {toks[0][1] + "\n" + toks[1][1]}, "user": {"demo"}, "password": {"demo"}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("glued tokens: %d", resp.StatusCode)
	}
}

package web

import (
	"regexp"
	"testing"
)

func TestStaticAssetsVersioned(t *testing.T) {
	f := newFixture(t)
	body := f.get("/login", nil).Body.String()
	m := regexp.MustCompile(`/static/app\.css\?v=([0-9a-f]{12})"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("login page has no versioned app.css URL:\n%s", body)
	}
	for _, name := range []string{"htmx.min.js", "favicon.svg"} {
		if !regexp.MustCompile(`/static/` + regexp.QuoteMeta(name) + `\?v=[0-9a-f]{12}"`).MatchString(body) {
			t.Errorf("%s is not versioned", name)
		}
	}
	cases := map[string]string{
		"/static/app.css?v=" + m[1]:  "public, max-age=31536000, immutable",
		"/static/app.css":            "no-cache",
		"/static/app.css?v=mauvaise": "no-cache",
	}
	for path, want := range cases {
		rec := f.get(path, nil)
		if rec.Code != 200 {
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", path, got, want)
		}
	}
	if got := f.get("/login", nil).Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("HTML Cache-Control %q, want no-store", got)
	}
}

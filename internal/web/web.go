// Package web is the HTML presentation of pgCellar: server-rendered pages,
// HTMX fragments, sessions and CSRF. All decisions live in internal/app.
package web

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/api"
	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/web"
)

// Server serves the web interface.
type Server struct {
	app     *app.App
	tpl     map[string]*template.Template
	static  http.Handler
	assets  map[string]string
	secret  []byte
	limiter *limiter
	rate    *rateLimiter
	apiH    http.Handler
}

var pages = []string{"login", "status", "servers", "server_new", "server", "restore", "ssh_key", "runs", "run", "history", "audit", "error", "alerts"}

func funcs(assets map[string]string) template.FuncMap {
	return template.FuncMap{
		"asset":      func(name string) string { return "/static/" + name + "?v=" + assets[name] },
		"t":          tr,
		"ts":         fmtTime,
		"when":       fmtWhen,
		"span":       fmtSpan,
		"whenstr":    fmtWhenStr,
		"okcount":    okCount,
		"setupok":    setupOK,
		"ago":        func(t time.Time) string { return app.HumanDuration(time.Since(t)) },
		"ageSec":     ageSec,
		"size":       humanSize,
		"retention":  fmtRetention,
		"nexttext":   fmtNext,
		"lastsched":  fmtLastScheduled,
		"actor":      actorLabel,
		"runtime":    runtime,
		"f1":         f1,
		"failedstep": failedStep,
	}
}

// New builds the web server on top of an App.
func New(a *app.App) (*Server, error) {
	s := &Server{app: a, tpl: map[string]*template.Template{}, limiter: newLimiter(), rate: newRateLimiter(loginRate)}
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	if s.assets, err = fingerprints(sub); err != nil {
		return nil, err
	}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs(s.assets)).ParseFS(web.FS, "templates/layout.html", "templates/fragments.html", "templates/storage.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", p, err)
		}
		s.tpl[p] = t
	}
	s.static = http.StripPrefix("/static/", s.cacheControl(http.FileServerFS(sub)))
	secret, ok, err := a.Store.GetSetting("csrf_secret")
	if err != nil {
		return nil, err
	}
	if !ok || len(secret) < 32 {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		secret = hex.EncodeToString(buf)
		if err := a.Store.SetSetting("csrf_secret", secret); err != nil {
			return nil, err
		}
	}
	s.secret = []byte(secret)
	s.apiH = api.New(a).Handler()
	return s, nil
}

// fingerprints returns the first 12 hex digits of the SHA-256 of every
// embedded static file, computed once at startup.
func fingerprints(fsys fs.FS) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[p] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return out, err
}

// cacheControl makes a file immutable when the request carries its current
// fingerprint (?v=), and otherwise forces revalidation (ETag/Last-Modified).
func (s *Server) cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if v := r.URL.Query().Get("v"); v != "" && v == s.assets[name] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

// Handler returns the whole HTTP handler: pages, API, health and metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.Handle("/api/", s.apiH)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/status", http.StatusSeeOther) })
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.guard(app.RoleViewer, s.logout))

	mux.HandleFunc("GET /status", s.guard(app.RoleViewer, s.statusPage))
	mux.HandleFunc("GET /status/table", s.guard(app.RoleViewer, s.statusTable))
	mux.HandleFunc("GET /servers", s.guard(app.RoleViewer, s.serversPage))
	mux.HandleFunc("GET /servers/new", s.guard(app.RoleAdmin, s.serverNewForm))
	mux.HandleFunc("POST /servers", s.guard(app.RoleAdmin, s.serverCreate))
	mux.HandleFunc("GET /servers/{name}", s.guard(app.RoleViewer, s.serverPage))
	mux.HandleFunc("POST /servers/{name}", s.guard(app.RoleAdmin, s.serverUpdate))
	mux.HandleFunc("GET /servers/{name}/check", s.guard(app.RoleOperator, s.serverCheck))
	mux.HandleFunc("POST /servers/{name}/backup", s.guard(app.RoleOperator, s.serverBackup))
	mux.HandleFunc("POST /servers/{name}/switch-wal", s.guard(app.RoleOperator, s.serverSwitchWAL))
	mux.HandleFunc("GET /servers/{name}/backups/list", s.guard(app.RoleViewer, s.backupListFragment))
	mux.HandleFunc("POST /servers/{name}/backups/{id}/delete", s.guard(app.RoleAdmin, s.serverBackupDelete))
	mux.HandleFunc("POST /servers/{name}/ssh-test", s.guard(app.RoleAdmin, s.serverSSHTest))
	mux.HandleFunc("GET /ssh-key", s.guard(app.RoleAdmin, s.sshKeyPage))
	mux.HandleFunc("POST /ssh-key/authorized-keys", s.guard(app.RoleAdmin, s.sshKeyAllow))
	mux.HandleFunc("POST /ssh-key/authorized-keys/revoke", s.guard(app.RoleAdmin, s.sshKeyRevoke))
	mux.HandleFunc("GET /servers/{name}/restore", s.guard(app.RoleOperator, s.restoreForm))
	mux.HandleFunc("POST /servers/{name}/restore", s.guard(app.RoleOperator, s.restoreSubmit))
	mux.HandleFunc("GET /servers/{name}/recoveries/list", s.guard(app.RoleViewer, s.recoveryListFragment))
	mux.HandleFunc("GET /recoveries/{id}/download", s.guard(app.RoleOperator, s.recoveryDownload))
	mux.HandleFunc("POST /recoveries/{id}/delete", s.guard(app.RoleAdmin, s.recoveryDelete))
	mux.HandleFunc("POST /servers/{name}/drill", s.guard(app.RoleOperator, s.serverDrill))
	mux.HandleFunc("GET /drills/{id}/report", s.guard(app.RoleOperator, s.drillReport))
	mux.HandleFunc("GET /runs", s.guard(app.RoleViewer, s.runsPage))
	mux.HandleFunc("GET /runs/{id}", s.guard(app.RoleViewer, s.runPage))
	mux.HandleFunc("GET /runs/{id}/fragment", s.guard(app.RoleViewer, s.runFragment))
	mux.HandleFunc("GET /history", s.guard(app.RoleViewer, s.historyPage))
	mux.HandleFunc("GET /audit", s.guard(app.RoleAdmin, s.auditPage))
	mux.HandleFunc("GET /alerts", s.guard(app.RoleAdmin, s.alertsPage))
	mux.HandleFunc("POST /alerts/test", s.guard(app.RoleAdmin, s.alertsTest))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, nil, http.StatusNotFound, "This page does not exist.")
	})
	return s.secure(s.demoGate(mux))
}

// secure adds the response headers every page needs and recovers from panics
// so that a bug never shows a stack trace.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		if s.app.Demo {
			hd.Set("X-Robots-Tag", "noindex, nofollow")
		}
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			hd.Set("Cache-Control", "no-store")
		}
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				http.Error(w, "Something went wrong on the server. The details were logged.", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// ---- rendering ----

// page is what every full page template receives.
type page struct {
	Title, Active  string
	User, Role     string
	CSRF           string
	CanOperate     bool
	CanAdmin       bool
	Dev            bool
	Demo           bool
	Version        string
	LicenseFooter  string
	Msg, Warn, Err string
	Data           any
}

func (s *Server) newPage(r *http.Request, u *userCtx, title, active string, data any) page {
	p := page{Title: title, Active: active, Dev: s.app.Dev, Demo: s.app.Demo, Version: s.app.Version, Data: data}
	if u != nil {
		p.LicenseFooter = s.app.LicenseInfo().Footer
		p.User, p.Role, p.CSRF = u.Name, u.Role, u.CSRF
		p.CanOperate = s.canOperate(u)
		p.CanAdmin = s.canAdmin(u)
	}
	if r != nil {
		switch r.URL.Query().Get("ok") {
		case "created":
			p.Msg = "Server created and recorded in git."
		case "updated":
			p.Msg = "Changes saved and recorded in git."
		case "allowed":
			p.Msg = "Key authorised: it can push the WAL files of that server, and run nothing else."
		case "revoked":
			p.Msg = "Key removed: that PostgreSQL server can no longer push WAL files."
		case "gone":
			p.Msg = "That key was already gone."
		}
		if r.URL.Query().Get("warn") == "git" {
			p.Warn = "The change was saved but could not be recorded in git. See the pgCellar log."
		}
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	var buf bytes.Buffer
	if err := s.tpl[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		s.app.Logf("render %s: %v", name, err)
		http.Error(w, "The page could not be displayed.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fragment renders a named template without the layout.
func (s *Server) fragment(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl["status"].ExecuteTemplate(&buf, name, data); err != nil {
		s.app.Logf("fragment %s: %v", name, err)
		http.Error(w, "The fragment could not be displayed.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type errorData struct {
	Code    int
	Message string
}

func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// errorPage shows a readable message, never a stack trace. HTMX requests get
// a small box instead of a full page.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, u *userCtx, code int, msg string) {
	if isHX(r) {
		s.fragment(w, code, "result_fragment", map[string]any{"Class": "bad", "Text": msg})
		return
	}
	s.render(w, code, "error", s.newPage(r, u, fmt.Sprint(code), "", errorData{code, msg}))
}

// fail turns a service error into a response.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, u *userCtx, err error) {
	var conflict *app.ConflictError
	var problems *app.ProblemsError
	switch {
	case errors.Is(err, app.ErrNotFound):
		s.errorPage(w, r, u, http.StatusNotFound, "That server or run does not exist.")
	case errors.Is(err, app.ErrNoFile):
		s.errorPage(w, r, u, http.StatusConflict, err.Error())
	case errors.As(err, &conflict):
		s.errorPage(w, r, u, http.StatusConflict, conflict.Msg)
	case errors.As(err, &problems):
		s.errorPage(w, r, u, http.StatusUnprocessableEntity, strings.Join(problems.Problems, " "))
	default:
		s.app.Logf("%s %s: %v", r.Method, r.URL.Path, err)
		s.errorPage(w, r, u, http.StatusBadGateway, "The operation failed: "+err.Error())
	}
}

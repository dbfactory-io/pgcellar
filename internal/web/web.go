// Package web is the HTML presentation of pgCellar: server-rendered pages,
// HTMX fragments, sessions and CSRF. All decisions live in internal/app.
package web

import (
	"bytes"
	"crypto/rand"
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
	secret  []byte
	limiter *limiter
	apiH    http.Handler
}

var pages = []string{"login", "status", "servers", "server_new", "server", "runs", "run", "history", "audit", "error"}

func funcs() template.FuncMap {
	return template.FuncMap{
		"t":         tr,
		"ts":        fmtTime,
		"when":      fmtWhen,
		"span":      fmtSpan,
		"whenstr":   fmtWhenStr,
		"okcount":   okCount,
		"ago":       func(t time.Time) string { return app.HumanDuration(time.Since(t)) },
		"ageSec":    ageSec,
		"size":      humanSize,
		"retention": fmtRetention,
		"runtime":   runtime,
	}
}

// New builds the web server on top of an App.
func New(a *app.App) (*Server, error) {
	s := &Server{app: a, tpl: map[string]*template.Template{}, limiter: newLimiter()}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs()).ParseFS(web.FS, "templates/layout.html", "templates/fragments.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", p, err)
		}
		s.tpl[p] = t
	}
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	s.static = http.StripPrefix("/static/", cacheControl(http.FileServerFS(sub)))
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

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
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
	mux.HandleFunc("GET /runs", s.guard(app.RoleViewer, s.runsPage))
	mux.HandleFunc("GET /runs/{id}", s.guard(app.RoleViewer, s.runPage))
	mux.HandleFunc("GET /runs/{id}/fragment", s.guard(app.RoleViewer, s.runFragment))
	mux.HandleFunc("GET /history", s.guard(app.RoleViewer, s.historyPage))
	mux.HandleFunc("GET /audit", s.guard(app.RoleAdmin, s.auditPage))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, nil, http.StatusNotFound, "This page does not exist.")
	})
	return secure(mux)
}

// secure adds the response headers every page needs and recovers from panics
// so that a bug never shows a stack trace.
func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
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
	Version        string
	Msg, Warn, Err string
	Data           any
}

func (s *Server) newPage(r *http.Request, u *userCtx, title, active string, data any) page {
	p := page{Title: title, Active: active, Dev: s.app.Dev, Version: s.app.Version, Data: data}
	if u != nil {
		p.User, p.Role, p.CSRF = u.Name, u.Role, u.CSRF
		p.CanOperate = app.RoleAtLeast(u.Role, app.RoleOperator)
		p.CanAdmin = app.RoleAtLeast(u.Role, app.RoleAdmin)
	}
	if r != nil {
		switch r.URL.Query().Get("ok") {
		case "created":
			p.Msg = "Server created and recorded in git."
		case "updated":
			p.Msg = "Changes saved and recorded in git."
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

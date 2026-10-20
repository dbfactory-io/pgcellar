// Package api is the JSON presentation of pgCellar, under /api/v1. It is a
// thin layer over internal/app: whatever the web interface can do, this API
// can do, through the same service code.
//
// Authentication: "Authorization: Bearer <key>". Keys are declared in
// pgcellar.yaml (api_keys, SHA-256 only). Every answer is {"data": ...} or
// {"error": "..."}.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/config"
)

// API serves /api/v1.
type API struct{ app *app.App }

// New builds the API on top of an App.
func New(a *app.App) *API { return &API{app: a} }

type principal struct {
	Name string
	Role string
}

func (p principal) actor() string { return "api:" + p.Name }

type handler func(w http.ResponseWriter, r *http.Request, p principal)

// Handler returns the handler to mount on "/api/".
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	const v = "/api/v1"
	mux.HandleFunc("GET "+v+"/openapi.json", a.openapi)
	mux.HandleFunc("GET "+v+"/servers", a.auth(app.RoleViewer, a.listServers))
	mux.HandleFunc("POST "+v+"/servers", a.auth(app.RoleAdmin, a.createServer))
	mux.HandleFunc("GET "+v+"/servers/{name}", a.auth(app.RoleViewer, a.getServer))
	mux.HandleFunc("PUT "+v+"/servers/{name}", a.auth(app.RoleAdmin, a.putServer))
	mux.HandleFunc("POST "+v+"/servers/{name}/check", a.auth(app.RoleOperator, a.check))
	mux.HandleFunc("POST "+v+"/servers/{name}/backup", a.auth(app.RoleOperator, a.backup))
	mux.HandleFunc("POST "+v+"/servers/{name}/switch-wal", a.auth(app.RoleOperator, a.switchWAL))
	mux.HandleFunc("GET "+v+"/servers/{name}/backups", a.auth(app.RoleViewer, a.backups))
	mux.HandleFunc("GET "+v+"/status", a.auth(app.RoleViewer, a.status))
	mux.HandleFunc("GET "+v+"/runs", a.auth(app.RoleViewer, a.runs))
	mux.HandleFunc("GET "+v+"/runs/{id}", a.auth(app.RoleViewer, a.run))
	mux.HandleFunc("GET "+v+"/history", a.auth(app.RoleViewer, a.history))
	mux.HandleFunc("GET "+v+"/audit", a.auth(app.RoleAdmin, a.audit))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint; see /api/v1/openapi.json")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeData(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, map[string]any{"data": v})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail maps a service error to a status code.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *app.ConflictError
	var problems *app.ProblemsError
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, app.ErrNoFile):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, conflict.Msg)
	case errors.As(err, &problems):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": strings.Join(problems.Problems, " "), "problems": problems.Problems})
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "request cancelled")
	default:
		a.app.Logf("api %s %s: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

// auth checks the bearer key and the role.
func (a *API) auth(min string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		key, ok := strings.CutPrefix(hdr, "Bearer ")
		if !ok || strings.TrimSpace(key) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pgcellar"`)
			writeError(w, http.StatusUnauthorized, "missing API key: send 'Authorization: Bearer <key>'")
			return
		}
		k, found := config.MatchAPIKey(a.app.Config(), strings.TrimSpace(key))
		if !found {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pgcellar", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		if !app.RoleAtLeast(k.Role, min) {
			writeError(w, http.StatusForbidden, "this key has the role "+k.Role+"; "+min+" is required")
			return
		}
		h(w, r, principal{Name: k.Name, Role: k.Role})
	}
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

func limitParam(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		return n
	}
	return def
}

func (a *API) listServers(w http.ResponseWriter, r *http.Request, p principal) {
	list, err := a.app.ListServers(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func (a *API) getServer(w http.ResponseWriter, r *http.Request, p principal) {
	d, err := a.app.GetServer(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, d)
}

type writeResponse struct {
	Server *app.ServerDetail `json:"server"`
	Commit string            `json:"commit,omitempty"`
	Warn   string            `json:"warning,omitempty"`
}

func (a *API) createServer(w http.ResponseWriter, r *http.Request, p principal) {
	in := app.ServerInput{BackupMethod: "postgres"}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	res, err := a.app.CreateServer(p.actor(), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, _ := a.app.GetServer(r.Context(), strings.TrimSpace(in.Name))
	writeData(w, http.StatusCreated, writeResponse{Server: d, Commit: res.Commit, Warn: res.Warning})
}

// putServer replaces the given fields; fields left out of the body keep
// their current value.
func (a *API) putServer(w http.ResponseWriter, r *http.Request, p principal) {
	name := r.PathValue("name")
	in, err := a.app.CurrentInput(r.Context(), name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if in.Name != name {
		writeError(w, http.StatusBadRequest, "the name in the body must match the URL (a server cannot be renamed)")
		return
	}
	res, err := a.app.UpdateServer(p.actor(), name, in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, _ := a.app.GetServer(r.Context(), name)
	writeData(w, http.StatusOK, writeResponse{Server: d, Commit: res.Commit, Warn: res.Warning})
}

func (a *API) check(w http.ResponseWriter, r *http.Request, p principal) {
	checks, err := a.app.Check(r.Context(), p.actor(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	ok := len(checks) > 0
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	writeData(w, http.StatusOK, map[string]any{"ok": ok, "checks": checks})
}

func (a *API) backup(w http.ResponseWriter, r *http.Request, p principal) {
	id, err := a.app.StartBackup(r.Context(), p.actor(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+strconv.FormatInt(id, 10))
	writeData(w, http.StatusAccepted, map[string]any{"run_id": id, "status": app.RunRunning, "location": "/api/v1/runs/" + strconv.FormatInt(id, 10)})
}

func (a *API) switchWAL(w http.ResponseWriter, r *http.Request, p principal) {
	out, err := a.app.SwitchWAL(r.Context(), p.actor(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"output": out})
}

func (a *API) backups(w http.ResponseWriter, r *http.Request, p principal) {
	list, err := a.app.Backups(r.Context(), p.actor(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func (a *API) status(w http.ResponseWriter, r *http.Request, p principal) {
	states, at, errMsg := a.app.States()
	out := map[string]any{"servers": states, "updated_at": at}
	if errMsg != "" {
		out["error"] = errMsg
	}
	writeData(w, http.StatusOK, out)
}

func (a *API) runs(w http.ResponseWriter, r *http.Request, p principal) {
	list, err := a.app.Runs(r.URL.Query().Get("server"), limitParam(r, 50))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func (a *API) run(w http.ResponseWriter, r *http.Request, p principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the run id must be a number")
		return
	}
	run, err := a.app.Run(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, run)
}

func (a *API) history(w http.ResponseWriter, r *http.Request, p principal) {
	if h := r.URL.Query().Get("diff"); h != "" {
		d, err := a.app.Diff(r.URL.Query().Get("repo"), h)
		if err != nil {
			writeError(w, http.StatusNotFound, "that commit cannot be shown")
			return
		}
		writeData(w, http.StatusOK, map[string]string{"hash": h, "diff": d})
		return
	}
	list, err := a.app.History(limitParam(r, 50))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func (a *API) audit(w http.ResponseWriter, r *http.Request, p principal) {
	list, err := a.app.Audit(limitParam(r, 100))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

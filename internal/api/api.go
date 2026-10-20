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
	"path/filepath"
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
	mux.HandleFunc("GET "+v+"/servers/{name}/setup", a.auth(app.RoleViewer, a.setup))
	mux.HandleFunc("GET "+v+"/servers/{name}/timeline", a.auth(app.RoleViewer, a.timeline))
	mux.HandleFunc("POST "+v+"/servers/{name}/check", a.auth(app.RoleOperator, a.check))
	mux.HandleFunc("POST "+v+"/servers/{name}/backup", a.auth(app.RoleOperator, a.backup))
	mux.HandleFunc("POST "+v+"/servers/{name}/switch-wal", a.auth(app.RoleOperator, a.switchWAL))
	mux.HandleFunc("GET "+v+"/servers/{name}/backups", a.auth(app.RoleViewer, a.backups))
	mux.HandleFunc("DELETE "+v+"/servers/{name}/backups/{id}", a.auth(app.RoleAdmin, a.deleteBackup))
	mux.HandleFunc("POST "+v+"/servers/{name}/recoveries", a.auth(app.RoleOperator, a.startRecovery))
	mux.HandleFunc("GET "+v+"/servers/{name}/recoveries", a.auth(app.RoleViewer, a.recoveries))
	mux.HandleFunc("GET "+v+"/recoveries/{id}", a.auth(app.RoleViewer, a.recovery))
	mux.HandleFunc("GET "+v+"/recoveries/{id}/download", a.auth(app.RoleOperator, a.downloadRecovery))
	mux.HandleFunc("DELETE "+v+"/recoveries/{id}", a.auth(app.RoleAdmin, a.deleteRecovery))
	mux.HandleFunc("POST "+v+"/servers/{name}/drills", a.auth(app.RoleOperator, a.startDrill))
	mux.HandleFunc("GET "+v+"/servers/{name}/drills", a.auth(app.RoleViewer, a.drills))
	mux.HandleFunc("GET "+v+"/drills", a.auth(app.RoleViewer, a.allDrills))
	mux.HandleFunc("GET "+v+"/drills/{id}", a.auth(app.RoleViewer, a.drill))
	mux.HandleFunc("GET "+v+"/drills/{id}/report", a.auth(app.RoleOperator, a.drillReport))
	mux.HandleFunc("GET "+v+"/status", a.auth(app.RoleViewer, a.status))
	mux.HandleFunc("GET "+v+"/runs", a.auth(app.RoleViewer, a.runs))
	mux.HandleFunc("GET "+v+"/runs/{id}", a.auth(app.RoleViewer, a.run))
	mux.HandleFunc("GET "+v+"/history", a.auth(app.RoleViewer, a.history))
	mux.HandleFunc("GET "+v+"/audit", a.auth(app.RoleAdmin, a.audit))
	mux.HandleFunc("GET "+v+"/alerts", a.auth(app.RoleAdmin, a.alerts))
	mux.HandleFunc("POST "+v+"/alerts/test", a.auth(app.RoleAdmin, a.alertsTest))
	mux.HandleFunc("GET "+v+"/license", a.auth(app.RoleViewer, a.license))
	mux.HandleFunc("GET "+v+"/ssh/authorized-keys", a.auth(app.RoleAdmin, a.authorizedKeys))
	mux.HandleFunc("POST "+v+"/ssh/authorized-keys", a.auth(app.RoleAdmin, a.allowKey))
	mux.HandleFunc("DELETE "+v+"/ssh/authorized-keys", a.auth(app.RoleAdmin, a.revokeKey))
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
	case errors.Is(err, app.ErrRefreshPending):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "first refresh pending: the status cache has no data for this server yet")
	case errors.Is(err, app.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
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

func (a *API) setup(w http.ResponseWriter, r *http.Request, p principal) {
	v, err := a.app.Setup(r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, v)
}

func (a *API) timeline(w http.ResponseWriter, r *http.Request, p principal) {
	t, err := a.app.GetTimeline(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, t)
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

func (a *API) deleteBackup(w http.ResponseWriter, r *http.Request, p principal) {
	if err := a.app.DeleteBackup(r.Context(), p.actor(), r.PathValue("name"), r.PathValue("id")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) startRecovery(w http.ResponseWriter, r *http.Request, p principal) {
	var in app.RecoveryInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	id, err := a.app.StartRecovery(r.Context(), p.actor(), r.PathValue("name"), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	loc := "/api/v1/runs/" + strconv.FormatInt(id, 10)
	w.Header().Set("Location", loc)
	writeData(w, http.StatusAccepted, map[string]any{"run_id": id, "status": app.RunRunning, "location": loc})
}

func (a *API) recoveries(w http.ResponseWriter, r *http.Request, p principal) {
	name := r.PathValue("name")
	if _, err := a.app.GetServer(r.Context(), name); err != nil {
		a.fail(w, r, err)
		return
	}
	list, err := a.app.ListRecoveries(name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func recoveryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the recovery id must be a number")
		return 0, false
	}
	return id, true
}

func (a *API) recovery(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := recoveryID(w, r)
	if !ok {
		return
	}
	v, err := a.app.GetRecovery(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, v)
}

// downloadRecovery serves the .tar.gz with http.ServeFile, so that ranges
// and conditional requests work for large files.
func (a *API) downloadRecovery(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := recoveryID(w, r)
	if !ok {
		return
	}
	path, _, err := a.app.RecoveryArchive(p.actor(), id)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			writeError(w, http.StatusNotFound, "this recovery has no archive to download")
			return
		}
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (a *API) deleteRecovery(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := recoveryID(w, r)
	if !ok {
		return
	}
	if err := a.app.DeleteRecovery(p.actor(), id); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) authorizedKeys(w http.ResponseWriter, r *http.Request, p principal) {
	keys, err := a.app.AuthorizedKeys()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, keys)
}

func (a *API) allowKey(w http.ResponseWriter, r *http.Request, p principal) {
	var in struct {
		Server    string `json:"server"`
		PublicKey string `json:"public_key"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	k, err := a.app.AllowSSHKey(p.actor(), strings.TrimSpace(in.Server), in.PublicKey)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusCreated, k)
}

// revokeKey removes the key whose fingerprint is given in ?fingerprint=SHA256:...
func (a *API) revokeKey(w http.ResponseWriter, r *http.Request, p principal) {
	fp := r.URL.Query().Get("fingerprint")
	if fp == "" {
		writeError(w, http.StatusBadRequest, "give the key to remove: ?fingerprint=SHA256:...")
		return
	}
	if err := a.app.RevokeSSHKey(p.actor(), fp); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- restore drills ----

func (a *API) startDrill(w http.ResponseWriter, r *http.Request, p principal) {
	var in app.DrillInput
	if r.ContentLength != 0 { // the body is optional: an empty POST drills the latest backup
		if err := decode(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	id, err := a.app.StartDrill(r.Context(), p.actor(), r.PathValue("name"), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	loc := "/api/v1/runs/" + strconv.FormatInt(id, 10)
	w.Header().Set("Location", loc)
	writeData(w, http.StatusAccepted, map[string]any{"run_id": id, "status": app.RunRunning, "location": loc})
}

func (a *API) drills(w http.ResponseWriter, r *http.Request, p principal) {
	name := r.PathValue("name")
	if _, err := a.app.GetServer(r.Context(), name); err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeDrills(w, r, name)
}

func (a *API) allDrills(w http.ResponseWriter, r *http.Request, p principal) {
	a.writeDrills(w, r, r.URL.Query().Get("server"))
}

func (a *API) writeDrills(w http.ResponseWriter, r *http.Request, server string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.app.ListDrills(server, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func (a *API) drill(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := recoveryID(w, r)
	if !ok {
		return
	}
	v, err := a.app.GetDrill(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, v)
}

func (a *API) drillReport(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := recoveryID(w, r)
	if !ok {
		return
	}
	path, _, err := a.app.DrillReport(p.actor(), id)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			writeError(w, http.StatusNotFound, "this drill has no report")
			return
		}
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

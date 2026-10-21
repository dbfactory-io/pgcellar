package api

import (
	"net/http"
	"strings"
)

// updates shows what the last release check found and how updates are set up.
func (a *API) updates(w http.ResponseWriter, r *http.Request, p principal) {
	writeData(w, http.StatusOK, a.app.UpdateStatus())
}

// updatesCheck asks for the latest release now. A failed check is still an
// answer: the status carries the error, with 502.
func (a *API) updatesCheck(w http.ResponseWriter, r *http.Request, p principal) {
	if _, err := a.app.CheckUpdates(r.Context(), p.actor()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "data": a.app.UpdateStatus()})
		return
	}
	writeData(w, http.StatusOK, a.app.UpdateStatus())
}

// updatesReady is asked by the updater on the Docker host before it replaces
// the container: 200 {"ready":true} when nothing runs and the clock is inside
// updates.window, else 409 {"ready":false,"reason":"..."}. ?now=1 skips the
// window (an update started by hand). The answer is not wrapped in "data" so
// that a shell script can test it with a plain grep.
func (a *API) updatesReady(w http.ResponseWriter, r *http.Request, p principal) {
	now := strings.TrimSpace(r.URL.Query().Get("now"))
	ok, why := a.app.UpdateReady(a.app.Now(), now == "1" || now == "true")
	window := a.app.Config().Updates.ParsedWindow().String()
	if ok {
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "window": window})
		return
	}
	writeJSON(w, http.StatusConflict, map[string]any{"ready": false, "reason": why, "window": window})
}

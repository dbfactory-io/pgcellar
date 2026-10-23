package api

import (
	"net/http"
	"strconv"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

// vault answers GET /vault: settings (never a secret), lock rule, quota and
// use, the status of every server and the queue.
func (a *API) vault(w http.ResponseWriter, r *http.Request, p principal) {
	writeData(w, http.StatusOK, a.app.VaultStatus())
}

// putVault changes the vault settings; fields left out keep their value. A
// secret_key is written into vault.secret_key_file, never into the YAML.
func (a *API) putVault(w http.ResponseWriter, r *http.Request, p principal) {
	var in app.VaultInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	res, err := a.app.SaveVault(p.actor(), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"vault": a.app.VaultStatus(), "commit": res.Commit, "warning": res.Warning})
}

// vaultTest tests the bucket: with an empty body the saved settings, else the
// settings of the body over the saved ones (not saved).
func (a *API) vaultTest(w http.ResponseWriter, r *http.Request, p principal) {
	var in *app.VaultInput
	if r.ContentLength != 0 {
		in = &app.VaultInput{}
		if err := decode(r, in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	res, err := a.app.TestVault(r.Context(), p.actor(), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if !res.OK {
		status = http.StatusBadGateway
	}
	writeData(w, status, res)
}

// vaultCopy queues a copy of one server: {"server": "pg-main"}.
func (a *API) vaultCopy(w http.ResponseWriter, r *http.Request, p principal) {
	var in struct {
		Server string `json:"server"`
	}
	if err := decode(r, &in); err != nil || in.Server == "" {
		writeError(w, http.StatusBadRequest, `send {"server": "<name>"}`)
		return
	}
	q, err := a.app.VaultCopyNow(r.Context(), p.actor(), in.Server)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusAccepted, q)
}

func (a *API) vaultCopies(w http.ResponseWriter, r *http.Request, p principal) {
	list, err := a.app.ListVaultCopies(r.URL.Query().Get("server"), limitParam(r, 50), false)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, list)
}

func (a *API) vaultCopyOne(w http.ResponseWriter, r *http.Request, p principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the copy id must be a number")
		return
	}
	c, err := a.app.GetVaultCopy(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, c)
}

// vaultKey hands out the recovery key (text/plain), audited.
func (a *API) vaultKey(w http.ResponseWriter, r *http.Request, p principal) {
	name, body, err := a.app.RecoveryKey(p.actor())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(body)
}

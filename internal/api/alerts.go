package api

import (
	"net/http"
	"strings"

	"github.com/stranix79/pgcellar/internal/alerts"
)

func (a *API) alerts(w http.ResponseWriter, r *http.Request, p principal) {
	journal, err := a.app.AlertJournal(limitParam(r, 100))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"channels": a.app.AlertChannels(), "sent": journal})
}

func (a *API) alertsTest(w http.ResponseWriter, r *http.Request, p principal) {
	var in struct {
		Channel string `json:"channel"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	res, err := a.app.SendTestAlert(r.Context(), p.actor(), strings.TrimSpace(in.Channel))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if res == nil {
		res = []alerts.Result{}
	}
	writeData(w, http.StatusOK, res)
}

func (a *API) license(w http.ResponseWriter, r *http.Request, p principal) {
	writeData(w, http.StatusOK, a.app.LicenseInfo())
}

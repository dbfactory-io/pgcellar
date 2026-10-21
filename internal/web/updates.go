package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

// Labels of the Updates page, merged into the catalogue of i18n.go.
var updateMessages = map[string]string{
	"nav.updates":            "Updates",
	"updates.title":          "Updates",
	"updates.intro":          "pgCellar looks for a new release once a day and tells you. It sends nothing but its own version, in the User-Agent of the request.",
	"updates.current":        "Running",
	"updates.latest":         "Latest release",
	"updates.checked":        "Last check",
	"updates.next":           "Next check",
	"updates.published":      "Published",
	"updates.never":          "not checked yet",
	"updates.unknown":        "unknown",
	"updates.dev_build":      "This is a development build: pgCellar cannot compare it with a release, and does not check by itself.",
	"updates.off":            "The automatic check is off (updates.check: false). \"Check now\" still works.",
	"updates.uptodate":       "You run the latest release.",
	"updates.available":      "is available.",
	"updates.notes":          "What changed",
	"updates.error":          "The last check failed:",
	"updates.how":            "How to update",
	"updates.how_docker":     "This pgCellar runs in a container: it is updated from the host. On the Docker host, run:",
	"updates.how_docker_2":   "The updater asks pgCellar first (GET /api/v1/updates/ready) and waits for a moment when no backup, restore or drill runs.",
	"updates.how_self_on":    "Self-update is on: the new release is installed during the update window",
	"updates.how_self_off":   "Self-update is off: install the new release by hand, or set updates.auto: true in pgcellar.yaml.",
	"updates.self":           "Automatic updates",
	"updates.self_on":        "on",
	"updates.self_off":       "off",
	"updates.self_waiting":   "Not now:",
	"updates.self_last":      "Last attempt",
	"updates.self_error":     "The last attempt failed:",
	"updates.self_failed":    "Versions rolled back and never tried again:",
	"updates.window":         "Update window",
	"updates.window_now":     "now inside it",
	"updates.btn_check":      "Check now",
	"updates.new":            "new",
	"updates.checked_ok":     "Checked just now.",
	"updates.banner_link":    "See what changed",
	"updates.banner_version": "pgCellar %s is available.",
}

func init() {
	for k, v := range updateMessages {
		if _, taken := messages[k]; !taken {
			messages[k] = v
		}
	}
}

// updateBanner is the discreet line on the Status page; empty when there is
// nothing new.
func (s *Server) updateBanner() string {
	if s.app.Demo {
		return ""
	}
	st := s.app.Updates().Latest()
	if !st.Available {
		return ""
	}
	return fmt.Sprintf(tr("updates.banner_version"), st.Latest)
}

type updatesData struct {
	app.UpdateView
	CSRF    string
	CanOp   bool
	Checked bool
}

func (s *Server) updatesPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	d := updatesData{UpdateView: s.app.UpdateStatus(), CSRF: u.CSRF, CanOp: s.canOperate(u), Checked: r.URL.Query().Get("ok") == "checked"}
	p := s.newPage(r, u, tr("updates.title"), "updates", d)
	if d.Checked && d.Error == "" {
		p.Msg = tr("updates.checked_ok")
	}
	s.render(w, http.StatusOK, "updates", p)
}

// updatesCheck runs a check now and goes back to the page, which shows the
// result (or the error of the check).
func (s *Server) updatesCheck(w http.ResponseWriter, r *http.Request, u *userCtx) {
	_, _ = s.app.CheckUpdates(r.Context(), u.Name)
	if isHX(r) {
		w.Header().Set("HX-Redirect", "/updates?ok=checked")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/updates?ok=checked", http.StatusSeeOther)
}

// updateMetrics adds pgcellar_update_available to /metrics.
func (s *Server) updateMetrics(b *strings.Builder) {
	v := 0
	if s.app.Updates().Latest().Available {
		v = 1
	}
	_, _ = fmt.Fprintf(b, "# HELP pgcellar_update_available 1 when a newer pgCellar release was found by the last check, else 0.\n# TYPE pgcellar_update_available gauge\npgcellar_update_available %d\n", v)
}

package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/stranix79/pgcellar/internal/app"
)

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, _, err := s.app.Store.GetSetting("csrf_secret"); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "database unavailable")
		return
	}
	fmt.Fprintln(w, "ok")
}

func promEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// metrics prints the Prometheus text format from the cached statuses: a
// scrape never calls Barman.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	states, _, _ := s.app.States()
	var b strings.Builder
	family := func(name, help string, value func(app.ServerState) (string, bool)) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, st := range states {
			if v, ok := value(st); ok {
				fmt.Fprintf(&b, "%s{server=\"%s\"} %s\n", name, promEscape(st.Name), v)
			}
		}
	}
	family("pgcellar_last_backup_age_seconds", "Seconds since the last successful backup ended.", func(st app.ServerState) (string, bool) {
		if st.LastBackupAgeSeconds == nil {
			return "", false
		}
		return fmt.Sprint(*st.LastBackupAgeSeconds), true
	})
	family("pgcellar_last_wal_age_seconds", "Seconds since the last archived WAL file.", func(st app.ServerState) (string, bool) {
		if st.LastWALAgeSeconds == nil {
			return "", false
		}
		return fmt.Sprint(*st.LastWALAgeSeconds), true
	})
	family("pgcellar_backups_total", "Number of backups Barman holds for the server.", func(st app.ServerState) (string, bool) {
		return fmt.Sprint(st.BackupCount), true
	})
	family("pgcellar_check_ok", "1 when every barman check passes, else 0.", func(st app.ServerState) (string, bool) {
		if st.CheckOK {
			return "1", true
		}
		return "0", true
	})
	fmt.Fprintf(&b, "# HELP pgcellar_build_info Version of the running pgCellar.\n# TYPE pgcellar_build_info gauge\npgcellar_build_info{version=\"%s\"} 1\n", promEscape(s.app.Version))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

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
		_, _ = fmt.Fprintln(w, "database unavailable")
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
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
		_, _ = fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, st := range states {
			if v, ok := value(st); ok {
				_, _ = fmt.Fprintf(&b, "%s{server=\"%s\"} %s\n", name, promEscape(st.Name), v)
			}
		}
	}
	family("pgcellar_last_backup_age_seconds", "Seconds since the last successful backup ended.", func(st app.ServerState) (string, bool) {
		if st.LastBackupAgeSeconds == nil {
			return "", false
		}
		return fmt.Sprint(*st.LastBackupAgeSeconds), true
	})
	family("pgcellar_last_wal_received_age_seconds", "Seconds since data last reached Barman (newest file in the streaming directory).", func(st app.ServerState) (string, bool) {
		if st.LastDataReceivedAgeSeconds == nil {
			return "", false
		}
		return fmt.Sprint(*st.LastDataReceivedAgeSeconds), true
	})
	family("pgcellar_wal_streaming_live", "1 when the Barman WAL receiver is running, else 0.", func(st app.ServerState) (string, bool) {
		if st.StreamingLive {
			return "1", true
		}
		return "0", true
	})
	family("pgcellar_last_wal_age_seconds", "Seconds since the last filled WAL file.", func(st app.ServerState) (string, bool) {
		if st.LastFilledWALAgeSeconds == nil {
			return "", false
		}
		return fmt.Sprint(*st.LastFilledWALAgeSeconds), true
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
	family("pgcellar_setup_level", "Server setup: 0 ok, 1 warning, 2 error. Absent until the PostgreSQL settings were read.", func(st app.ServerState) (string, bool) {
		switch st.SetupLevel {
		case "ok":
			return "0", true
		case "warning":
			return "1", true
		case "error":
			return "2", true
		}
		return "", false
	})
	family("pgcellar_last_drill_ok", "1 when the last restore drill of the server passed, 0 when it failed. Absent until a drill ran.", func(st app.ServerState) (string, bool) {
		if st.Drill == nil || st.Drill.Last == nil {
			return "", false
		}
		if st.Drill.Last.OK {
			return "1", true
		}
		return "0", true
	})
	family("pgcellar_last_drill_age_seconds", "Seconds since the last restore drill of the server ended, whatever its result. Absent until a drill ran.", func(st app.ServerState) (string, bool) {
		if st.Drill == nil || st.Drill.Last == nil {
			return "", false
		}
		return fmt.Sprint(st.Drill.Last.AgeSeconds), true
	})
	writeStorageMetrics(&b, states)
	_, _ = b.WriteString("# HELP pgcellar_recoveries_total Restores to a directory that finished successfully, since the database was created.\n# TYPE pgcellar_recoveries_total counter\n")
	done, _ := s.app.Store.CountRuns("recover")
	for _, st := range states {
		_, _ = fmt.Fprintf(&b, "pgcellar_recoveries_total{server=\"%s\"} %d\n", promEscape(st.Name), done[st.Name])
	}
	_, _ = fmt.Fprintf(&b, "# HELP pgcellar_build_info Version of the running pgCellar.\n# TYPE pgcellar_build_info gauge\npgcellar_build_info{version=\"%s\"} 1\n", promEscape(s.app.Version))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

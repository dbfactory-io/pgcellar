package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

// alertsData feeds the Alerts page.
type alertsData struct {
	Channels       []app.AlertChannelInfo
	Journal        []app.AlertSentView
	RepeatEvery    string
	OnDrillSuccess bool
	DailySummary   string
	BaseURLSet     bool
}

func (s *Server) alertsData() (alertsData, error) {
	j, err := s.app.AlertJournal(100)
	if err != nil {
		return alertsData{}, err
	}
	cfg := s.app.Config()
	d := alertsData{Channels: s.app.AlertChannels(), Journal: j, OnDrillSuccess: cfg.Alerts.OnDrillSuccess,
		DailySummary: strings.TrimSpace(cfg.Alerts.DailySummary), BaseURLSet: strings.TrimSpace(cfg.BaseURL) != ""}
	if cfg.Alerts.EffectiveRepeatEvery() > 0 {
		d.RepeatEvery = app.HumanDuration(cfg.Alerts.EffectiveRepeatEvery())
	}
	return d, nil
}

func (s *Server) alertsPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	d, err := s.alertsData()
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "alerts", s.newPage(r, u, tr("nav.alerts"), "alerts", d))
}

// alertsTest sends a test message to one channel and shows the outcome.
func (s *Server) alertsTest(w http.ResponseWriter, r *http.Request, u *userCtx) {
	channel := strings.TrimSpace(r.FormValue("channel"))
	res, err := s.app.SendTestAlert(r.Context(), u.Name, channel)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	out := map[string]any{"Class": "ok", "Text": "Test message sent."}
	var bad []string
	var good []string
	for _, x := range res {
		if x.OK {
			good = append(good, x.Channel)
		} else {
			bad = append(bad, x.Channel+": "+x.Error)
		}
	}
	switch {
	case len(bad) > 0:
		out = map[string]any{"Class": "bad", "Text": fmt.Sprintf("The test message could not be sent (%d of %d channels failed).", len(bad), len(res)), "Output": strings.Join(bad, "\n")}
	case len(good) > 0:
		out["Text"] = "Test message sent through " + strings.Join(good, ", ") + ". Look for it in that channel."
	}
	if !isHX(r) {
		http.Redirect(w, r, "/alerts", http.StatusSeeOther)
		return
	}
	s.fragment(w, http.StatusOK, "result_fragment", out)
}

func (s *Server) alertCounts(b *strings.Builder) {
	_, _ = b.WriteString("# HELP pgcellar_alerts_sent_total Messages sent to an alert channel since the database was created, by channel and outcome.\n# TYPE pgcellar_alerts_sent_total counter\n")
	seen := map[string]bool{}
	for _, c := range s.app.AlertCounts() {
		seen[fmt.Sprintf("%s/%t", c.Channel, c.OK)] = true
		_, _ = fmt.Fprintf(b, "pgcellar_alerts_sent_total{channel=\"%s\",ok=\"%t\"} %d\n", promEscape(c.Channel), c.OK, c.N)
	}
	for _, c := range s.app.AlertChannels() { // a configured channel shows 0 until it sends
		for _, ok := range []bool{true, false} {
			if !seen[fmt.Sprintf("%s/%t", c.Name, ok)] {
				_, _ = fmt.Fprintf(b, "pgcellar_alerts_sent_total{channel=\"%s\",ok=\"%t\"} 0\n", promEscape(c.Name), ok)
			}
		}
	}
}

func (s *Server) licenseMetrics(b *strings.Builder) {
	li := s.app.LicenseInfo()
	_, _ = fmt.Fprintf(b, "# HELP pgcellar_license_servers_max Servers the licence in force allows (0 means unlimited).\n# TYPE pgcellar_license_servers_max gauge\npgcellar_license_servers_max %d\n", li.Max)
	_, _ = fmt.Fprintf(b, "# HELP pgcellar_license_servers_used Servers pgCellar holds.\n# TYPE pgcellar_license_servers_used gauge\npgcellar_license_servers_used %d\n", li.Used)
}

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/config"
)

// ---- status ----

type statusData struct {
	States  []app.ServerState
	Updated time.Time
	Err     string
	Dev     bool // the app clock is frozen for the fixtures
}

func (s *Server) statusData() statusData {
	states, at, errMsg := s.app.States()
	return statusData{States: states, Updated: at, Err: errMsg, Dev: s.app.Dev}
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	s.render(w, http.StatusOK, "status", s.newPage(r, u, tr("status.title"), "status", s.statusData()))
}

func (s *Server) statusTable(w http.ResponseWriter, r *http.Request, u *userCtx) {
	s.fragment(w, http.StatusOK, "status_table", s.statusData())
}

// ---- servers ----

func (s *Server) serversPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	list, err := s.app.ListServers(r.Context())
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "servers", s.newPage(r, u, tr("servers.title"), "servers", list))
}

// formData feeds the "server_form" template.
type formData struct {
	In       app.ServerInput
	Problems []string
	Action   string
	CSRF     string
	New      bool
	ReadOnly bool
}

func (s *Server) serverNewForm(w http.ResponseWriter, r *http.Request, u *userCtx) {
	in := app.ServerInput{BackupMethod: "postgres", Schedule: config.Schedule{Backup: "daily 02:00", Enabled: true}, Retention: config.Retention{RecoveryWindowDays: 14, MinimumRedundancy: 2}}
	s.render(w, http.StatusOK, "server_new", s.newPage(r, u, tr("servers.new"), "servers", formData{In: in, Action: "/servers", CSRF: u.CSRF, New: true}))
}

func atoi(r *http.Request, field string, problems *[]string) int {
	v := strings.TrimSpace(r.PostForm.Get(field))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%q is not a whole number.", v))
	}
	return n
}

func parseServerForm(r *http.Request) (app.ServerInput, []string) {
	var problems []string
	if err := r.ParseForm(); err != nil {
		return app.ServerInput{}, []string{"The form could not be read."}
	}
	f := r.PostForm.Get
	in := app.ServerInput{
		Name: f("name"), Label: f("label"), Description: f("description"),
		BackupMethod: f("backup_method"), ConnInfo: f("conninfo"), StreamingConnInfo: f("streaming_conninfo"),
		Slot: f("slot_name"), SSHCommand: f("ssh_command"),
		Schedule: config.Schedule{Backup: f("schedule_backup"), Enabled: f("schedule_enabled") != ""},
	}
	in.Retention.RecoveryWindowDays = atoi(r, "recovery_window_days", &problems)
	in.Retention.Redundancy = atoi(r, "redundancy", &problems)
	in.Retention.MinimumRedundancy = atoi(r, "minimum_redundancy", &problems)
	return in, problems
}

func redirectAfterWrite(w http.ResponseWriter, r *http.Request, name, ok string, res *app.WriteResult) {
	target := "/servers/" + name + "?ok=" + ok
	if res != nil && res.Warning != "" {
		target += "&warn=git"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) serverCreate(w http.ResponseWriter, r *http.Request, u *userCtx) {
	in, problems := parseServerForm(r)
	if len(problems) == 0 {
		res, err := s.app.CreateServer(u.Name, in)
		if err == nil {
			redirectAfterWrite(w, r, in.Name, "created", res)
			return
		}
		var pe *app.ProblemsError
		var ce *app.ConflictError
		switch {
		case errors.As(err, &pe):
			problems = pe.Problems
		case errors.As(err, &ce):
			problems = []string{ce.Msg}
		default:
			s.fail(w, r, u, err)
			return
		}
	}
	s.render(w, http.StatusUnprocessableEntity, "server_new", s.newPage(r, u, tr("servers.new"), "servers", formData{In: in, Problems: problems, Action: "/servers", CSRF: u.CSRF, New: true}))
}

type serverPageData struct {
	Detail     app.ServerDetail
	Form       formData
	Backups    []app.BackupView
	BackupsErr string
	Runs       []app.RunView
	CanOperate bool
	CanAdmin   bool
}

func (s *Server) serverPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	s.renderServer(w, r, u, http.StatusOK, nil)
}

// renderServer shows the detail page; override carries a form the user just
// submitted with problems.
func (s *Server) renderServer(w http.ResponseWriter, r *http.Request, u *userCtx, status int, override *formData) {
	name := r.PathValue("name")
	d, err := s.app.GetServer(r.Context(), name)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	data := serverPageData{
		Detail:     *d,
		CanOperate: app.RoleAtLeast(u.Role, app.RoleOperator),
		CanAdmin:   app.RoleAtLeast(u.Role, app.RoleAdmin),
		Form:       formData{In: d.ServerInput, Action: "/servers/" + name, CSRF: u.CSRF, ReadOnly: !app.RoleAtLeast(u.Role, app.RoleAdmin)},
	}
	if override != nil {
		data.Form = *override
	}
	if d.InBarman || d.HasFile {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		backups, err := s.app.Backups(ctx, u.Name, name)
		cancel()
		if err != nil {
			data.BackupsErr = "Barman could not list the backups: " + err.Error()
		} else {
			data.Backups = backups
		}
	} else {
		data.BackupsErr = "Barman does not know this server, so it has no backups to list."
	}
	data.Runs, _ = s.app.Runs(name, 10)
	s.render(w, status, "server", s.newPage(r, u, name, "servers", data))
}

func (s *Server) serverUpdate(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	in, problems := parseServerForm(r)
	in.Name = name
	if len(problems) == 0 {
		res, err := s.app.UpdateServer(u.Name, name, in)
		if err == nil {
			redirectAfterWrite(w, r, name, "updated", res)
			return
		}
		var pe *app.ProblemsError
		if !errors.As(err, &pe) {
			s.fail(w, r, u, err)
			return
		}
		problems = pe.Problems
	}
	s.renderServer(w, r, u, http.StatusUnprocessableEntity, &formData{In: in, Problems: problems, Action: "/servers/" + name, CSRF: u.CSRF})
}

type checkData struct {
	Checks []app.CheckView
	Err    string
}

func (s *Server) serverCheck(w http.ResponseWriter, r *http.Request, u *userCtx) {
	checks, err := s.app.Check(r.Context(), u.Name, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			s.fail(w, r, u, err)
			return
		}
		s.fragment(w, http.StatusOK, "check_fragment", checkData{Err: "Barman could not run the check: " + err.Error()})
		return
	}
	s.fragment(w, http.StatusOK, "check_fragment", checkData{Checks: checks})
}

type runData struct{ Run app.RunView }

func (s *Server) serverBackup(w http.ResponseWriter, r *http.Request, u *userCtx) {
	id, err := s.app.StartBackup(r.Context(), u.Name, r.PathValue("name"))
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	if !isHX(r) {
		http.Redirect(w, r, "/runs/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	run, err := s.app.Run(id)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.fragment(w, http.StatusOK, "run_fragment", runData{*run})
}

func (s *Server) serverSwitchWAL(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	out, err := s.app.SwitchWAL(r.Context(), u.Name, name)
	res := map[string]any{"Class": "ok", "Text": "WAL switched on " + name + ": the current segment was closed and archived.", "Output": out}
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			s.fail(w, r, u, err)
			return
		}
		res = map[string]any{"Class": "bad", "Text": "Barman could not switch the WAL: " + err.Error(), "Output": out}
	}
	if !isHX(r) {
		http.Redirect(w, r, "/servers/"+name, http.StatusSeeOther)
		return
	}
	s.fragment(w, http.StatusOK, "result_fragment", res)
}

// ---- runs, history, audit ----

func (s *Server) runsPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	runs, err := s.app.Runs("", 100)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "runs", s.newPage(r, u, tr("runs.title"), "runs", runs))
}

func (s *Server) loadRun(r *http.Request) (*app.RunView, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, app.ErrNotFound
	}
	return s.app.Run(id)
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	run, err := s.loadRun(r)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "run", s.newPage(r, u, fmt.Sprintf("Run #%d", run.ID), "runs", runData{*run}))
}

func (s *Server) runFragment(w http.ResponseWriter, r *http.Request, u *userCtx) {
	run, err := s.loadRun(r)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.fragment(w, http.StatusOK, "run_fragment", runData{*run})
}

type historyData struct {
	Entries  []app.HistoryItem
	Diff     string
	DiffHash string
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	entries, err := s.app.History(100)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	data := historyData{Entries: entries}
	if h := r.URL.Query().Get("diff"); h != "" {
		diff, err := s.app.Diff(r.URL.Query().Get("repo"), h)
		if err != nil {
			s.fail(w, r, u, errors.New("that commit cannot be shown"))
			return
		}
		data.Diff, data.DiffHash = diff, h
	}
	s.render(w, http.StatusOK, "history", s.newPage(r, u, tr("history.title"), "history", data))
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	list, err := s.app.Audit(300)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "audit", s.newPage(r, u, tr("audit.title"), "audit", list))
}

package web

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/sshkeys"
)

// ---- status ----

type statusData struct {
	States  []app.ServerState
	Updated time.Time
	Err     string
	Dev     bool   // the app clock is frozen for the fixtures
	Every   string // refresh interval, for people
	Stale   bool   // states come from before the restart; the first refresh is running
	Summary summary
	Storage []storageLine // one line per file system holding backups
	License string        // discreet banner: over the quota, or expired
}

func (s *Server) statusData() statusData {
	states, at, errMsg := s.app.States()
	stale := s.app.CacheStale()
	return statusData{States: states, Updated: at, Err: errMsg, Dev: s.app.Dev, Every: everyText(s.app.StatusInterval), Stale: stale, Summary: summarize(states, stale), Storage: storageLines(states), License: s.app.LicenseInfo().Banner}
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
	in := app.ServerInput{BackupMethod: "postgres", WALMethod: app.WALStreaming, Schedule: config.Schedule{Backup: "daily 02:00", Enabled: true}, Retention: config.Retention{RecoveryWindowDays: 14, MinimumRedundancy: 2}}
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
		Slot: f("slot_name"), SSHCommand: f("ssh_command"), WALMethod: f("wal_method"),
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
	BackupList backupListData
	Recoveries recoveryListData
	Drills     drillListData
	Timeline   tlView
	Updated    time.Time // when the cache entry of the server was built
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
	if tl, err := s.app.GetTimeline(r.Context(), name); err == nil {
		data.Timeline = buildTimelineView(tl)
	} else {
		data.Timeline = tlView{Unavailable: "unavailable: " + err.Error()}
	}
	data.BackupList = s.backupList(r, u, name)
	data.Recoveries = s.recoveryList(u, name)
	data.Drills = s.drillList(u, name)
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

// backupListData feeds the "backup_list" fragment: the backups of a server,
// with at most one row asking for a confirmation, and the outcome of the last
// delete.
type backupListData struct {
	Server   string
	Backups  []app.BackupView
	Err      string
	CanAdmin bool
	Confirm  string // id of the backup whose deletion is being confirmed
	Flash    string
	FlashBad bool
}

func (s *Server) backupList(r *http.Request, u *userCtx, name string) backupListData {
	d := backupListData{Server: name, CanAdmin: app.RoleAtLeast(u.Role, app.RoleAdmin)}
	switch backups, err := s.app.Backups(r.Context(), u.Name, name); {
	case errors.Is(err, app.ErrRefreshPending):
		d.Err = "First refresh pending: the backup list appears after pgCellar has read Barman once."
	case err != nil:
		d.Err = "The backup list is not available: " + err.Error()
	default:
		d.Backups = backups
	}
	return d
}

func (s *Server) backupListFragment(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	d := s.backupList(r, u, name)
	if id := r.URL.Query().Get("confirm"); id != "" && d.CanAdmin {
		for _, b := range d.Backups {
			if b.ID == id {
				d.Confirm = id
			}
		}
	}
	s.fragment(w, http.StatusOK, "backup_list", d)
}

func (s *Server) serverBackupDelete(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name, id := r.PathValue("name"), r.PathValue("id")
	err := s.app.DeleteBackup(r.Context(), u.Name, name, id)
	if !isHX(r) {
		if err != nil {
			s.fail(w, r, u, err)
			return
		}
		http.Redirect(w, r, "/servers/"+name, http.StatusSeeOther)
		return
	}
	d := s.backupList(r, u, name)
	switch {
	case err == nil:
		d.Flash = "Backup " + id + " deleted."
	case errors.Is(err, app.ErrNotFound):
		d.Flash, d.FlashBad = "Backup "+id+" does not exist (any more).", true
	default:
		var ce *app.ConflictError
		var pe *app.ProblemsError
		switch {
		case errors.As(err, &ce):
			d.Flash = "Barman did not delete backup " + id + ": " + ce.Msg
		case errors.As(err, &pe):
			d.Flash = strings.Join(pe.Problems, " ")
		default:
			s.app.Logf("delete backup %s/%s: %v", name, id, err)
			d.Flash = "The delete failed: " + err.Error()
		}
		d.FlashBad = true
	}
	s.fragment(w, http.StatusOK, "backup_list", d)
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

type runData struct {
	Run        app.RunView
	Recovery   *app.RecoveryView // the restore a finished recover run produced
	Drill      *app.DrillView    // the drill a finished drill run produced
	CanOperate bool
	CanAdmin   bool
}

func (s *Server) newRunData(run *app.RunView, u *userCtx) runData {
	d := runData{Run: *run}
	if u != nil {
		d.CanOperate = app.RoleAtLeast(u.Role, app.RoleOperator)
		d.CanAdmin = app.RoleAtLeast(u.Role, app.RoleAdmin)
	}
	if run.Kind == "recover" && run.Status != app.RunRunning {
		if rec, ok := s.app.RecoveryOfRun(run.ID); ok {
			d.Recovery = rec
		}
	}
	if run.Kind == "drill" && run.Status != app.RunRunning {
		if dr, ok := s.app.DrillOfRun(run.ID); ok {
			d.Drill = dr
		}
	}
	return d
}

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
	s.fragment(w, http.StatusOK, "run_fragment", s.newRunData(run, u))
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
	s.render(w, http.StatusOK, "run", s.newPage(r, u, fmt.Sprintf("Run #%d", run.ID), "runs", s.newRunData(run, u)))
}

func (s *Server) runFragment(w http.ResponseWriter, r *http.Request, u *userCtx) {
	run, err := s.loadRun(r)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.fragment(w, http.StatusOK, "run_fragment", s.newRunData(run, u))
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

// everyText renders a refresh interval for the status footer: "minute",
// "5 minutes", "45 seconds".
func everyText(d time.Duration) string {
	if d <= 0 {
		d = time.Minute
	}
	switch {
	case d == time.Minute:
		return "minute"
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
	return d.String()
}

// ---- restore (point-in-time recovery to a directory) ----

type restoreData struct {
	Server   string
	Backups  []app.BackupView // complete backups only, newest first
	Err      string
	Dir      string // recoveries.dir
	Zone     string // time zone a datetime without zone is read in
	MaxDL    int64
	KeepDays int
	Problems []string
	// What the user typed, kept when the form comes back with problems.
	BackupID, Target, TargetTime string
	Archive                      bool
}

func (s *Server) restoreData(r *http.Request, u *userCtx, name string) (restoreData, error) {
	if _, err := s.app.GetServer(r.Context(), name); err != nil {
		return restoreData{}, err
	}
	cfg := s.app.Config().Recoveries
	d := restoreData{Server: name, Dir: cfg.Dir, MaxDL: cfg.MaxDownloadBytes, KeepDays: cfg.KeepDays, Target: app.TargetLatest}
	d.Zone = time.Now().Format("MST (UTC-07:00)")
	switch backups, err := s.app.Backups(r.Context(), u.Name, name); {
	case errors.Is(err, app.ErrRefreshPending):
		d.Err = "First refresh pending: the backup list appears after pgCellar has read Barman once."
	case err != nil:
		d.Err = "The backup list is not available: " + err.Error()
	default:
		for _, b := range backups {
			if b.Status == "DONE" {
				d.Backups = append(d.Backups, b)
			}
		}
	}
	if len(d.Backups) > 0 {
		d.BackupID = d.Backups[0].ID
		if want := r.URL.Query().Get("backup"); want != "" {
			for _, b := range d.Backups {
				if b.ID == want {
					d.BackupID = want
				}
			}
		}
	}
	return d, nil
}

func (s *Server) restoreForm(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	d, err := s.restoreData(r, u, name)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "restore", s.newPage(r, u, tr("restore.title")+" "+name, "servers", d))
}

func (s *Server) restoreSubmit(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, u, http.StatusBadRequest, "The form could not be read.")
		return
	}
	f := r.PostForm.Get
	in := app.RecoveryInput{BackupID: f("backup_id"), Target: f("target"), TargetTime: f("target_time"), Archive: f("archive") != ""}
	id, err := s.app.StartRecovery(r.Context(), u.Name, name, in)
	if err == nil {
		http.Redirect(w, r, "/runs/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	var pe *app.ProblemsError
	var ce *app.ConflictError
	if !errors.As(err, &pe) && !errors.As(err, &ce) {
		s.fail(w, r, u, err)
		return
	}
	d, derr := s.restoreData(r, u, name)
	if derr != nil {
		s.fail(w, r, u, derr)
		return
	}
	if pe != nil {
		d.Problems = pe.Problems
	} else {
		d.Problems = []string{ce.Msg}
	}
	d.BackupID, d.Target, d.TargetTime, d.Archive = in.BackupID, in.Target, in.TargetTime, in.Archive
	s.render(w, http.StatusUnprocessableEntity, "restore", s.newPage(r, u, tr("restore.title")+" "+name, "servers", d))
}

// ---- recoveries of a server ----

// recoveryListData feeds the "recovery_list" fragment.
type recoveryListData struct {
	Server     string
	Recoveries []app.RecoveryView
	CanOperate bool
	CanAdmin   bool
	Confirm    int64 // id of the restore whose deletion is being confirmed
	Flash      string
	FlashBad   bool
}

func (s *Server) recoveryList(u *userCtx, name string) recoveryListData {
	d := recoveryListData{Server: name, CanOperate: app.RoleAtLeast(u.Role, app.RoleOperator), CanAdmin: app.RoleAtLeast(u.Role, app.RoleAdmin)}
	d.Recoveries, _ = s.app.ListRecoveries(name)
	return d
}

func (s *Server) recoveryListFragment(w http.ResponseWriter, r *http.Request, u *userCtx) {
	d := s.recoveryList(u, r.PathValue("name"))
	if id, err := strconv.ParseInt(r.URL.Query().Get("confirm"), 10, 64); err == nil && d.CanAdmin {
		for _, rec := range d.Recoveries {
			if rec.ID == id {
				d.Confirm = id
			}
		}
	}
	s.fragment(w, http.StatusOK, "recovery_list", d)
}

func (s *Server) recoveryDelete(w http.ResponseWriter, r *http.Request, u *userCtx) {
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		s.fail(w, r, u, app.ErrNotFound)
		return
	}
	rec, gerr := s.app.GetRecovery(id)
	if gerr != nil {
		s.fail(w, r, u, gerr)
		return
	}
	err := s.app.DeleteRecovery(u.Name, id)
	if !isHX(r) {
		if err != nil {
			s.fail(w, r, u, err)
			return
		}
		http.Redirect(w, r, "/servers/"+rec.Server, http.StatusSeeOther)
		return
	}
	d := s.recoveryList(u, rec.Server)
	if err != nil {
		s.app.Logf("delete recovery %d: %v", id, err)
		d.Flash, d.FlashBad = "The restore was not deleted: "+err.Error(), true
	} else {
		d.Flash = "Restore " + filepath.Base(rec.Path) + " deleted."
	}
	s.fragment(w, http.StatusOK, "recovery_list", d)
}

func (s *Server) recoveryDownload(w http.ResponseWriter, r *http.Request, u *userCtx) {
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		s.fail(w, r, u, app.ErrNotFound)
		return
	}
	path, _, err := s.app.RecoveryArchive(u.Name, id)
	switch {
	case errors.Is(err, app.ErrNotFound):
		s.errorPage(w, r, u, http.StatusNotFound, "This restore has no archive to download.")
		return
	case errors.Is(err, app.ErrTooLarge):
		s.errorPage(w, r, u, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		s.fail(w, r, u, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

// ---- SSH key ----

type sshServerRow struct {
	Name       string
	SSHCommand string
	WALMethod  string
	Archive    bool   // WAL is shipped by archive_command
	Command    string // the archive_command to set on that server
}

type sshKeyData struct {
	Key     app.SSHKeyView
	Servers []sshServerRow
	Allowed []sshkeys.AuthorizedKey
	// Confirm is the fingerprint whose removal waits for a click on "Yes, remove".
	Confirm string
	// The allow form, kept when it is sent back with a problem.
	FormServer, FormKey string
	AllowCommand        string // an example forced command, for the help text
}

func (s *Server) sshKeyData(r *http.Request, u *userCtx, formServer, formKey string) (sshKeyData, error) {
	key, err := s.app.SSHKey(u.Name)
	if err != nil {
		return sshKeyData{}, err
	}
	data := sshKeyData{Key: key, FormServer: formServer, FormKey: formKey, AllowCommand: sshkeys.ForcedCommand("<server>")}
	if list, err := s.app.ListServers(r.Context()); err == nil {
		for _, d := range list {
			if !d.HasFile {
				continue
			}
			row := sshServerRow{Name: d.Name, SSHCommand: d.SSHCommand, WALMethod: d.WALMethod}
			row.Archive = d.WALMethod == app.WALArchiveCommand || d.WALMethod == app.WALBoth
			row.Command = barman.ArchiveCommand(key.Host, key.Port, d.Name)
			data.Servers = append(data.Servers, row)
		}
	}
	data.Allowed, err = s.app.AuthorizedKeys()
	if err != nil {
		s.app.Logf("read authorized_keys: %v", err)
	}
	fp := r.URL.Query().Get("confirm")
	for _, k := range data.Allowed {
		if fp != "" && k.Fingerprint == fp {
			data.Confirm = fp
		}
	}
	return data, nil
}

func (s *Server) sshKeyPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	data, err := s.sshKeyData(r, u, "", "")
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	s.render(w, http.StatusOK, "ssh_key", s.newPage(r, u, tr("nav.ssh_key"), "ssh-key", data))
}

func (s *Server) sshKeyAllow(w http.ResponseWriter, r *http.Request, u *userCtx) {
	server, pub := strings.TrimSpace(r.FormValue("server")), r.FormValue("public_key")
	_, err := s.app.AllowSSHKey(u.Name, server, pub)
	if err == nil {
		http.Redirect(w, r, "/ssh-key?ok=allowed", http.StatusSeeOther)
		return
	}
	var pe *app.ProblemsError
	var ce *app.ConflictError
	msg := ""
	status := http.StatusUnprocessableEntity
	switch {
	case errors.As(err, &pe):
		msg = strings.Join(pe.Problems, " ")
	case errors.As(err, &ce):
		msg, status = ce.Msg, http.StatusConflict
	case errors.Is(err, app.ErrNotFound):
		msg = "Choose one of the servers of the list."
	default:
		s.fail(w, r, u, err)
		return
	}
	data, derr := s.sshKeyData(r, u, server, pub)
	if derr != nil {
		s.fail(w, r, u, derr)
		return
	}
	p := s.newPage(r, u, tr("nav.ssh_key"), "ssh-key", data)
	p.Err = msg
	s.render(w, status, "ssh_key", p)
}

func (s *Server) sshKeyRevoke(w http.ResponseWriter, r *http.Request, u *userCtx) {
	err := s.app.RevokeSSHKey(u.Name, r.FormValue("fingerprint"))
	switch {
	case err == nil:
		http.Redirect(w, r, "/ssh-key?ok=revoked", http.StatusSeeOther)
	case errors.Is(err, app.ErrNotFound):
		http.Redirect(w, r, "/ssh-key?ok=gone", http.StatusSeeOther)
	default:
		s.fail(w, r, u, err)
	}
}

func (s *Server) serverSSHTest(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	res, err := s.app.TestSSH(r.Context(), u.Name, name)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	out := map[string]any{"Class": "ok", "Text": "SSH connection to the PostgreSQL server of " + name + ": OK."}
	if !res.OK {
		out = map[string]any{"Class": "bad", "Text": "SSH connection to the PostgreSQL server of " + name + " failed.", "Output": res.Message}
	}
	if !isHX(r) {
		http.Redirect(w, r, "/servers/"+name, http.StatusSeeOther)
		return
	}
	s.fragment(w, http.StatusOK, "result_fragment", out)
}

// ---- restore drills ----

// drillListData feeds the "drill_list" fragment on the server page.
type drillListData struct {
	Server     string
	Status     *app.DrillStatus
	Drills     []app.DrillView
	CanOperate bool
}

func (s *Server) drillList(u *userCtx, name string) drillListData {
	d := drillListData{Server: name, CanOperate: app.RoleAtLeast(u.Role, app.RoleOperator)}
	d.Status = s.app.DrillStatusOf(name, s.app.Now())
	d.Drills, _ = s.app.ListDrills(name, 10)
	return d
}

func (s *Server) serverDrill(w http.ResponseWriter, r *http.Request, u *userCtx) {
	id, err := s.app.StartDrill(r.Context(), u.Name, r.PathValue("name"), app.DrillInput{})
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
	s.fragment(w, http.StatusOK, "run_fragment", s.newRunData(run, u))
}

func (s *Server) drillReport(w http.ResponseWriter, r *http.Request, u *userCtx) {
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil {
		s.fail(w, r, u, app.ErrNotFound)
		return
	}
	path, _, err := s.app.DrillReport(u.Name, id)
	if errors.Is(err, app.ErrNotFound) {
		s.errorPage(w, r, u, http.StatusNotFound, "This drill has no report to download.")
		return
	}
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

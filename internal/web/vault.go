package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

// Labels of the Vault page, merged into the catalogue of i18n.go.
var vaultMessages = map[string]string{
	"nav.vault":             "Vault",
	"vault.title":           "Vault",
	"vault.intro":           "An encrypted copy of every backup, somewhere else. After each backup, and every few minutes for the WAL files, pgCellar sends the Barman directory of each server to an S3 bucket with restic. The copy is encrypted on this machine before it leaves: whoever stores it cannot read it.",
	"vault.off":             "The off-site copy is off.",
	"vault.off_hint":        "Fill in the bucket below, save, then test the connection. Any S3-compatible storage works; the manual walks through OVHcloud Object Storage.",
	"vault.status":          "Where the copies go",
	"vault.bucket":          "Bucket",
	"vault.endpoint":        "Endpoint",
	"vault.folder":          "Folder",
	"vault.lock":            "Object Lock",
	"vault.lock_help":       "With a default retention, nobody can delete or change a copy before its time, not even with your keys. pgCellar does not set it: you set it on the bucket.",
	"vault.quota":           "Storage",
	"vault.of":              "of",
	"vault.used":            "used",
	"vault.no_quota":        "no quota set",
	"vault.noncurrent":      "of it kept by versioning or Object Lock (billed too)",
	"vault.keep":            "Snapshots kept",
	"vault.every":           "WAL files sent every",
	"vault.after_backup":    "after each backup only",
	"vault.max_age":         "Orange after",
	"vault.drill_source":    "Scheduled drills restore from",
	"vault.servers":         "Servers",
	"vault.copy_now":        "Copy now",
	"vault.queued":          "queued",
	"vault.running":         "copying now",
	"vault.not_copied":      "not copied",
	"vault.never":           "never",
	"vault.sent":            "sent",
	"vault.covers":          "covers",
	"vault.in":              "in",
	"vault.stored":          "in the bucket",
	"vault.recent":          "Recent copies",
	"vault.no_copy":         "No copy yet.",
	"vault.unchanged":       "nothing new",
	"vault.trigger.backup":  "after a backup",
	"vault.trigger.wal":     "WAL pass",
	"vault.trigger.manual":  "by hand",
	"vault.key":             "Recovery key",
	"vault.key_warn":        "The copies are encrypted with a key that only this pgCellar holds. Lose it and the copies are unreadable: nobody can open them, not you, not dbfactory, not the storage provider. Download the recovery key now and keep it offline, in a password manager or a safe.",
	"vault.key_download":    "Download recovery key",
	"vault.key_done":        "Downloaded",
	"vault.key_never":       "Not downloaded yet",
	"vault.key_by":          "by",
	"vault.key_created":     "created",
	"vault.test":            "Connection test",
	"vault.test_btn":        "Test connection",
	"vault.test_none":       "No test yet: save the settings, then test the connection.",
	"vault.test_ok":         "The bucket works: pgCellar wrote, read and removed a test object.",
	"vault.test_bad":        "The test failed:",
	"vault.settings":        "Settings",
	"vault.settings_ro":     "Only administrators change these settings. They live in the vault block of pgcellar.yaml.",
	"vault.enabled":         "Copy the backups to this bucket",
	"vault.f_endpoint":      "Endpoint",
	"vault.f_endpoint_h":    "With https://. OVHcloud: https://s3.<region>.io.cloud.ovh.net, for example https://s3.gra.io.cloud.ovh.net",
	"vault.f_region":        "Region",
	"vault.f_region_h":      "OVHcloud: gra, sbg, rbx, de, uk, waw, bhs... (the one of the bucket)",
	"vault.f_bucket":        "Bucket",
	"vault.f_bucket_h":      "It must exist. Create it with Object Lock if you want copies nobody can delete.",
	"vault.f_prefix":        "Folder in the bucket",
	"vault.f_prefix_h":      "One folder per pgCellar if several share a bucket. Each server gets its own repository below it.",
	"vault.f_access":        "Access key",
	"vault.f_secret":        "Secret key",
	"vault.f_secret_set":    "A secret key is saved. Leave empty to keep it.",
	"vault.f_secret_h":      "Written into a file readable by pgCellar only (mode 0600), never into pgcellar.yaml nor git.",
	"vault.f_quota":         "Storage bought",
	"vault.f_quota_h":       "For example 100GB, 500GB or 2TB. Warnings at 80 % and 100 %; copies never stop because of it. Empty = no quota.",
	"vault.f_wal":           "Send the WAL files every",
	"vault.f_wal_h":         "15m by default; 0 = only after each backup.",
	"vault.f_maxage":        "Turn a server orange when its last good copy is older than",
	"vault.f_keep":          "Snapshots kept per server",
	"vault.f_keep_h":        "restic forget --prune runs once a day. 0 = none of that kind.",
	"vault.f_last":          "last",
	"vault.f_daily":         "daily",
	"vault.f_weekly":        "weekly",
	"vault.f_monthly":       "monthly",
	"vault.f_drills":        "Scheduled restore drills restore from",
	"vault.f_drills_local":  "this host (Barman directory)",
	"vault.f_drills_vault":  "the off-site copy (fetched with restic first)",
	"vault.saved":           "Vault settings saved and recorded in git.",
	"vault.card":            "Off-site copy",
	"vault.card_off":        "This server is not copied off-site (vault: false). Tick the box in the configuration below to copy it.",
	"vault.last":            "Last copy",
	"vault.last_ok":         "Last good copy",
	"vault.repo":            "Repository",
	"vault.see_all":         "Vault page",
	"vault.help":            "How to set it up",
	"vault.help_text":       "Create an S3 user and a bucket (with Object Lock in governance mode for copies nobody can delete), paste the endpoint, the region, the bucket and the keys here, save, test, then download the recovery key. The manual has the steps for OVHcloud.",
	"vault.queued_msg":      "Copy of %s queued (position %d). It runs in the background; this page shows the result.",
	"vault.queued_after":    "Copy of %s queued: it starts when the copy in progress ends.",
	"form.vault_legend":     "Off-site copy",
	"form.vault":            "Copy this server to the Vault",
	"form.vault_h":          "After each backup and every few minutes for the WAL files, encrypted before it leaves.",
	"status.vault_none_yet": "no copy yet",
}

func init() {
	for k, v := range vaultMessages {
		if _, taken := messages[k]; !taken {
			messages[k] = v
		}
	}
}

// vaultForm is the settings form, as written or as submitted.
type vaultForm struct {
	Enabled                          bool
	Endpoint, Region, Bucket, Prefix string
	AccessKey, Quota, WALInterval    string
	MaxAge, DrillSource              string
	KeepLast, KeepDaily, KeepWeekly  int
	KeepMonthly                      int
	SecretSet                        bool
}

func formOf(v app.VaultView) vaultForm {
	c := v.Config
	last, daily, weekly, monthly := c.Keep.Resolved()
	f := vaultForm{Enabled: c.Enabled, Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.EffectivePrefix(),
		AccessKey: c.AccessKey, Quota: c.Quota, MaxAge: shortDuration(c.EffectiveMaxAge()), WALInterval: shortDuration(c.EffectiveWALInterval()),
		KeepLast: last, KeepDaily: daily, KeepWeekly: weekly, KeepMonthly: monthly, DrillSource: v.DrillSource, SecretSet: v.SecretKeySet}
	return f
}

// shortDuration writes 15m0s as 15m and 36h0m0s as 36h.
func shortDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

type vaultPage struct {
	app.VaultView
	Form     vaultForm
	Copies   []app.VaultCopyView
	Test     *app.VaultTestResult // a test just run (shown at the top)
	Problems []string
	CSRF     string
	CanOp    bool
	CanAdmin bool
}

func (s *Server) vaultData(r *http.Request, u *userCtx) vaultPage {
	v := s.app.VaultStatus()
	d := vaultPage{VaultView: v, Form: formOf(v), CSRF: u.CSRF, CanOp: s.canOperate(u), CanAdmin: s.canAdmin(u)}
	d.Copies, _ = s.app.ListVaultCopies("", 20, false)
	return d
}

func (s *Server) vaultPage(w http.ResponseWriter, r *http.Request, u *userCtx) {
	d := s.vaultData(r, u)
	p := s.newPage(r, u, tr("vault.title"), "vault", d)
	if r.URL.Query().Get("ok") == "saved" {
		p.Msg = tr("vault.saved")
	}
	s.render(w, http.StatusOK, "vault", p)
}

// parseVaultForm reads the settings form into an input (every field set).
func parseVaultForm(r *http.Request) (app.VaultInput, vaultForm, []string) {
	if err := r.ParseForm(); err != nil {
		return app.VaultInput{}, vaultForm{}, []string{"The form could not be read."}
	}
	f := r.PostForm.Get
	var problems []string
	num := func(field string) int {
		v := strings.TrimSpace(f(field))
		if v == "" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			problems = append(problems, fmt.Sprintf("%q is not a whole number of snapshots.", v))
		}
		return n
	}
	form := vaultForm{Enabled: f("enabled") != "", Endpoint: f("endpoint"), Region: f("region"), Bucket: f("bucket"), Prefix: f("prefix"),
		AccessKey: f("access_key"), Quota: f("quota"), WALInterval: f("wal_interval"), MaxAge: f("max_age"), DrillSource: f("drill_source"),
		KeepLast: num("keep_last"), KeepDaily: num("keep_daily"), KeepWeekly: num("keep_weekly"), KeepMonthly: num("keep_monthly")}
	in := app.VaultInput{Enabled: config.Ptr(form.Enabled), Endpoint: config.Ptr(form.Endpoint), Region: config.Ptr(form.Region),
		Bucket: config.Ptr(form.Bucket), Prefix: config.Ptr(form.Prefix), AccessKey: config.Ptr(form.AccessKey), SecretKey: strings.TrimSpace(f("secret_key")),
		Quota: config.Ptr(form.Quota), WALInterval: config.Ptr(form.WALInterval), MaxAge: config.Ptr(form.MaxAge),
		KeepLast: config.Ptr(form.KeepLast), KeepDaily: config.Ptr(form.KeepDaily), KeepWeekly: config.Ptr(form.KeepWeekly), KeepMonthly: config.Ptr(form.KeepMonthly),
		DrillSource: config.Ptr(form.DrillSource)}
	return in, form, problems
}

func (s *Server) vaultSave(w http.ResponseWriter, r *http.Request, u *userCtx) {
	in, form, problems := parseVaultForm(r)
	if len(problems) == 0 {
		res, err := s.app.SaveVault(u.Name, in)
		if err == nil {
			target := "/vault?ok=saved"
			if res != nil && res.Warning != "" {
				target += "&warn=git"
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		var pe *app.ProblemsError
		if !errors.As(err, &pe) {
			s.fail(w, r, u, err)
			return
		}
		problems = pe.Problems
	}
	d := s.vaultData(r, u)
	form.SecretSet = d.Form.SecretSet
	d.Form, d.Problems = form, problems
	s.render(w, http.StatusUnprocessableEntity, "vault", s.newPage(r, u, tr("vault.title"), "vault", d))
}

// vaultTest tests the settings of the form (saved or not) and shows the page
// again with the result on top.
func (s *Server) vaultTest(w http.ResponseWriter, r *http.Request, u *userCtx) {
	in, form, problems := parseVaultForm(r)
	d := s.vaultData(r, u)
	form.SecretSet = d.Form.SecretSet
	if len(problems) == 0 {
		res, err := s.app.TestVault(r.Context(), u.Name, &in)
		var pe *app.ProblemsError
		switch {
		case errors.As(err, &pe):
			problems = pe.Problems
		case err != nil:
			s.fail(w, r, u, err)
			return
		default:
			d = s.vaultData(r, u) // the result may have been stored
			d.Test = res
		}
	}
	d.Form, d.Problems = form, problems
	status := http.StatusOK
	if len(problems) > 0 {
		status = http.StatusUnprocessableEntity
	}
	s.render(w, status, "vault", s.newPage(r, u, tr("vault.title"), "vault", d))
}

func (s *Server) vaultKey(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name, body, err := s.app.RecoveryKey(u.Name)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(body)
}

// vaultCopy queues a copy of one server ("Copy now").
func (s *Server) vaultCopy(w http.ResponseWriter, r *http.Request, u *userCtx) {
	name := r.PathValue("name")
	q, err := s.app.VaultCopyNow(r.Context(), u.Name, name)
	if err != nil {
		s.fail(w, r, u, err)
		return
	}
	msg := fmt.Sprintf(tr("vault.queued_msg"), name, q.Position)
	if q.Running {
		msg = fmt.Sprintf(tr("vault.queued_after"), name)
	}
	if !isHX(r) {
		http.Redirect(w, r, "/vault", http.StatusSeeOther)
		return
	}
	s.fragment(w, http.StatusAccepted, "result_fragment", map[string]any{"Class": "ok", "Text": msg})
}

// vaultCard feeds the "Off-site copy" section of the server page.
type vaultCard struct {
	Server   string
	Status   *app.VaultServerStatus
	Copies   []app.VaultCopyView
	CanOp    bool
	Line     string
	Enabled  bool
	Interval time.Duration
}

func (s *Server) vaultCardOf(u *userCtx, name string, st *app.ServerState) vaultCard {
	cfg := s.app.Config()
	c := vaultCard{Server: name, CanOp: s.canOperate(u), Enabled: cfg.Vault.Enabled, Interval: cfg.Vault.EffectiveWALInterval()}
	if st != nil {
		c.Status = st.Vault
	}
	if c.Enabled {
		c.Copies, _ = s.app.ListVaultCopies(name, 5, false)
	}
	return c
}

// vaultMetrics adds the off-site copy families to /metrics.
func vaultMetrics(b *strings.Builder, states []app.ServerState) {
	type fam struct{ name, help string }
	fams := []fam{
		{"pgcellar_vault_last_copy_ok", "1 when the last off-site copy of the server succeeded, else 0. Absent until a copy ran."},
		{"pgcellar_vault_last_copy_age_seconds", "Seconds since the last successful off-site copy of the server ended."},
		{"pgcellar_vault_bytes", "Bytes the off-site copies of the server take in the bucket, older versions included (last measure)."},
	}
	for i, f := range fams {
		_, _ = fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", f.name, f.help, f.name)
		for _, st := range states {
			v := st.Vault
			if v == nil || !v.Copied {
				continue
			}
			switch i {
			case 0:
				if v.Last != nil {
					ok := 0
					if v.Last.OK {
						ok = 1
					}
					_, _ = fmt.Fprintf(b, "%s{server=\"%s\"} %d\n", f.name, promEscape(st.Name), ok)
				}
			case 1:
				if v.LastOKAgeSeconds != nil {
					_, _ = fmt.Fprintf(b, "%s{server=\"%s\"} %d\n", f.name, promEscape(st.Name), *v.LastOKAgeSeconds)
				}
			case 2:
				if !v.UsageAt.IsZero() {
					_, _ = fmt.Fprintf(b, "%s{server=\"%s\"} %d\n", f.name, promEscape(st.Name), v.UsedBytes)
				}
			}
		}
	}
}

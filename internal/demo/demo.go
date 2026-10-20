// Package demo is the public demonstration instance of pgCellar
// (pgcellar serve --demo): five fictional PostgreSQL servers of a fictional
// company, the fake Barman, psql and friends embedded in the binary, a history
// that is rebuilt from the clock so that ages are plausible whenever somebody
// looks, and nothing that can be changed.
//
// It is built on the same App as --dev, with the same files read the same way;
// only the data is made up. The real Barman, the real PostgreSQL servers and
// the data of the owner are never involved.
package demo

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/license"
)

//go:embed assets/fake-* fixtures/*
var assetsFS embed.FS

// Credentials of the one user of the demo.
const (
	User     = "demo"
	Password = "demo"
)

// DefaultBaseURL is used in links when PGCELLAR_BASE_URL is not set.
const DefaultBaseURL = "https://demo.pgcellar.dbfactory.io"

// How often the instance looks at itself.
const (
	streamTick  = 20 * time.Second // the date of the WAL receiver's file
	rebuildTick = 5 * time.Minute  // Barman's files, fixtures, history
	forceEvery  = time.Hour        // the database is rewritten even if nothing changed
)

// Options of Open.
type Options struct {
	// BaseURL is the public address, for links. Empty = PGCELLAR_BASE_URL, then DefaultBaseURL.
	BaseURL string
	// Now replaces the clock (tests). Zero = time.Now.
	Now func() time.Time
}

// Demo is a running demonstration instance.
type Demo struct {
	App *app.App

	root    string
	home    string // barman_home
	fixDir  string // where the fake Barman reads its answers
	recDir  string
	all     []server
	plan    plan
	now     func() time.Time
	logger  *log.Logger
	mu      sync.Mutex
	lastSig [32]byte
	lastDB  time.Time
	fps     map[string]string
}

// Root is the temporary directory that holds everything of the instance.
func (d *Demo) Root() string { return d.root }

// ExtractAssets writes the fake binaries, their fixtures and the symbolic
// links that make them answer to the real names (barman, psql, ...) into dir.
func ExtractAssets(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o750); err != nil {
		return err
	}
	err := fs.WalkDir(assetsFS, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		data, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.Base(p))
		mode := os.FileMode(0o755)
		if strings.HasPrefix(p, "fixtures/") {
			dst = filepath.Join(dir, "fixtures", filepath.Base(p))
			mode = 0o640
		}
		return os.WriteFile(dst, data, mode)
	})
	if err != nil {
		return err
	}
	for real, fake := range map[string]string{
		"barman": "fake-barman", "psql": "fake-psql", "pg_basebackup": "fake-pg_basebackup",
		"ssh": "fake-ssh", "pg_ctl": "fake-pg_ctl", "pg_isready": "fake-pg_isready", "pg_checksums": "fake-pg_checksums",
	} {
		link := filepath.Join(dir, real)
		_ = os.Remove(link)
		if err := os.Symlink(fake, link); err != nil {
			return err
		}
	}
	return nil
}

// Open builds the whole instance in a temporary directory and loads the App on
// it. The caller starts the App (a.Start) and then Run.
func Open(opts Options) (*Demo, error) {
	d := &Demo{all: servers(), now: opts.Now, logger: log.New(os.Stderr, "pgcellar demo: ", log.LstdFlags), fps: map[string]string{}}
	if d.now == nil {
		d.now = time.Now
	}
	base := opts.BaseURL
	if base == "" {
		base = os.Getenv("PGCELLAR_BASE_URL")
	}
	if base == "" {
		base = DefaultBaseURL
	}
	root, err := os.MkdirTemp("", "pgcellar-demo-")
	if err != nil {
		return nil, err
	}
	d.root = root
	fail := func(err error) (*Demo, error) {
		_ = os.RemoveAll(root)
		return nil, err
	}
	bin := filepath.Join(root, "bin")
	if err := ExtractAssets(bin); err != nil {
		return fail(fmt.Errorf("cannot extract the fake binaries: %w", err))
	}
	d.fixDir = filepath.Join(bin, "fixtures")
	d.home = filepath.Join(root, "barman")
	d.recDir = filepath.Join(root, "recoveries")
	// The fakes shadow the real programs for this process and its children.
	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return fail(err)
	}
	if err := checkFakes(bin); err != nil {
		return fail(err)
	}
	// Channels whose secrets are "set": the names appear on the Alerts page. No
	// message is ever sent by the demo.
	for _, k := range []string{"PGCELLAR_TELEGRAM_TOKEN", "PGCELLAR_SMTP_USER", "PGCELLAR_SMTP_PASSWORD"} {
		_ = os.Setenv(k, "demo-not-a-secret")
	}

	now := d.now()
	d.plan = newPlan(now)

	// A throw-away licence key: the private half exists in this process only.
	pub, priv, err := license.GenerateKeyPair()
	if err != nil {
		return fail(err)
	}
	key, err := license.Sign(priv, license.Payload{ID: "demo", Edition: license.Business, Licensee: Company, Servers: 0,
		Issued: "2026-01-01", Expires: "2099-12-31"})
	if err != nil {
		return fail(err)
	}

	hash, err := config.HashPassword(Password)
	if err != nil {
		return fail(err)
	}
	cfgPath, err := d.writeConfig(root, base, key, hash, now)
	if err != nil {
		return fail(fmt.Errorf("cannot write the configuration: %w", err))
	}
	if err := d.rebuildFiles(now); err != nil {
		return fail(err)
	}

	a, err := app.LoadDemo(cfgPath, bin)
	if err != nil {
		return fail(err)
	}
	d.App = a
	a.Demo = true
	a.Now = d.now
	a.TempDir = root
	a.CronInterval = 0
	a.LicensePublicKey = pub
	a.SSHHome = filepath.Join(root, "home")
	a.DrillTools = app.DrillTools{PgCtl: "pg_ctl", PgIsReady: "pg_isready", PgChecksums: "pg_checksums"}
	a.FSStat = d.fsStat
	a.DisplayHome = "/var/lib/barman"
	a.SetupTools.Psql, a.SetupTools.Basebackup = "psql", "pg_basebackup"

	if err := d.seedSSH(); err != nil {
		_ = a.Close()
		return nil, err
	}
	if err := d.rebuildHistory(now, true); err != nil {
		_ = a.Close()
		return nil, err
	}
	a.RefreshStatuses(context.Background())
	return d, nil
}

// fsStat pretends the backups live on a 4 TiB volume with 1.8 TiB free.
func (d *Demo) fsStat(string) (total, free, used int64, dev uint64, err error) {
	total = 4 * tib
	free = 1843 * gib
	return total, free, total - free, 1, nil
}

// seedSSH creates the key pair of the instance and authorises two servers.
func (d *Demo) seedSSH() error {
	a := d.App
	if _, err := a.SSHKey("system"); err != nil {
		return err
	}
	for _, name := range []string{ErpProd, Legacy9} {
		seed := sha256.Sum256([]byte("pgcellar-demo-postgres-key-" + name))
		pub, ok := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
		if !ok {
			return errors.New("unexpected key type")
		}
		sk, err := ssh.NewPublicKey(pub)
		if err != nil {
			return err
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sk))) + " postgres@" + name
		k, err := a.AllowSSHKey("system", name, line)
		if err != nil {
			return err
		}
		d.fps[name] = k.Fingerprint
	}
	return nil
}

// writeConfig writes the configuration directory, with a small git history,
// and returns the path of pgcellar.yaml.
func (d *Demo) writeConfig(root, baseURL, licenseKey, hash string, now time.Time) (string, error) {
	etc := filepath.Join(root, "etc")
	barmanD := filepath.Join(etc, "barman.d")
	if err := os.MkdirAll(barmanD, 0o750); err != nil {
		return "", err
	}
	barmanConf := filepath.Join(etc, "barman.conf")
	if err := os.WriteFile(barmanConf, []byte(fmt.Sprintf("[barman]\nbarman_home = %s\nbarman_user = barman\nlog_file = %s/barman.log\nconfiguration_files_directory = %s\n", d.home, root, barmanD)), 0o640); err != nil {
		return "", err
	}
	path := filepath.Join(etc, "pgcellar.yaml")
	if err := config.EnsureRepo(etc); err != nil {
		return "", err
	}

	type commit struct {
		ago    time.Duration
		author string
		msg    string
		n      int // servers present
		days   int // retention of analytics
	}
	commits := []commit{
		{61 * 24 * time.Hour, "pgcellar", "Import the existing configuration", 1, 30},
		{52 * 24 * time.Hour, "maria", "Add server crm-prod", 2, 30},
		{44 * 24 * time.Hour, "maria", "Add server analytics", 3, 30},
		{33 * 24 * time.Hour, "tom", "Add server shop-staging", 4, 30},
		{27 * 24 * time.Hour, "tom", "Add server legacy-9", 5, 30},
		{9 * 24 * time.Hour, "maria", "Change retention of analytics to 14 days", 5, 14},
	}
	for _, c := range commits {
		cfg := d.config(root, baseURL, licenseKey, hash, c.n, c.days)
		if err := config.Save(path, cfg); err != nil {
			return "", err
		}
		for _, s := range d.serversUpTo(c.n, c.days) {
			if err := os.WriteFile(filepath.Join(barmanD, s.Name+".conf"), []byte(d.plan.barmanConf(s)), 0o640); err != nil {
				return "", err
			}
		}
		if _, err := config.CommitAt(etc, c.msg, c.author, c.author+"@northwind.example", now.Add(-c.ago)); err != nil {
			return "", err
		}
	}
	return path, nil
}

func (d *Demo) serversUpTo(n, analyticsDays int) []server {
	out := append([]server(nil), d.all[:n]...)
	for i := range out {
		if out[i].Name == Analytics {
			out[i].Ret.RecoveryWindowDays = analyticsDays
		}
	}
	return out
}

func (d *Demo) config(root, baseURL, licenseKey, hash string, n, analyticsDays int) *config.Config {
	cfg := config.Defaults()
	cfg.Listen = ":8080"
	cfg.BaseURL = baseURL
	cfg.DataDir = filepath.Join(root, "data")
	cfg.Barman = config.Barman{Binary: "barman", ConfigFile: filepath.Join(root, "etc", "barman.conf"), ConfigDir: filepath.Join(root, "etc", "barman.d")}
	cfg.Scheduler.Enabled = config.Ptr(false) // the demo never starts anything
	cfg.SSHHost, cfg.SSHPort = "demo.pgcellar.example", 2222
	cfg.AuditRetentionDays = config.Ptr(3650)
	cfg.Recoveries = config.Recoveries{Dir: d.recDir, MaxDownloadBytes: 2 * gib, KeepDays: config.Ptr(0)}
	cfg.License.Key = licenseKey
	cfg.Users = []config.User{{Name: User, Role: app.RoleViewer, PasswordHash: hash}}
	cfg.Alerts.MaxBackupAge, cfg.Alerts.MaxWALAge = 26*time.Hour, time.Hour
	cfg.Alerts.DailySummary = "08:30"
	cfg.Alerts.Telegram = &config.TelegramAlert{TokenEnv: "PGCELLAR_TELEGRAM_TOKEN", ChatID: "-1001234567890"}
	cfg.Alerts.Mail = &config.MailAlert{Host: "smtp.northwind.example", Port: 587, StartTLS: config.Ptr(true), From: "pgcellar@northwind.example",
		To: []string{"dba@northwind.example", "ops@northwind.example"}, UserEnv: "PGCELLAR_SMTP_USER", PasswordEnv: "PGCELLAR_SMTP_PASSWORD"}
	for _, s := range d.serversUpTo(n, analyticsDays) {
		cs := config.Server{Name: s.Name, Label: s.Label, Retention: s.Ret, Tags: s.Tags,
			Schedule: config.Schedule{Backup: d.plan.scheduleText(s.Name)}, Drills: config.ServerDrills{Schedule: s.DrillSchedule}}
		if s.ScheduleOff {
			cs.Schedule.Enabled = config.Ptr(false)
		} else {
			cs.Schedule.Enabled = config.Ptr(true)
		}
		cfg.Servers = append(cfg.Servers, cs)
	}
	return cfg
}

// rebuildFiles writes Barman's files and the answers of the fakes for `now`.
func (d *Demo) rebuildFiles(now time.Time) error {
	if err := d.plan.writeFixtures(d.fixDir, d.all, now); err != nil {
		return err
	}
	for _, s := range d.all {
		if err := d.plan.writeHome(d.home, s, now); err != nil {
			return err
		}
	}
	return nil
}

// rebuildHistory rewrites the runs, drills, audit lines and alert journal when
// they changed (or force), and the scheduler's memory.
func (d *Demo) rebuildHistory(now time.Time, force bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.plan.history(d.all, now, d.recDir, d.fps)
	if err := h.writeReports(); err != nil {
		return err
	}
	raw, err := json.Marshal(h.Snapshot)
	if err != nil {
		return err
	}
	sig := sha256.Sum256(raw)
	if !force && sig == d.lastSig && time.Since(d.lastDB) < forceEvery {
		return nil
	}
	st := d.App.Store
	if err := st.ReplaceSnapshot(h.Snapshot); err != nil {
		return err
	}
	for key, rec := range h.Last {
		name := "scheduler.last." + key
		if strings.HasPrefix(key, "drill:") {
			name = "scheduler.drill.last." + strings.TrimPrefix(key, "drill:")
		}
		b, _ := json.Marshal(rec)
		if err := st.SetSetting(name, string(b)); err != nil {
			return err
		}
	}
	d.lastSig, d.lastDB = sig, time.Now()
	return nil
}

// Run keeps the fictional world moving until ctx ends: the WAL receiver
// "writes", new nightly backups appear, old ones expire.
func (d *Demo) Run(ctx context.Context) {
	stream := time.NewTicker(streamTick)
	rebuild := time.NewTicker(rebuildTick)
	defer stream.Stop()
	defer rebuild.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stream.C:
			now := d.now()
			for _, s := range d.all {
				if err := d.plan.touchStreaming(d.home, s, now); err != nil {
					d.logger.Printf("streaming: %v", err)
				}
			}
		case <-rebuild.C:
			now := d.now()
			if err := d.rebuildFiles(now); err != nil {
				d.logger.Printf("rebuild files: %v", err)
				continue
			}
			if err := d.rebuildHistory(now, false); err != nil {
				d.logger.Printf("rebuild history: %v", err)
			}
		}
	}
}

// fakeNames are the programs the demo must find in its own directory.
var fakeNames = []string{"barman", "psql", "pg_basebackup", "ssh", "pg_ctl", "pg_isready", "pg_checksums"}

// checkFakes refuses to go on when a program of the demo does not resolve to
// the extracted fake: with a directory mounted noexec (the default of a Docker
// tmpfs) the real Barman or psql of the image would answer instead.
func checkFakes(dir string) error {
	if err := barman.CheckOnlyUnder(dir, fakeNames...); err != nil {
		return fmt.Errorf("demo mode: the fake barman in %s is not executable (noexec mount?): %w", dir, err)
	}
	return nil
}

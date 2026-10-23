package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/vault/s3test"
)

// vaultEnv is a test App with the vault on, aimed at a fake S3 service, and
// the S3 secret key written where pgCellar looks for it.
func vaultEnv(t *testing.T, mutate func(*config.Config)) (*apptest.Env, *s3test.Server) {
	t.Helper()
	srv := s3test.New()
	t.Cleanup(srv.Close)
	e := apptest.NewWith(t, func(c *config.Config) {
		c.Vault = config.Vault{Enabled: true, Endpoint: srv.URL, Region: srv.Region, Bucket: srv.Bucket, AccessKey: srv.AccessKey}
		if mutate != nil {
			mutate(c)
		}
	})
	cfg := e.App.Config()
	p := cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(srv.SecretKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e, srv
}

func resticCalls(t *testing.T, e *apptest.Env) string {
	t.Helper()
	b, _ := os.ReadFile(e.ResticLog)
	return string(b)
}

func copyNow(t *testing.T, a *app.App, server string) {
	t.Helper()
	if _, err := a.VaultCopyNow(context.Background(), "alice", server); err != nil {
		t.Fatal(err)
	}
}

func lastCopy(t *testing.T, a *app.App, server string) app.VaultCopyView {
	t.Helper()
	list, err := a.ListVaultCopies(server, 1, true)
	if err != nil || len(list) == 0 {
		t.Fatalf("no copy of %s: %v", server, err)
	}
	return list[0]
}

func TestVaultCopyEndToEnd(t *testing.T) {
	e, srv := vaultEnv(t, nil)
	a := e.App
	ctx := context.Background()
	srv.PutObject("pgcellar/pg-main/data/00/abc", make([]byte, 5000))

	q, err := a.VaultCopyNow(ctx, "alice", "pg-main")
	if err != nil || q.Position != 1 || q.Running {
		t.Fatalf("queued: %+v %v", q, err)
	}
	a.VaultTick(ctx, a.Now())
	c := lastCopy(t, a, "pg-main")
	if !c.OK || c.Trigger != app.VaultTriggerManual || c.Actor != "alice" || c.SnapshotID == "" || !c.Initialised || c.BytesTotal == 0 || c.Running {
		t.Fatalf("copy: %+v", c)
	}
	if !strings.Contains(c.Journal, "created the repository") {
		t.Errorf("journal: %s", c.Journal)
	}

	// The restic password was generated, 0600, and is not in pgcellar.yaml.
	cfg := a.Config()
	pf := cfg.Vault.EffectivePasswordFile(cfg.DataDir)
	fi, err := os.Stat(pf)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("password file: %v %v", fi, err)
	}
	pw, _ := os.ReadFile(pf)
	yaml, _ := os.ReadFile(filepath.Join(e.Dir, "pgcellar.yaml"))
	if strings.Contains(string(yaml), strings.TrimSpace(string(pw))) || strings.Contains(string(yaml), srv.SecretKey) {
		t.Error("a secret reached pgcellar.yaml")
	}

	calls := resticCalls(t, e)
	repo := "s3:" + srv.URL + "/" + srv.Bucket + "/pgcellar/pg-main"
	for _, want := range []string{"-r " + repo, "backup --json --host pgcellar --tag pgcellar --tag server=pg-main --skip-if-unchanged " + filepath.Join(e.Home, "pg-main") + " " + filepath.Join(e.BarmanDir, "pg-main.conf"), " init", "-o s3.region=" + srv.Region} {
		if !strings.Contains(calls, want) {
			t.Errorf("restic calls lack %q:\n%s", want, calls)
		}
	}
	// The command is audited, never the secret key.
	audit, _ := a.Audit(100)
	found := false
	for _, l := range audit {
		if strings.Contains(l.Action, srv.SecretKey) || strings.Contains(l.Details, srv.SecretKey) {
			t.Fatal("the secret key reached the audit log")
		}
		if strings.Contains(l.Action, "fake-restic -r "+repo) && l.Target == "pg-main" && l.Actor == "alice" {
			found = true
		}
	}
	if !found {
		t.Errorf("restic is not in the audit log: %+v", audit)
	}

	st, _ := a.State("pg-main")
	if st.Vault == nil || !st.Vault.Copied || st.Vault.Last == nil || !st.Vault.Last.OK || st.Vault.Stale || st.Vault.Emoji != "✅" || !strings.HasPrefix(st.Vault.Text, "Off-site copy: ") {
		t.Fatalf("status: %+v", st.Vault)
	}
	if st.Vault.UsedBytes != 5000 {
		t.Errorf("usage measured on S3 = %d", st.Vault.UsedBytes)
	}
	if st.Level != app.LevelOK {
		t.Errorf("a good copy keeps the server healthy: %s %v", st.Level, st.Problems)
	}

	// Nothing changed: restic makes no new snapshot.
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	if c2 := lastCopy(t, a, "pg-main"); !c2.OK || !c2.Unchanged || c2.SnapshotID != "" || c2.Initialised {
		t.Errorf("second copy: %+v", c2)
	}
	v := a.VaultStatus()
	if !v.Enabled || !v.SecretKeySet || !v.KeyExists || v.KeyCreatedAt.IsZero() || len(v.Servers) != 1 || v.UsedBytes != 5000 {
		t.Errorf("vault view: %+v", v)
	}
}

func TestVaultAfterBackupAndWALPasses(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	ctx := context.Background()
	id, err := a.StartBackup(ctx, "scheduler", "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	apptest.WaitRun(t, a, id)
	st, _ := a.State("pg-main")
	if st.Vault == nil || !st.Vault.Queued {
		t.Fatalf("a successful backup queues a copy: %+v", st.Vault)
	}
	now := a.Now()
	a.VaultTick(ctx, now)
	c := lastCopy(t, a, "pg-main")
	if !c.OK || c.Trigger != app.VaultTriggerBackup || c.Actor != "scheduler" {
		t.Fatalf("copy after the backup: %+v", c)
	}
	// The WAL pass waits for vault.wal_interval (15 min) after the last copy.
	a.VaultTick(ctx, now.Add(5*time.Minute))
	if lastCopy(t, a, "pg-main").ID != c.ID {
		t.Error("a WAL pass ran before wal_interval")
	}
	a.VaultTick(ctx, now.Add(16*time.Minute))
	w := lastCopy(t, a, "pg-main")
	if w.ID == c.ID || w.Trigger != app.VaultTriggerWAL || w.Actor != "system" {
		t.Errorf("WAL pass: %+v", w)
	}
}

func TestVaultSkipsAServerAndWaitsForItsBackup(t *testing.T) {
	e, _ := vaultEnv(t, func(c *config.Config) {
		c.Servers = append(c.Servers, config.Server{Name: "pg-rsync", Vault: config.Ptr(false)})
	})
	a := e.App
	ctx := context.Background()
	if _, err := a.VaultCopyNow(ctx, "alice", "pg-rsync"); err == nil {
		t.Error("a server with vault: false cannot be copied")
	}
	st, _ := a.State("pg-rsync")
	if st.Vault == nil || st.Vault.Copied || st.Vault.Emoji != "💤" {
		t.Errorf("vault: false status: %+v", st.Vault)
	}

	// A copy waits while a backup of the same server runs.
	t.Setenv("FAKE_BARMAN_BACKUP_DELAY", "1")
	id, err := a.StartBackup(ctx, "alice", "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.VaultCopyNow(ctx, "alice", "pg-main"); err != nil {
		t.Fatal(err)
	}
	a.VaultTick(ctx, a.Now())
	if list, _ := a.ListVaultCopies("pg-main", 5, false); len(list) != 0 {
		t.Fatalf("a copy ran during the backup: %+v", list)
	}
	apptest.WaitRun(t, a, id)
	a.VaultTick(ctx, a.Now())
	if c := lastCopy(t, a, "pg-main"); !c.OK || c.Trigger != app.VaultTriggerManual {
		t.Errorf("the click is kept over the backup trigger: %+v", c)
	}
}

func TestVaultFailuresTurnTheServerOrangeAndAlert(t *testing.T) {
	sk := newSink(t)
	e, _ := vaultEnv(t, func(c *config.Config) {
		c.Alerts.Webhook = &config.WebhookAlert{URL: sk.srv.URL}
	})
	a := e.App
	ctx := context.Background()
	a.AlertsTick(ctx, day0) // baseline
	t.Setenv("FAKE_RESTIC_FAIL", "1")

	now := a.Now()
	a.VaultTick(ctx, now) // a WAL pass (first copy of the server)
	c := lastCopy(t, a, "pg-main")
	if c.OK || !strings.Contains(c.Error, "cannot be reached") {
		t.Fatalf("failed copy: %+v", c)
	}
	a.AlertsTick(ctx, now)
	if got := sk.take("pg-main"); len(got) != 0 {
		t.Fatalf("one failed WAL pass is not news: %+v", got)
	}
	a.VaultTick(ctx, now.Add(16*time.Minute))
	a.AlertsTick(ctx, now.Add(16*time.Minute))
	got := sk.take("pg-main")
	if len(got) != 1 || got[0].Kind != alerts.KindVaultFailed || got[0].Level != "warn" || !strings.Contains(got[0].Title, "Off-site copy of pg-main failed") {
		t.Fatalf("second failure in a row: %+v", got)
	}
	st, _ := a.State("pg-main")
	if st.Vault.Stale || st.Level != app.LevelOK {
		t.Errorf("two failures do not turn the server orange yet: %+v", st.Vault)
	}
	a.VaultTick(ctx, now.Add(32*time.Minute))
	st, _ = a.State("pg-main")
	if !st.Vault.Stale || st.Level != app.LevelWarn || st.Vault.FailStreak != 3 || !strings.Contains(strings.Join(st.Problems, " "), "The last 3 off-site copies failed") {
		t.Fatalf("three failures: %s %+v %v", st.Level, st.Vault, st.Problems)
	}
	a.AlertsTick(ctx, now.Add(32*time.Minute))
	for _, m := range sk.take("pg-main") {
		if m.Kind == alerts.KindVaultFailed {
			t.Errorf("a run of failures is announced once: %+v", m)
		}
	}

	// Fixed: the next copy works, the server is healthy again.
	t.Setenv("FAKE_RESTIC_FAIL", "")
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, now.Add(33*time.Minute))
	st, _ = a.State("pg-main")
	if st.Vault.Stale || st.Level != app.LevelOK {
		t.Errorf("after a good copy: %+v", st.Vault)
	}
	// A failed copy after a click is announced at once.
	t.Setenv("FAKE_RESTIC_FAIL", "1")
	a.AlertsTick(ctx, now.Add(34*time.Minute))
	sk.take("")
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, now.Add(35*time.Minute))
	a.AlertsTick(ctx, now.Add(35*time.Minute))
	if got := sk.take("pg-main"); len(got) != 1 || got[0].Kind != alerts.KindVaultFailed {
		t.Errorf("failed click: %+v", got)
	}
}

func TestVaultMaxAge(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	ctx := context.Background()
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	later := a.Now().Add(37 * time.Hour)
	setClock(e, later)
	st, _ := a.State("pg-main")
	if !st.Vault.Stale || !strings.Contains(st.Vault.Why, "No off-site copy succeeded for") {
		t.Errorf("a copy older than max_age: %+v", st.Vault)
	}
}

func TestVaultPartialCopyAndWrongPassword(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	ctx := context.Background()
	t.Setenv("FAKE_RESTIC_PARTIAL", "1")
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	c := lastCopy(t, a, "pg-main")
	if !c.OK || len(c.Warnings) < 2 || !strings.Contains(strings.Join(c.Warnings, " "), "vanished") {
		t.Fatalf("exit 3 is a copy with warnings: %+v", c)
	}
	t.Setenv("FAKE_RESTIC_PARTIAL", "")

	cfg := a.Config()
	if err := os.WriteFile(cfg.Vault.EffectivePasswordFile(cfg.DataDir), []byte("another\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	if c := lastCopy(t, a, "pg-main"); c.OK || !strings.Contains(c.Error, "password does not open this repository") {
		t.Errorf("wrong password: %+v", c)
	}
}

func TestVaultMissingSecret(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	cfg := a.Config()
	_ = os.Remove(cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir))
	copyNow(t, a, "pg-main")
	a.VaultTick(context.Background(), a.Now())
	if c := lastCopy(t, a, "pg-main"); c.OK || !strings.Contains(c.Error, "secret key file") {
		t.Errorf("missing secret: %+v", c)
	}
}

func TestVaultQuota(t *testing.T) {
	sk := newSink(t)
	e, srv := vaultEnv(t, func(c *config.Config) {
		c.Vault.Quota = "10KB"
		c.Alerts.Webhook = &config.WebhookAlert{URL: sk.srv.URL}
	})
	a := e.App
	ctx := context.Background()
	a.AlertsTick(ctx, day0)
	srv.PutObject("pgcellar/pg-main/data/a", make([]byte, 8500))
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	v := a.VaultStatus()
	if v.UsedBytes != 8500 || v.QuotaBytes != 10000 || v.QuotaLevel != app.LevelWarn || v.UsedPct() != 85 {
		t.Fatalf("quota: %+v", v)
	}
	vaultOnly := func(in []alerts.Payload) []alerts.Payload {
		var out []alerts.Payload
		for _, m := range in {
			if strings.HasPrefix(m.Kind, "vault_") {
				out = append(out, m)
			}
		}
		return out
	}
	a.AlertsTick(ctx, day0.Add(time.Minute))
	got := vaultOnly(sk.take(""))
	if len(got) != 1 || got[0].Kind != alerts.KindVaultQuotaWarn || got[0].Lines[0] != "8.5 kB of 10 kB used (85 %)." {
		t.Fatalf("80 %%: %+v", got)
	}
	a.AlertsTick(ctx, day0.Add(2*time.Minute))
	if got := vaultOnly(sk.take("")); len(got) != 0 {
		t.Fatalf("80 %% is said once: %+v", got)
	}
	// Over the quota: the copy still runs, and it is announced.
	srv.PutObject("pgcellar/pg-main/data/b", make([]byte, 3000))
	copyNow(t, a, "pg-main")
	a.VaultTick(ctx, a.Now())
	if c := lastCopy(t, a, "pg-main"); !c.OK {
		t.Fatalf("the quota never stops a copy: %+v", c)
	}
	a.AlertsTick(ctx, day0.Add(3*time.Minute))
	got = vaultOnly(sk.take(""))
	if len(got) != 1 || got[0].Kind != alerts.KindVaultQuota || got[0].Level != "fail" || !strings.Contains(strings.Join(got[0].Lines, " "), "Copies go on") {
		t.Fatalf("100 %%: %+v", got)
	}
	states, _, _ := a.States()
	line := a.VaultSummary(states)
	if line == nil || line.Level != app.LevelFail || !strings.Contains(line.Text, "11.5 kB of 10 kB used (115 %)") || !strings.Contains(line.Text, "1 of 1 servers up to date") {
		t.Errorf("status line: %+v", line)
	}
}

func TestVaultTestConnection(t *testing.T) {
	e, srv := vaultEnv(t, nil)
	a := e.App
	ctx := context.Background()
	srv.SetLock(30)
	res, err := a.TestVault(ctx, "admin-user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Steps) != 5 || !res.Lock.Locked() || res.Lock.Days != 30 {
		t.Fatalf("test: %+v", res)
	}
	if keys := srv.Keys("pgcellar/.pgcellar-test/"); len(keys) != 0 {
		t.Errorf("the test object stays: %v", keys)
	}
	v := a.VaultStatus()
	if v.LastTest == nil || !v.LastTest.OK || v.LockText != "Locked for 30 days (governance)" {
		t.Errorf("stored: %+v %q", v.LastTest, v.LockText)
	}
	states, _, _ := a.States()
	if line := a.VaultSummary(states); line == nil || !strings.Contains(line.Text, "locked 30 days") {
		t.Errorf("status line: %+v", line)
	}

	// Other settings from the form, not saved: tested, not stored.
	wrong := "nope"
	res, err = a.TestVault(ctx, "admin-user", &app.VaultInput{SecretKey: wrong})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "secret key does not match") {
		t.Errorf("wrong secret: %+v", res)
	}
	other := "other-bucket"
	res, _ = a.TestVault(ctx, "admin-user", &app.VaultInput{Bucket: &other})
	if res.OK || !strings.Contains(res.Error, "bucket does not exist") {
		t.Errorf("missing bucket: %+v", res)
	}
	if v := a.VaultStatus(); !v.LastTest.OK {
		t.Error("a test of unsaved settings must not replace the stored result")
	}
	bad := "ftp://x"
	if _, err := a.TestVault(ctx, "admin-user", &app.VaultInput{Endpoint: &bad}); err == nil {
		t.Error("an invalid endpoint is refused before any request")
	}
}

func TestVaultSaveAndRecoveryKey(t *testing.T) {
	e := apptest.New(t)
	a := e.App
	if _, _, err := a.RecoveryKey("admin-user"); err == nil {
		t.Error("no recovery key while the vault is off")
	}
	in := app.VaultInput{Enabled: config.Ptr(true), Endpoint: config.Ptr("https://s3.gra.io.cloud.ovh.net/"), Region: config.Ptr("gra"),
		Bucket: config.Ptr("acme-backups"), Prefix: config.Ptr("pgcellar"), AccessKey: config.Ptr("AK123"),
		Quota: config.Ptr("500GB"), WALInterval: config.Ptr("15m"), MaxAge: config.Ptr("48h"), KeepDaily: config.Ptr(7), KeepWeekly: config.Ptr(8)}
	if _, err := a.SaveVault("admin-user", in); err == nil || !strings.Contains(err.Error(), "secret key is missing") {
		t.Fatalf("enabling without a secret key: %v", err)
	}
	in.SecretKey = "top-secret-value"
	res, err := a.SaveVault("admin-user", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit == "" {
		t.Errorf("not recorded in git: %+v", res)
	}
	cfg := a.Config()
	v := cfg.Vault
	if !v.Enabled || v.Endpoint != "https://s3.gra.io.cloud.ovh.net" || v.Prefix != nil || v.WALInterval != nil || *v.MaxAge != 48*time.Hour || v.Keep.Daily != nil || *v.Keep.Weekly != 8 {
		t.Errorf("saved: %+v", v)
	}
	raw, _ := os.ReadFile(filepath.Join(e.Dir, "pgcellar.yaml"))
	if strings.Contains(string(raw), "top-secret-value") || !strings.Contains(string(raw), "bucket: acme-backups") {
		t.Errorf("yaml:\n%s", raw)
	}
	sp := v.EffectiveSecretKeyFile(cfg.DataDir)
	if b, err := os.ReadFile(sp); err != nil || strings.TrimSpace(string(b)) != "top-secret-value" {
		t.Errorf("secret file: %q %v", b, err)
	}
	if fi, _ := os.Stat(sp); fi.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode %v", fi.Mode())
	}
	bad := app.VaultInput{WALInterval: config.Ptr("often")}
	var pe *app.ProblemsError
	if _, err := a.SaveVault("admin-user", bad); !errors.As(err, &pe) {
		t.Errorf("a bad duration: %v", err)
	}

	name, body, err := a.RecoveryKey("admin-user")
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := os.ReadFile(v.EffectivePasswordFile(cfg.DataDir))
	text := string(body)
	if !strings.HasPrefix(name, "pgcellar-recovery-key-") || !strings.Contains(text, strings.TrimSpace(string(pw))) ||
		!strings.Contains(text, "s3:https://s3.gra.io.cloud.ovh.net/acme-backups/pgcellar/pg-main") || strings.Contains(text, "top-secret-value") {
		t.Errorf("recovery key %s:\n%s", name, text)
	}
	// The same password every time: it is generated once.
	_, again, _ := a.RecoveryKey("admin-user")
	if !strings.Contains(string(again), strings.TrimSpace(string(pw))) {
		t.Error("the password changed")
	}
	st := a.VaultStatus()
	if st.KeyDownloadedAt.IsZero() || st.KeyDownloadedBy != "admin-user" {
		t.Errorf("download not recorded: %+v", st)
	}
	audit, _ := a.Audit(50)
	seen := map[string]bool{}
	for _, l := range audit {
		seen[l.Action] = true
	}
	for _, want := range []string{"vault.settings", "vault.secret", "vault.key.download", "vault.key.create"} {
		if !seen[want] {
			t.Errorf("audit lacks %s", want)
		}
	}
}

func TestVaultUpdateReadyWaitsForACopy(t *testing.T) {
	e, _ := vaultEnv(t, nil)
	a := e.App
	t.Setenv("FAKE_RESTIC_DELAY", "1")
	copyNow(t, a, "pg-main")
	done := make(chan struct{})
	go func() { a.VaultTick(context.Background(), a.Now()); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if running, _ := a.UpdateReady(a.Now(), true); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("UpdateReady never saw the copy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done
	if ok, why := a.UpdateReady(a.Now(), true); !ok {
		t.Errorf("after the copy: %s", why)
	}
}

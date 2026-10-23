package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// VaultInput is what the Vault page and PUT /api/v1/vault change. A nil
// field keeps its value. The secret key is never stored in pgcellar.yaml:
// it is written into vault.secret_key_file.
type VaultInput struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	Endpoint    *string `json:"endpoint,omitempty"`
	Region      *string `json:"region,omitempty"`
	Bucket      *string `json:"bucket,omitempty"`
	Prefix      *string `json:"prefix,omitempty"`
	AccessKey   *string `json:"access_key,omitempty"`
	SecretKey   string  `json:"secret_key,omitempty"` // empty = keep the file as it is
	Quota       *string `json:"quota,omitempty"`
	WALInterval *string `json:"wal_interval,omitempty"` // "15m", "0" = after backups only
	MaxAge      *string `json:"max_age,omitempty"`      // "36h"
	KeepLast    *int    `json:"keep_last,omitempty"`
	KeepDaily   *int    `json:"keep_daily,omitempty"`
	KeepWeekly  *int    `json:"keep_weekly,omitempty"`
	KeepMonthly *int    `json:"keep_monthly,omitempty"`
	DrillSource *string `json:"drill_source,omitempty"` // local | vault
}

func trimmed(p *string) string { return strings.TrimSpace(*p) }

// apply writes the input into a configuration. Durations that do not parse
// are reported as problems.
func (in VaultInput) apply(cfg *config.Config) []string {
	var problems []string
	v := &cfg.Vault
	if in.Enabled != nil {
		v.Enabled = *in.Enabled
	}
	if in.Endpoint != nil {
		v.Endpoint = strings.TrimRight(trimmed(in.Endpoint), "/")
	}
	if in.Region != nil {
		v.Region = trimmed(in.Region)
	}
	if in.Bucket != nil {
		v.Bucket = trimmed(in.Bucket)
	}
	if in.Prefix != nil {
		if p := strings.Trim(trimmed(in.Prefix), "/"); p == config.DefaultVaultPrefix {
			v.Prefix = nil
		} else {
			v.Prefix = config.Ptr(p)
		}
	}
	if in.AccessKey != nil {
		v.AccessKey = trimmed(in.AccessKey)
	}
	if in.Quota != nil {
		v.Quota = trimmed(in.Quota)
	}
	dur := func(p *string, name string, def time.Duration) *time.Duration {
		s := trimmed(p)
		if s == "" {
			return nil
		}
		if s == "0" {
			return config.Ptr(time.Duration(0))
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			problems = append(problems, fmt.Sprintf("vault.%s %q is not a duration (for example \"15m\" or \"36h\")", name, s))
			return nil
		}
		if d == def {
			return nil
		}
		return &d
	}
	if in.WALInterval != nil {
		v.WALInterval = dur(in.WALInterval, "wal_interval", config.DefaultVaultWALInterval)
	}
	if in.MaxAge != nil {
		v.MaxAge = dur(in.MaxAge, "max_age", config.DefaultVaultMaxAge)
	}
	keep := func(dst **int, src *int, def int) {
		if src == nil {
			return
		}
		if *src == def {
			*dst = nil
			return
		}
		*dst = config.Ptr(*src)
	}
	keep(&v.Keep.Last, in.KeepLast, 0)
	keep(&v.Keep.Daily, in.KeepDaily, config.DefaultVaultKeepDaily)
	keep(&v.Keep.Weekly, in.KeepWeekly, config.DefaultVaultKeepWeekly)
	keep(&v.Keep.Monthly, in.KeepMonthly, 0)
	if in.DrillSource != nil {
		if s := strings.ToLower(trimmed(in.DrillSource)); s == "" || s == config.DrillSourceLocal {
			cfg.Drills.Source = ""
		} else {
			cfg.Drills.Source = s
		}
	}
	return problems
}

// SaveVault changes the vault settings in pgcellar.yaml (recorded in git) and
// writes the secret key into its file when one is given.
func (a *App) SaveVault(actor string, in VaultInput) (*WriteResult, error) {
	var parseProblems []string
	err := a.mutateConfig(func(cfg *config.Config) error {
		parseProblems = in.apply(cfg)
		if len(parseProblems) > 0 {
			return &ProblemsError{Problems: parseProblems}
		}
		if cfg.Vault.Enabled && in.SecretKey == "" {
			if _, serr := vaultSecret(cfg); serr != nil {
				return &ProblemsError{Problems: []string{"The S3 secret key is missing: paste it in the form (it is written into " + cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir) + ", never into pgcellar.yaml)."}}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	cfg := a.Config()
	if s := strings.TrimSpace(in.SecretKey); s != "" {
		p := cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir)
		if err := writeSecretFile(cfg, p, s); err != nil {
			return nil, fmt.Errorf("the settings were saved, but the secret key could not be written into %s: %w", p, err)
		}
		_ = a.Store.AddAudit(actor, "vault.secret", "", "the S3 secret key was replaced in "+p)
	}
	_ = a.Store.AddAudit(actor, "vault.settings", "", fmt.Sprintf("enabled=%t bucket=%s", cfg.Vault.Enabled, cfg.Vault.Bucket))
	return a.finishWrite("vault: settings changed", actor), nil
}

// VaultTestStep is one line of a connection test.
type VaultTestStep struct {
	Key    string `json:"key"` // restic, write, read, delete, lock
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Note   string `json:"note,omitempty"`
	Millis int64  `json:"millis"`
}

// VaultTestResult is the outcome of "Test connection".
type VaultTestResult struct {
	At     time.Time       `json:"at"`
	Actor  string          `json:"actor"`
	OK     bool            `json:"ok"`
	Steps  []VaultTestStep `json:"steps"`
	Lock   vault.Lock      `json:"lock"`
	Target string          `json:"target"`
	Error  string          `json:"error,omitempty"`

	secretGiven bool // the test used a secret key from the form, not the saved one
}

// sameVault reports whether the tested settings are the saved ones: only then
// is the result kept.
func sameVault(saved, tested *config.Config, secretGiven bool) bool {
	if secretGiven {
		return false
	}
	a, b := saved.Vault, tested.Vault
	a.Enabled, b.Enabled = true, true
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// LockText is the lock line of the result.
func (r VaultTestResult) LockText() string { return lockLine(r.Lock, r.At, "") }

// TestVault checks the bucket with the given settings (nil = the saved
// ones): restic is installed, a small object can be written, read back and
// removed, and the Object Lock rule is read. Nothing is saved but the result,
// and only when the settings tested are the saved ones.
func (a *App) TestVault(ctx context.Context, actor string, in *VaultInput) (*VaultTestResult, error) {
	saved := a.Config()
	cfg := *saved
	secret := ""
	if in != nil {
		if p := in.apply(&cfg); len(p) > 0 {
			return nil, &ProblemsError{Problems: p}
		}
		secret = strings.TrimSpace(in.SecretKey)
	}
	cfg.Vault.Enabled = true // a test is about the bucket, enabled or not
	if p := config.ValidateWith(&cfg, true); len(p) > 0 {
		var mine []string
		for _, s := range p {
			if strings.HasPrefix(s, "vault.") {
				mine = append(mine, s)
			}
		}
		if len(mine) > 0 {
			return nil, &ProblemsError{Problems: mine}
		}
	}
	res := &VaultTestResult{At: a.now(), Actor: actor, Target: vaultTarget(cfg.Vault), Steps: []VaultTestStep{}, secretGiven: secret != ""}
	step := func(key, label string, fn func() (string, error)) bool {
		t := time.Now()
		note, err := fn()
		s := VaultTestStep{Key: key, Label: label, OK: err == nil, Note: note, Millis: time.Since(t).Milliseconds()}
		if err != nil {
			s.Note = err.Error()
			if res.Error == "" {
				res.Error = label + ": " + err.Error()
			}
		}
		res.Steps = append(res.Steps, s)
		return err == nil
	}
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	step("restic", "restic is installed", func() (string, error) {
		out, _, err := a.Tools.RunBinary(tctx, a.resticBin(), "version")
		if err != nil {
			return "", fmt.Errorf("%s cannot be run: %v (the Docker image ships it; on a binary install, install restic 0.17 or later)", a.resticBin(), err)
		}
		return strings.TrimSpace(string(out)), nil
	})
	if secret == "" {
		s, err := vaultSecret(&cfg)
		if err != nil {
			step("secret", "S3 secret key", func() (string, error) { return "", err })
			return a.finishTest(saved, &cfg, res), nil
		}
		secret = s
	}
	v := cfg.Vault
	client := &vault.S3{Endpoint: v.Endpoint, Region: v.EffectiveRegion(), Bucket: v.Bucket, AccessKey: v.AccessKey, SecretKey: secret}
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	key := strings.TrimPrefix(v.EffectivePrefix()+"/.pgcellar-test/"+hex.EncodeToString(buf), "/")
	body := []byte("pgCellar connection test, " + res.At.UTC().Format(time.RFC3339) + "\n")
	if step("write", "Write a test object", func() (string, error) {
		return key, client.Put(tctx, key, body)
	}) {
		step("read", "Read it back", func() (string, error) {
			got, err := client.Get(tctx, key)
			if err == nil && !bytes.Equal(got, body) {
				err = fmt.Errorf("the object read back differs from the one written (%d bytes instead of %d)", len(got), len(body))
			}
			return fmt.Sprintf("%d bytes", len(got)), err
		})
		step("delete", "Remove it", func() (string, error) {
			return "", client.Delete(tctx, key)
		})
	}
	step("lock", "Read the Object Lock rule", func() (string, error) {
		l, err := client.ObjectLock(tctx)
		res.Lock = l
		return lockLine(l, res.At, ""), err
	})
	return a.finishTest(saved, &cfg, res), nil
}

func (a *App) finishTest(saved, tested *config.Config, res *VaultTestResult) *VaultTestResult {
	res.OK = res.Error == ""
	_ = a.Store.AddAudit(res.Actor, "vault.test", res.Target, fmt.Sprintf("ok=%t %s", res.OK, res.Error))
	if sameVault(saved, tested, res.secretGiven) {
		lockRead := false
		for _, s := range res.Steps {
			if s.Key == "lock" && s.OK {
				lockRead = true
			}
		}
		a.updateVaultInfo(func(vi *vaultInfo) {
			vi.Test = res
			if lockRead {
				vi.Lock, vi.LockCheckedAt, vi.LockError = res.Lock, res.At, ""
			}
		})
	}
	return res
}

// RecoveryKey returns the recovery key file: the restic password and how to
// read the copies without pgCellar. It is generated when no copy ran yet.
// Every download is audited.
func (a *App) RecoveryKey(actor string) (string, []byte, error) {
	cfg := a.Config()
	if !cfg.Vault.Enabled {
		return "", nil, &ConflictError{Msg: ErrVaultOff.Error()}
	}
	p, err := a.ensureVaultPassword(cfg)
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", nil, err
	}
	pw := strings.TrimSpace(string(raw))
	v := cfg.Vault
	now := a.now()
	var b strings.Builder
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "pgCellar off-site copy - RECOVERY KEY\n")
	fmt.Fprintf(&b, "=====================================\n\n")
	fmt.Fprintf(&b, "Keep this file offline: a password manager, a safe, a printed copy.\n")
	fmt.Fprintf(&b, "Without it, the off-site copies cannot be read: not by you, not by\n")
	fmt.Fprintf(&b, "dbfactory, not by the storage provider. Whoever holds it, plus the S3\n")
	fmt.Fprintf(&b, "credentials, can read every copy.\n\n")
	fmt.Fprintf(&b, "Instance:   %s\n", firstNonEmpty(cfg.BaseURL, host))
	fmt.Fprintf(&b, "Downloaded: %s by %s\n\n", now.Format("2006-01-02 15:04 MST"), actor)
	fmt.Fprintf(&b, "restic password (one line):\n\n    %s\n\n", pw)
	fmt.Fprintf(&b, "Bucket:   %s (endpoint %s, region %s)\n", v.Bucket, v.Endpoint, v.EffectiveRegion())
	fmt.Fprintf(&b, "Folder:   %s/\n", v.EffectivePrefix())
	fmt.Fprintf(&b, "Repositories, one per server:\n")
	for _, s := range cfg.Servers {
		if cfg.VaultFor(s) {
			fmt.Fprintf(&b, "    %-20s %s\n", s.Name, vault.Repo(v.Endpoint, v.Bucket, v.EffectivePrefix(), s.Name))
		}
	}
	fmt.Fprintf(&b, "\nRead a copy without pgCellar (restic 0.17 or later):\n\n")
	fmt.Fprintf(&b, "    export AWS_ACCESS_KEY_ID=<access key of the S3 user>\n")
	fmt.Fprintf(&b, "    export AWS_SECRET_ACCESS_KEY=<its secret key>\n")
	fmt.Fprintf(&b, "    printf '%%s\\n' '<the password above>' > restic-password && chmod 600 restic-password\n")
	fmt.Fprintf(&b, "    restic -r <repository> --password-file restic-password -o s3.region=%s snapshots\n", v.EffectiveRegion())
	fmt.Fprintf(&b, "    restic -r <repository> --password-file restic-password -o s3.region=%s restore latest --target /restore\n\n", v.EffectiveRegion())
	fmt.Fprintf(&b, "The restore gives back the Barman directory of the server (base backups and\n")
	fmt.Fprintf(&b, "WAL files) and its barman.d file, under their original paths below /restore.\n\n")
	fmt.Fprintf(&b, "Give the copies to a new pgCellar: write the password above, alone on one\n")
	fmt.Fprintf(&b, "line, into %s (mode 0600) BEFORE the first copy,\n", v.EffectivePasswordFile(cfg.DataDir))
	fmt.Fprintf(&b, "then configure the same bucket and folder.\n")
	_ = a.Store.AddAudit(actor, "vault.key.download", "", "recovery key downloaded")
	a.updateVaultInfo(func(vi *vaultInfo) { vi.KeyDownloadedAt, vi.KeyDownloadedBy = now, actor })
	name := "pgcellar-recovery-key-" + now.Format("20060102") + ".txt"
	return name, []byte(b.String()), nil
}

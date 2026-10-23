package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestVaultDefaults(t *testing.T) {
	cfg, path := loadYAML(t, minimalYAML)
	v := cfg.Vault
	if v.Enabled || v.EffectivePrefix() != "pgcellar" || v.EffectiveRegion() != "us-east-1" || v.EffectiveWALInterval() != 15*time.Minute || v.EffectiveMaxAge() != 36*time.Hour || v.QuotaBytes() != 0 {
		t.Fatalf("defaults: %+v", v)
	}
	if got := strings.Join(v.Keep.Args(), " "); got != "--keep-daily 7 --keep-weekly 4" {
		t.Errorf("keep args = %q", got)
	}
	if v.Keep.Text() != "7 daily, 4 weekly" {
		t.Errorf("keep text = %q", v.Keep.Text())
	}
	if cfg.VaultFor(cfg.Servers[0]) {
		t.Error("a disabled vault copies nothing")
	}
	if v.EffectivePasswordFile("/var/lib/pgcellar") != "/var/lib/pgcellar/vault/restic-password" || v.EffectiveSecretKeyFile("/d") != "/d/vault/s3-secret-key" {
		t.Error("default files")
	}
	if cfg.Drills.EffectiveSource() != "local" {
		t.Error("drills.source defaults to local")
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "vault") || strings.Contains(string(raw), "source:") {
		t.Errorf("Save wrote vault keys that were absent:\n%s", raw)
	}
}

const vaultYAML = `vault:
  enabled: true
  endpoint: https://s3.gra.io.cloud.ovh.net
  region: gra
  bucket: acme-pgcellar
  prefix: cellar/prod
  access_key: AKIAEXAMPLE
  secret_key_file: /etc/pgcellar/s3-secret
  quota: 500GB
  wal_interval: 5m
  max_age: 48h
  keep:
    daily: 14
    weekly: 0
    monthly: 6
`

func TestVaultValues(t *testing.T) {
	cfg, path := loadYAML(t, minimalYAML+vaultYAML+"drills:\n  source: vault\n")
	v := cfg.Vault
	if !v.Enabled || v.EffectivePrefix() != "cellar/prod" || v.EffectiveRegion() != "gra" || v.EffectiveWALInterval() != 5*time.Minute || v.EffectiveMaxAge() != 48*time.Hour {
		t.Fatalf("values: %+v", v)
	}
	if v.QuotaBytes() != 500_000_000_000 {
		t.Errorf("quota = %d", v.QuotaBytes())
	}
	if got := strings.Join(v.Keep.Args(), " "); got != "--keep-daily 14 --keep-monthly 6" {
		t.Errorf("keep args = %q", got)
	}
	if !cfg.VaultFor(cfg.Servers[0]) {
		t.Error("a server without vault: false is copied")
	}
	off := cfg.Servers[0]
	off.Vault = Ptr(false)
	if cfg.VaultFor(off) {
		t.Error("vault: false is respected")
	}
	for _, p := range Validate(cfg) {
		if strings.Contains(p, "vault") || strings.Contains(p, "drills") {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	// A round trip keeps what was written and nothing more.
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Vault.Keep.Args()[1] != "14" || *again.Vault.Keep.Weekly != 0 || again.Vault.Quota != "500GB" {
		t.Errorf("round trip: %+v", again.Vault)
	}
}

func TestVaultValidation(t *testing.T) {
	cfg, _ := loadYAML(t, minimalYAML+`vault:
  enabled: true
  endpoint: s3.gra.io.cloud.ovh.net
  bucket: Bad_Bucket
  prefix: "a//b"
  secret_key_file: relative/file
  quota: lots
  wal_interval: 10s
  keep:
    daily: 0
    weekly: 0
drills:
  source: tape
`)
	got := strings.Join(Validate(cfg), "\n")
	for _, want := range []string{"vault.endpoint", "vault.bucket", "vault.prefix", "vault.secret_key_file", "vault.quota", "vault.wal_interval", "vault.keep keeps nothing", "vault.access_key", `drills.source "tape"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	off, _ := loadYAML(t, minimalYAML+"drills:\n  source: vault\n")
	if !strings.Contains(strings.Join(Validate(off), "\n"), "vault is not enabled") {
		t.Error("drills.source vault needs the vault")
	}
	// A disabled block may be incomplete: it is a draft.
	draft, _ := loadYAML(t, minimalYAML+"vault:\n  bucket: x\n")
	for _, p := range Validate(draft) {
		if strings.Contains(p, "vault") {
			t.Errorf("draft refused: %s", p)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"": 0, "0": 0, "1024": 1024, "25GB": 25e9, "25 Go": 25e9, "2TB": 2e12, "2 To": 2e12,
		"100GiB": 100 << 30, "1.5TB": 1.5e12, "500 MB": 5e8, "1 TiB": 1 << 40,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"lots", "12 parsecs", "-3GB", "GB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
}

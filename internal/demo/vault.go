package demo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// The fictional off-site copy of Northwind: an OVHcloud bucket in
// Gravelines with Object Lock (30 days), 500 GB bought, every server copied
// but the staging one. Turned on twelve days before the instance started.
const (
	vaultEndpoint = "https://s3.gra.io.cloud.ovh.net"
	vaultBucket   = "northwind-pgcellar"
	vaultAge      = 12 * 24 * time.Hour
	gb            = int64(1_000_000_000)
)

func vaultConfig() config.Vault {
	return config.Vault{Enabled: true, Endpoint: vaultEndpoint, Region: "gra", Bucket: vaultBucket,
		AccessKey: "5f2a9c4e7b1d4e0f8a3c6b9d2e7f1a4c", Quota: "500GB"}
}

// vaultUsage is what each copied server takes in the bucket.
var vaultUsage = map[string]vault.Usage{
	ErpProd:   {CurrentBytes: 55_100_000_000, NoncurrentBytes: 3_300_000_000, Objects: 3712, Versions: true},
	CrmProd:   {CurrentBytes: 12_400_000_000, NoncurrentBytes: 700_000_000, Objects: 941, Versions: true},
	Analytics: {CurrentBytes: 171_200_000_000, NoncurrentBytes: 5_700_000_000, Objects: 10944, Versions: true},
	Legacy9:   {CurrentBytes: 4_100_000_000, NoncurrentBytes: 100_000_000, Objects: 287, Versions: true},
}

// copied reports whether the server is in the off-site copy (not staging).
func copied(s server) bool { return s.Name != ShopStaging }

// seedVault writes what the instance "knows" about the bucket, and the files
// a real instance would hold (a password that opens nothing, a secret key
// that is not one); the pages only look whether they exist.
func (d *Demo) seedVault(now time.Time) error {
	cfg := d.App.Config()
	for _, f := range []string{cfg.Vault.EffectivePasswordFile(cfg.DataDir), cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir)} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(f); err != nil {
			if err := os.WriteFile(f, []byte("demo-not-a-secret\n"), 0o600); err != nil {
				return err
			}
		}
	}
	turnedOn := d.plan.start.Add(-vaultAge)
	test := &app.VaultTestResult{At: turnedOn.Add(4 * time.Minute), Actor: maria, OK: true, Target: vaultEndpoint + "/" + vaultBucket + "/pgcellar",
		Lock: vault.Lock{Enabled: true, Mode: "GOVERNANCE", Days: 30},
		Steps: []app.VaultTestStep{
			{Key: "restic", Label: "restic is installed", OK: true, Note: "restic 0.19.1 compiled with go1.26.4 on linux/amd64", Millis: 41},
			{Key: "write", Label: "Write a test object", OK: true, Note: "pgcellar/.pgcellar-test/4f1e9a20c3b7", Millis: 212},
			{Key: "read", Label: "Read it back", OK: true, Note: "52 bytes", Millis: 64},
			{Key: "delete", Label: "Remove it", OK: true, Millis: 58},
			{Key: "lock", Label: "Read the Object Lock rule", OK: true, Note: "Locked for 30 days (governance)", Millis: 49},
		}}
	d.App.SeedDemoVault(app.DemoVault{
		Lock: test.Lock, LockCheckedAt: now.Add(-3 * time.Hour).Truncate(time.Minute),
		Usage: vaultUsage, MeasuredAt: now.Add(-40 * time.Minute).Truncate(time.Minute), Test: test,
		KeyCreatedAt: turnedOn.Add(9 * time.Minute), KeyDownloadedAt: turnedOn.Add(11 * time.Minute), KeyDownloadedBy: maria,
	})
	return nil
}

// vaultCopies is the copy history: one copy after each backup of the last
// week, and the WAL pass every 15 minutes of the last 24 hours. One WAL pass
// of crm-prod failed six hours ago (a network hiccup), the next one worked.
func (p plan) vaultCopies(all []server, cats map[string][]backup, now time.Time) []store.VaultCopy {
	var out []store.VaultCopy
	fin := func(t time.Time) *time.Time { return &t }
	add := func(c store.VaultCopy) {
		f := c.StartedAt.Add(time.Duration(c.DurationMs) * time.Millisecond)
		if f.After(now) {
			return
		}
		c.FinishedAt = fin(f)
		out = append(out, c)
	}
	for _, s := range all {
		if !copied(s) {
			continue
		}
		total := vaultUsage[s.Name].CurrentBytes * 21 / 20
		for _, b := range cats[s.Name] {
			if b.Status != "DONE" || now.Sub(b.End) > 7*24*time.Hour {
				continue
			}
			sent := b.Disk * 3 / 5
			ms := sent/(45<<20)*1000 + 9000
			snap := fmt.Sprintf("%08x", uint32(b.End.Unix())^uint32(len(s.Name)*2654435761))
			det, _ := json.Marshal(map[string]any{"journal": fmt.Sprintf("restic backup of /var/lib/barman/%s and /etc/barman.d/%s.conf\nsnapshot %s: %s added", s.Name, s.Name, snap, humanBytes(sent))})
			add(store.VaultCopy{Server: s.Name, Trigger: app.VaultTriggerBackup, Actor: app.SchedulerActor, StartedAt: b.End.Add(20 * time.Second),
				OK: true, SnapshotID: snap + "a1c3e5f7091b2d4f6a8c0e1f3a5c7e9b1d3f5a7c9e1b3d5f7a9c", BytesAdded: sent, BytesTotal: total, FilesNew: 1400 + int64(len(s.Name))*13,
				DurationMs: ms, Details: string(det)})
		}
		start := now.Add(-24 * time.Hour).Truncate(15 * time.Minute)
		for t := start; t.Before(now); t = t.Add(15 * time.Minute) {
			sent := s.SegSize * int64(15*time.Minute/s.SegEvery)
			c := store.VaultCopy{Server: s.Name, Trigger: app.VaultTriggerWAL, Actor: "system", StartedAt: t.Add(time.Duration(len(s.Name)) * 7 * time.Second),
				OK: true, BytesAdded: sent + 3100, BytesTotal: total, FilesNew: int64(15 * time.Minute / s.SegEvery), DurationMs: 2400 + sent/(30<<20)*1000}
			if sent > 0 {
				c.SnapshotID = fmt.Sprintf("%08x5e7a9c1b3d5f7a9c1e3b5d7f9a1c3e5b7d9f1a3c5e7b9d1f3a5c7e9b1d3f5a", uint32(t.Unix())+uint32(len(s.Name)))
			} else {
				c.Details = `{"unchanged":true}`
				c.BytesAdded = 0
			}
			if s.Name == CrmProd && now.Sub(t) > 6*time.Hour && now.Sub(t) <= 6*time.Hour+15*time.Minute {
				c.OK, c.SnapshotID, c.BytesAdded, c.FilesNew, c.DurationMs = false, "", 0, 0, 30_400
				c.Error = "the S3 endpoint cannot be reached from this host (restic said: Fatal: unable to open repository at s3:https://s3.gra.io.cloud.ovh.net/northwind-pgcellar/pgcellar/crm-prod: dial tcp 141.95.45.10:443: i/o timeout)"
			}
			add(c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	for i := range out {
		out[i].ID = int64(i + 1)
	}
	return out
}

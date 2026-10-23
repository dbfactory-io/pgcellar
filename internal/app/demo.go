package app

import (
	"time"

	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// DrillSummary is the sentence at the top of a drill report, for the fictional
// drills of the public demo.
func DrillSummary(server, backup string, at time.Time, ok bool, failedKey string, d DrillDetails) string {
	return drillSummary(server, backup, at, ok, failedKey, d)
}

// DemoVault is what the public demo shows about its fictional bucket.
type DemoVault struct {
	Lock            vault.Lock
	LockCheckedAt   time.Time
	Usage           map[string]vault.Usage // per server
	MeasuredAt      time.Time
	Test            *VaultTestResult
	KeyCreatedAt    time.Time
	KeyDownloadedAt time.Time
	KeyDownloadedBy string
}

// SeedDemoVault writes what pgCellar would have learnt about the bucket, for
// the fictional vault of the public demo.
func (a *App) SeedDemoVault(d DemoVault) {
	a.updateVaultInfo(func(vi *vaultInfo) {
		vi.Lock, vi.LockCheckedAt, vi.LockError = d.Lock, d.LockCheckedAt, ""
		vi.Test = d.Test
		vi.KeyCreatedAt, vi.KeyDownloadedAt, vi.KeyDownloadedBy = d.KeyCreatedAt, d.KeyDownloadedAt, d.KeyDownloadedBy
		vi.Usage = map[string]VaultUsage{}
		for name, u := range d.Usage {
			vi.Usage[name] = VaultUsage{Usage: u, MeasuredAt: d.MeasuredAt, Method: "s3"}
		}
	})
}

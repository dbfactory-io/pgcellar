package app

import (
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/license"
)

// A licence never blocks a backup, a restore or a drill. It limits one thing:
// adding a server beyond the quota (CreateServer).

type licenseCache struct {
	mu     sync.Mutex
	key    string
	file   string
	at     time.Time
	status license.Status
	count  int
	countT time.Time
}

// LicenseInfo is the licence as the interface and the API show it.
type LicenseInfo struct {
	license.Status
	Max    int    `json:"servers_max"` // servers that can be held now, 0 = unlimited
	Used   int    `json:"servers_used"`
	Footer string `json:"footer"`
	Banner string `json:"banner,omitempty"`
}

func (a *App) licensePub() ed25519.PublicKey {
	if a.LicensePublicKey != nil {
		return a.LicensePublicKey
	}
	return license.PublicKey()
}

// License returns the status of the licence named in pgcellar.yaml, at the
// application clock. A key file that cannot be read counts as an invalid key.
func (a *App) License() license.Status {
	if a.Dev { // the demo is not limited
		return license.Status{State: license.StateValid, Edition: license.Business, Licensee: "development", Servers: 0}
	}
	cfg := a.Config()
	lc := &a.lic
	lc.mu.Lock()
	defer lc.mu.Unlock()
	now := a.now()
	if lc.at.IsZero() || cfg.License.Key != lc.key || cfg.License.File != lc.file || now.Sub(lc.at) > 10*time.Second || now.Before(lc.at) {
		text, err := license.KeyText(cfg.License.Key, cfg.License.File)
		if err != nil {
			lc.status = license.Status{State: license.StateInvalid, Edition: license.Free, Servers: license.FreeServers, Message: err.Error()}
		} else {
			lc.status = license.Evaluate(text, a.licensePub(), now)
		}
		lc.key, lc.file, lc.at = cfg.License.Key, cfg.License.File, now
	}
	return lc.status
}

// serverCount is the number of servers pgCellar holds: the ones of barman.d,
// pgcellar.yaml and the status cache, as the Servers page lists them.
func (a *App) serverCount() int {
	lc := &a.lic
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if !lc.countT.IsZero() && time.Since(lc.countT) < 5*time.Second {
		return lc.count
	}
	files, _ := barman.LoadServers(a.barmanConfigDir())
	lc.count, lc.countT = len(a.serverNameSet(files, a.Config())), time.Now()
	return lc.count
}

// invalidateServerCount makes the next count read the directory again.
func (a *App) invalidateServerCount() {
	a.lic.mu.Lock()
	a.lic.countT = time.Time{}
	a.lic.mu.Unlock()
}

// LicenseInfo describes the licence and how much of it is used.
func (a *App) LicenseInfo() LicenseInfo {
	st := a.License()
	n := a.serverCount()
	return LicenseInfo{Status: st, Max: st.Max(), Used: n, Footer: st.Footer(n), Banner: st.Banner(n)}
}

// SetLicenseKey writes the key into pgcellar.yaml (license.key). Used by tests
// and by tools that install a licence; the file stays the source of truth.
func (a *App) SetLicenseKey(key string) error {
	err := a.mutateConfig(func(cfg *config.Config) error { cfg.License.Key = key; cfg.License.File = ""; return nil })
	a.lic.mu.Lock()
	a.lic.at = time.Time{}
	a.lic.mu.Unlock()
	return err
}

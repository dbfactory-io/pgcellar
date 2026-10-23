package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// A drill from the vault proves the off-site copy, not only the local one: it
// fetches the latest restic snapshot of the server into
// recoveries.dir/<server>/vault-<ts>/ (the backup catalogue first, then the
// chosen backup, its parents and the WAL files, never the other backups),
// writes a Barman configuration that reads that copy, rebuilds its WAL
// catalogue, and then runs the same drill as always with `barman -c`.

// vaultReserve marks server busy for restic work other than a copy (a drill
// from the vault). It fails when a copy of that server runs.
func (a *App) vaultReserve(server, what string) bool {
	h := &a.vault
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	if h.busy[server] != "" {
		return false
	}
	h.busy[server] = what
	return true
}

// vaultCfgOf is the pgcellar.yaml entry of a server, or the defaults when it
// has none.
func vaultCfgOf(cfg *config.Config, name string) *config.Server {
	if cs := findCfg(cfg, name); cs != nil {
		return cs
	}
	return &config.Server{Name: name}
}

// configRunner runs Barman with another configuration file (-c).
type configRunner struct {
	inner barman.Runner
	conf  string
}

func (c configRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return c.inner.Run(ctx, append([]string{"-c", c.conf}, args...)...)
}

// vaultFetchDir is where a drill fetches the off-site copy:
// <root>/<server>/vault-<timestamp>.
func vaultFetchDir(root, server string, at time.Time) (string, error) {
	if !barman.ValidName(server) {
		return "", fmt.Errorf("refusing to build a restore path for %q", server)
	}
	return filepath.Join(root, server, "vault-"+at.Format("20060102T150405.000")), nil
}

// fetchFromVault is the first step of a drill from the vault.
func (r *drillRun) fetchFromVault() (string, error) {
	a, cfg := r.a, r.cfg
	env, err := vaultEnv(cfg)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(cfg.Vault.EffectivePasswordFile(cfg.DataDir)); err != nil {
		return "", fmt.Errorf("there is no restic password in %s: without the recovery key the off-site copy cannot be read", cfg.Vault.EffectivePasswordFile(cfg.DataDir))
	}
	dir, err := vaultFetchDir(r.root, r.server, time.Now())
	if err != nil {
		return "", err
	}
	if err := prepareRecoveryDir(dir, cfg); err != nil {
		return "", err
	}
	r.extraDirs = append(r.extraDirs, dir)
	files := filepath.Join(dir, "files")
	c := &resticCall{a: a, cfg: cfg, server: r.server, env: env, journal: &r.journal, unlimited: true}

	out, errOut, err := c.run(r.ctx, "snapshots", "--json", "--host", vault.Host, "--latest", "1")
	if err != nil {
		return "", resticError(err, errOut)
	}
	snaps, err := vault.ParseSnapshots(out)
	if err != nil {
		return "", err
	}
	if len(snaps) == 0 {
		return "", fmt.Errorf("the vault holds no copy of %s yet: run \"Copy now\" first", r.server)
	}
	snap := snaps[len(snaps)-1]
	srcDir, confPath := "", ""
	for _, p := range snap.Paths {
		switch {
		case strings.HasSuffix(p, ".conf"):
			confPath = p
		case filepath.Base(p) == r.server:
			srcDir = p
		}
	}
	if srcDir == "" {
		return "", fmt.Errorf("the snapshot %s does not hold the Barman directory of %s", shortID(snap.ID), r.server)
	}
	short := firstNonEmpty(snap.ShortID, shortID(snap.ID))
	r.det.VaultSnapshot, r.det.VaultSnapshotTime = short, snap.Time

	// The backup catalogue first: it says which backup to take.
	if _, errOut, err := c.run(r.ctx, "restore", snap.ID, "--target", files, "--include", srcDir+"/meta", "--include", srcDir+"/base/*/backup.info"); err != nil {
		return "", resticError(err, errOut)
	}
	home := filepath.Join(files, filepath.Dir(srcDir))
	infos, _, err := barman.ReadBackupInfos(home, r.server)
	if err != nil {
		return "", fmt.Errorf("the off-site copy has no readable backup catalogue: %w", err)
	}
	byID := map[string]barman.BackupInfo{}
	for _, b := range infos {
		byID[b.ID] = b
	}
	chosen := r.in.BackupID
	if chosen != "" {
		b, ok := byID[chosen]
		switch {
		case !ok:
			return "", fmt.Errorf("backup %s is not in the off-site copy of %s", chosen, snap.Time.Local().Format("2006-01-02 15:04"))
		case b.Status != "DONE":
			return "", fmt.Errorf("backup %s is %s in the off-site copy: only a complete backup (DONE) can be restored", chosen, b.Status)
		}
	} else {
		for i := len(infos) - 1; i >= 0; i-- {
			if infos[i].Status == "DONE" {
				chosen = infos[i].ID
				break
			}
		}
		if chosen == "" {
			return "", fmt.Errorf("the off-site copy of %s holds no complete backup (DONE)", r.server)
		}
	}
	keep := map[string]bool{}
	for id := chosen; id != "" && !keep[id]; id = byID[id].ParentID {
		keep[id] = true // an incremental backup needs its parents
	}
	r.in.BackupID = chosen

	// Then everything but the other backups: the chosen one, its parents, the WAL files.
	args := []string{"restore", snap.ID, "--target", files}
	for _, b := range infos {
		if !keep[b.ID] {
			args = append(args, "--exclude", srcDir+"/base/"+b.ID)
		}
	}
	if _, errOut, err := c.run(r.ctx, args...); err != nil {
		return "", resticError(err, errOut)
	}
	r.det.FetchedBytes = dirSize(files)

	g, _ := barman.LoadGlobal(cfg.Barman.ConfigFile)
	var srv *barman.Server
	if confPath != "" {
		if list, err := barman.LoadServers(filepath.Dir(filepath.Join(files, confPath))); err == nil {
			srv = findFile(list, r.server)
		}
	}
	owner := cfg.Barman.User
	if owner == "" && g != nil {
		owner = g.BarmanUser
	}
	if owner == "" {
		if u, err := user.Current(); err == nil {
			owner = u.Username
		}
	}
	conf, err := barman.WriteCopyConfig(filepath.Join(dir, "barman"), home, owner, g, srv, r.server)
	if err != nil {
		return "", fmt.Errorf("cannot write the Barman configuration of the copy: %w", err)
	}
	chownBarman(cfg, filepath.Join(dir, "barman"), conf, filepath.Join(dir, "barman", "barman.d"), filepath.Join(dir, "barman", "locks"))
	r.barmanConf = conf
	eng := barman.NewEngine(&journalRunner{r: r, inner: configRunner{inner: a.Runner, conf: conf}})
	if _, err := eng.RebuildXlogDB(r.ctx, r.server); err != nil {
		var re *barman.RunError
		if errors.As(err, &re) || r.ctx.Err() == nil {
			return "", fmt.Errorf("barman rebuild-xlogdb on the fetched copy failed: %w", err)
		}
		return "", err
	}
	return fmt.Sprintf("backup %s from the copy of %s (snapshot %s), %s fetched", chosen, snap.Time.Local().Format("2 Jan 15:04"), short, humanBytes(r.det.FetchedBytes)), nil
}

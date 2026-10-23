package app

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/alerts"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
)

// alertVault decides what the off-site copy is worth a message for:
//   - a failed copy, once per run of failures: at the first failure of a copy
//     that follows a backup or a click, or at the second failure in a row of
//     the periodic WAL pass (a network hiccup heals by itself);
//   - the quota, at 80 % once, and at 100 % (repeated every
//     alerts.repeat_every while it lasts).
//
// A copy older than vault.max_age needs no message of its own: the server
// turns orange and the level change is announced.
func (a *App) alertVault(st *alertState, cfg *config.Config, now time.Time) []alerts.Message {
	if st.VaultFailing == nil {
		st.VaultFailing = map[string]bool{}
	}
	copies, err := a.Store.ListVaultCopies("", 300)
	if err != nil {
		return nil
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].ID < copies[j].ID })
	if !st.Init { // first look: what happened before is not news
		for _, c := range copies {
			if c.FinishedAt != nil {
				st.LastVaultID = max(st.LastVaultID, c.ID)
			}
		}
		return nil
	}
	var out []alerts.Message
	prevFailed := map[string]bool{} // outcome of the previous copy of each server
	for _, c := range copies {
		if c.ID > st.LastVaultID && c.FinishedAt == nil {
			break // copies run one at a time: the rest comes later
		}
		if c.ID <= st.LastVaultID {
			prevFailed[c.Server] = !c.OK
			continue
		}
		st.LastVaultID = c.ID
		interrupted := strings.HasPrefix(c.Error, "interrupted:")
		switch {
		case c.OK:
			delete(st.VaultFailing, c.Server)
		case interrupted:
		case st.VaultFailing[c.Server]:
		case c.Trigger != VaultTriggerWAL || prevFailed[c.Server]:
			st.VaultFailing[c.Server] = true
			out = append(out, a.vaultFailedMessage(c, cfg))
		}
		if !interrupted {
			prevFailed[c.Server] = !c.OK
		}
	}
	if m := a.alertVaultQuota(st, cfg, now); m != nil {
		out = append(out, *m)
	}
	return out
}

func (a *App) vaultFailedMessage(c store.VaultCopy, cfg *config.Config) alerts.Message {
	what := map[string]string{VaultTriggerBackup: "copy that follows the backup", VaultTriggerManual: "copy started by " + c.Actor, VaultTriggerWAL: "periodic copy of the WAL files"}[c.Trigger]
	if what == "" {
		what = "copy"
	}
	next := "after the next backup"
	if iv := cfg.Vault.EffectiveWALInterval(); iv > 0 {
		next = "within " + HumanDuration(iv) + " and after the next backup"
	}
	return alerts.Message{
		Kind: alerts.KindVaultFailed, Server: c.Server, Level: alerts.LevelWarn,
		Title: fmt.Sprintf("Off-site copy of %s failed", c.Server),
		Lines: []string{
			fmt.Sprintf("The %s (copy #%d) failed: %s", what, c.ID, shorten(c.Error, 300)),
			"The backups on this host are not affected. pgCellar tries again " + next + ".",
		},
		Action: "Open the Vault page, run \"Test connection\" and fix what it says, then click \"Copy now\" on the server.",
		URL:    a.alertURL("/vault"),
		At:     a.now(),
	}
}

// alertVaultQuota compares what the vault takes with vault.quota.
func (a *App) alertVaultQuota(st *alertState, cfg *config.Config, now time.Time) *alerts.Message {
	quota := cfg.Vault.QuotaBytes()
	if !cfg.Vault.Enabled || quota <= 0 {
		st.VaultQuota = ""
		return nil
	}
	var used int64
	for _, u := range a.currentVaultInfo().Usage {
		used += u.Total()
	}
	pct, level := quotaLevel(used, quota)
	line := fmt.Sprintf("%s of %s used (%d %%).", FormatDecimalSize(used), FormatDecimalSize(quota), int(math.Round(pct)))
	repeat := cfg.Alerts.EffectiveRepeatEvery()
	switch level {
	case LevelFail:
		if st.VaultQuota == LevelFail && (repeat <= 0 || now.Sub(st.VaultQuotaAt) < repeat) {
			return nil
		}
		st.VaultQuota, st.VaultQuotaAt = LevelFail, now
		return &alerts.Message{
			Kind: alerts.KindVaultQuota, Level: alerts.LevelFail,
			Title:  "Off-site storage is over its quota",
			Lines:  []string{line, "Copies go on: pgCellar never stops a backup or a copy because of the quota."},
			Action: "Take a bigger storage pack, or keep fewer snapshots (vault.keep), then wait for the next daily clean-up.",
			URL:    a.alertURL("/vault"), At: a.now(),
		}
	case LevelWarn:
		if st.VaultQuota != "" {
			if st.VaultQuota == LevelFail {
				st.VaultQuota = LevelWarn // back under 100 %: no message, but 100 % is news again
			}
			return nil
		}
		st.VaultQuota, st.VaultQuotaAt = LevelWarn, now
		return &alerts.Message{
			Kind: alerts.KindVaultQuotaWarn, Level: alerts.LevelWarn,
			Title:  "Off-site storage at 80 % of its quota",
			Lines:  []string{line},
			Action: "Plan a bigger storage pack, or keep fewer snapshots (vault.keep). Copies go on even over the quota.",
			URL:    a.alertURL("/vault"), At: a.now(),
		}
	}
	if pct < 75 {
		st.VaultQuota = ""
	}
	return nil
}

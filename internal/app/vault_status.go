package app

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/store"
	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// VaultCopyView is one off-site copy as the pages and the API show it.
type VaultCopyView struct {
	ID          int64      `json:"id"`
	Server      string     `json:"server"`
	Trigger     string     `json:"trigger"` // backup, wal, manual
	Actor       string     `json:"actor"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Running     bool       `json:"running"`
	OK          bool       `json:"ok"`
	SnapshotID  string     `json:"snapshot_id,omitempty"`
	BytesAdded  int64      `json:"bytes_added"` // uploaded by this copy
	BytesTotal  int64      `json:"bytes_total"` // what the copy covers
	FilesNew    int64      `json:"files_new"`
	DurationMs  int64      `json:"duration_ms"`
	Error       string     `json:"error,omitempty"`
	Warnings    []string   `json:"warnings,omitempty"`
	Unchanged   bool       `json:"unchanged"`
	Initialised bool       `json:"initialised"`
	Pruned      bool       `json:"pruned"`
	PruneError  string     `json:"prune_error,omitempty"`
	Journal     string     `json:"journal,omitempty"`
}

// Duration is the time the copy took.
func (v VaultCopyView) Duration() time.Duration {
	return time.Duration(v.DurationMs) * time.Millisecond
}

func vaultCopyView(c store.VaultCopy) VaultCopyView {
	v := VaultCopyView{ID: c.ID, Server: c.Server, Trigger: c.Trigger, Actor: c.Actor, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt,
		Running: c.FinishedAt == nil, OK: c.OK, SnapshotID: c.SnapshotID, BytesAdded: c.BytesAdded, BytesTotal: c.BytesTotal,
		FilesNew: c.FilesNew, DurationMs: c.DurationMs, Error: c.Error}
	var det vaultCopyDetails
	if json.Unmarshal([]byte(c.Details), &det) == nil {
		v.Warnings, v.Unchanged, v.Initialised, v.Pruned, v.PruneError, v.Journal = det.Warnings, det.Unchanged, det.Initialised, det.Pruned, det.PruneError, det.Journal
	}
	return v
}

// ListVaultCopies lists the copies of a server ("" = all), newest first,
// without their journal unless withJournal.
func (a *App) ListVaultCopies(server string, limit int, withJournal bool) ([]VaultCopyView, error) {
	list, err := a.Store.ListVaultCopies(server, limit)
	if err != nil {
		return nil, err
	}
	out := make([]VaultCopyView, 0, len(list))
	for _, c := range list {
		v := vaultCopyView(c)
		if !withJournal {
			v.Journal = ""
		}
		out = append(out, v)
	}
	return out, nil
}

// GetVaultCopy returns one copy with its journal.
func (a *App) GetVaultCopy(id int64) (*VaultCopyView, error) {
	c, err := a.Store.GetVaultCopy(id)
	if err != nil {
		return nil, ErrNotFound
	}
	v := vaultCopyView(*c)
	return &v, nil
}

// VaultServerStatus is the off-site copy of one server, computed when a
// state is read (from SQLite only, never from S3).
type VaultServerStatus struct {
	Copied           bool           `json:"copied"` // the vault is on and the server is not vault: false
	Running          bool           `json:"running"`
	Queued           bool           `json:"queued"`
	Last             *VaultCopyView `json:"last,omitempty"`
	LastOKAt         time.Time      `json:"last_ok_at,omitzero"`
	LastOKAgeSeconds *int64         `json:"last_ok_age_seconds,omitempty"`
	LastOKBytes      int64          `json:"last_ok_bytes"` // what the last good copy covers
	FailStreak       int            `json:"fail_streak"`   // failed copies in a row
	UsedBytes        int64          `json:"used_bytes"`    // what the server takes in the bucket (last measure)
	UsageAt          time.Time      `json:"usage_at,omitzero"`
	Repo             string         `json:"repo,omitempty"`
	// Stale: no good copy for vault.max_age, or the last copies failed. The
	// server turns orange.
	Stale bool   `json:"stale"`
	Why   string `json:"why,omitempty"`
	Text  string `json:"text"`  // "Off-site copy: 12 min ago ✔"
	Emoji string `json:"emoji"` // ✅ ⚠️ ❌ ⏳ 💤 for the lists
}

// VaultStatusOf computes the off-site copy status of a server, nil when the
// vault is off.
func (a *App) VaultStatusOf(name string, now time.Time) *VaultServerStatus {
	cfg := a.Config()
	if !cfg.Vault.Enabled {
		return nil
	}
	vs := &VaultServerStatus{Emoji: "💤", Text: "Not copied off-site (vault: false)"}
	cs := findCfg(cfg, name)
	if cs == nil || !cfg.VaultFor(*cs) {
		return vs
	}
	vs.Copied = true
	vs.Repo = vault.Repo(cfg.Vault.Endpoint, cfg.Vault.Bucket, cfg.Vault.EffectivePrefix(), name)
	running, _, queue := a.vaultActivity()
	vs.Running = running[name] == "copy"
	for _, q := range queue {
		if q == name {
			vs.Queued = true
		}
	}
	if u, ok := a.currentVaultInfo().Usage[name]; ok {
		vs.UsedBytes, vs.UsageAt = u.Total(), u.MeasuredAt
	}
	recent, err := a.Store.ListVaultCopies(name, vaultFailStreak+1)
	if err != nil {
		return vs
	}
	var done []store.VaultCopy
	for _, c := range recent {
		if c.FinishedAt != nil {
			done = append(done, c)
		}
	}
	if len(done) > 0 {
		v := vaultCopyView(done[0])
		v.Journal = ""
		vs.Last = &v
	}
	for _, c := range done {
		if c.OK {
			break
		}
		vs.FailStreak++
	}
	if ok, _ := a.Store.LastOKVaultCopy(name); ok != nil && ok.FinishedAt != nil {
		vs.LastOKAt, vs.LastOKBytes = *ok.FinishedAt, ok.BytesTotal
		age := int64(now.Sub(*ok.FinishedAt).Seconds())
		vs.LastOKAgeSeconds = &age
	}
	maxAge := cfg.Vault.EffectiveMaxAge()
	switch {
	case vs.Last == nil && vs.Running:
		vs.Text, vs.Emoji = "First off-site copy running", "⏳"
	case vs.Last == nil:
		vs.Text, vs.Emoji = "No off-site copy yet", "⏳"
	case vs.Last.OK:
		vs.Text, vs.Emoji = "Off-site copy: "+shortAgo(now.Sub(*vs.Last.FinishedAt))+" ✔", "✅"
	default:
		vs.Text, vs.Emoji = "Off-site copy failed "+shortAgo(now.Sub(*vs.Last.FinishedAt))+" ✘", "⚠️"
	}
	switch {
	case vs.FailStreak >= vaultFailStreak:
		vs.Stale = true
		vs.Why = fmt.Sprintf("The last %d off-site copies failed: %s", vs.FailStreak, firstSentence(vs.Last.Error))
	case vs.Last != nil && !vs.LastOKAt.IsZero() && now.Sub(vs.LastOKAt) > maxAge:
		vs.Stale = true
		vs.Why = fmt.Sprintf("No off-site copy succeeded for %s (vault.max_age is %s).", HumanDuration(now.Sub(vs.LastOKAt)), HumanDuration(maxAge))
	case vs.Last != nil && vs.LastOKAt.IsZero():
		if first := a.firstVaultAttempt(name); !first.IsZero() && now.Sub(first) > maxAge {
			vs.Stale = true
			vs.Why = fmt.Sprintf("No off-site copy ever succeeded, the first attempt was %s ago.", HumanDuration(now.Sub(first)))
		}
	}
	if vs.Stale {
		vs.Emoji = "❌"
	}
	return vs
}

func (a *App) firstVaultAttempt(name string) time.Time {
	list, err := a.Store.ListVaultCopies(name, 1000)
	if err != nil || len(list) == 0 {
		return time.Time{}
	}
	return list[len(list)-1].StartedAt
}

// applyVault adds the off-site copy status to a state read from the cache
// and turns the server orange when its copies are late or failing.
func (a *App) applyVault(st *ServerState, now time.Time) {
	vs := a.VaultStatusOf(st.Name, now)
	st.Vault = vs
	if vs != nil && vs.Stale && vs.Why != "" && !strings.Contains(strings.Join(st.Problems, "\n"), vs.Why) {
		st.Problems = append(append([]string(nil), st.Problems...), vs.Why)
		if st.Level == LevelOK {
			st.Level = LevelWarn
		}
	}
}

// ---- the whole vault ----

// VaultServerRow is one line of the Vault page.
type VaultServerRow struct {
	Name   string             `json:"name"`
	Status *VaultServerStatus `json:"status"`
}

// VaultView is the Vault page and GET /api/v1/vault.
type VaultView struct {
	Config          config.Vault     `json:"config"`
	Enabled         bool             `json:"enabled"`
	SecretKeySet    bool             `json:"secret_key_set"`
	KeyExists       bool             `json:"key_exists"`
	KeyCreatedAt    time.Time        `json:"key_created_at,omitzero"`
	KeyDownloadedAt time.Time        `json:"key_downloaded_at,omitzero"`
	KeyDownloadedBy string           `json:"key_downloaded_by,omitempty"`
	Lock            vault.Lock       `json:"lock"`
	LockCheckedAt   time.Time        `json:"lock_checked_at,omitzero"`
	LockError       string           `json:"lock_error,omitempty"`
	LockText        string           `json:"lock_text"`
	QuotaBytes      int64            `json:"quota_bytes"`
	UsedBytes       int64            `json:"used_bytes"`
	NoncurrentBytes int64            `json:"noncurrent_bytes"` // part of used_bytes kept by versioning or Object Lock
	UsedPercent     float64          `json:"used_percent"`
	QuotaLevel      string           `json:"quota_level"` // "" (no quota), ok, warn (80 %), fail (100 %)
	LastTest        *VaultTestResult `json:"last_test,omitempty"`
	Servers         []VaultServerRow `json:"servers"`
	Queue           []string         `json:"queue"`
	DrillSource     string           `json:"drill_source"`
}

// UsedPct is the used share rounded for the gauge, capped at 100.
func (v VaultView) UsedPct() int { return int(math.Min(100, math.Round(v.UsedPercent))) }

// LockLine is "Locked for 30 days (governance)" or "Not locked".
func lockLine(l vault.Lock, checked time.Time, lerr string) string {
	switch {
	case l.Locked():
		return fmt.Sprintf("Locked for %d days (%s)", l.Days, strings.ToLower(l.Mode))
	case l.Enabled:
		return "Object Lock is on, but without a default retention: not locked"
	case lerr != "" && checked.IsZero():
		return "Unknown: " + lerr
	case checked.IsZero():
		return "Not checked yet"
	}
	return "Not locked"
}

// quotaLevel compares the use with the quota.
func quotaLevel(used, quota int64) (pct float64, level string) {
	if quota <= 0 {
		return 0, ""
	}
	pct = 100 * float64(used) / float64(quota)
	switch {
	case pct >= 100:
		return pct, LevelFail
	case pct >= 80:
		return pct, LevelWarn
	}
	return pct, LevelOK
}

// VaultStatus is everything about the vault, read from the configuration
// and SQLite (never from S3).
func (a *App) VaultStatus() VaultView {
	cfg := a.Config()
	now := a.now()
	vi := a.currentVaultInfo()
	v := VaultView{Config: cfg.Vault, Enabled: cfg.Vault.Enabled, Lock: vi.Lock, LockCheckedAt: vi.LockCheckedAt, LockError: vi.LockError,
		LastTest: vi.Test, KeyCreatedAt: vi.KeyCreatedAt, KeyDownloadedAt: vi.KeyDownloadedAt, KeyDownloadedBy: vi.KeyDownloadedBy,
		QuotaBytes: cfg.Vault.QuotaBytes(), Servers: []VaultServerRow{}, DrillSource: cfg.Drills.EffectiveSource()}
	v.LockText = lockLine(vi.Lock, vi.LockCheckedAt, vi.LockError)
	if _, err := os.Stat(cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir)); err == nil {
		v.SecretKeySet = true
	}
	if _, err := os.Stat(cfg.Vault.EffectivePasswordFile(cfg.DataDir)); err == nil {
		v.KeyExists = true
	}
	for _, u := range vi.Usage {
		v.UsedBytes += u.Total()
		v.NoncurrentBytes += u.NoncurrentBytes
	}
	v.UsedPercent, v.QuotaLevel = quotaLevel(v.UsedBytes, v.QuotaBytes)
	_, _, v.Queue = a.vaultActivity()
	if v.Queue == nil {
		v.Queue = []string{}
	}
	if cfg.Vault.Enabled {
		for _, s := range cfg.Servers {
			v.Servers = append(v.Servers, VaultServerRow{Name: s.Name, Status: a.VaultStatusOf(s.Name, now)})
		}
	}
	return v
}

// VaultLine is the line of the Status page about the vault, nil when it is
// off.
type VaultLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// VaultSummary sums up the off-site copy for the Status page: how many
// servers have a fresh copy, the quota and the lock.
func (a *App) VaultSummary(states []ServerState) *VaultLine {
	cfg := a.Config()
	if !cfg.Vault.Enabled {
		return nil
	}
	copied, fresh := 0, 0
	level := LevelOK
	for _, st := range states {
		if st.Vault == nil || !st.Vault.Copied {
			continue
		}
		copied++
		if st.Vault.Last != nil && st.Vault.Last.OK && !st.Vault.Stale {
			fresh++
		}
		if st.Vault.Stale {
			level = LevelWarn
		}
	}
	vi := a.currentVaultInfo()
	var used int64
	for _, u := range vi.Usage {
		used += u.Total()
	}
	parts := []string{fmt.Sprintf("🔐 Off-site copy: %d of %d servers up to date", fresh, copied)}
	quota := cfg.Vault.QuotaBytes()
	pct, ql := quotaLevel(used, quota)
	switch {
	case quota > 0:
		parts = append(parts, fmt.Sprintf("%s of %s used (%d %%)", FormatDecimalSize(used), FormatDecimalSize(quota), int(math.Round(pct))))
	case used > 0:
		parts = append(parts, FormatDecimalSize(used)+" stored")
	}
	if ql == LevelFail || (ql == LevelWarn && level == LevelOK) {
		level = ql
	}
	if l := vi.Lock; l.Locked() {
		parts = append(parts, fmt.Sprintf("locked %d days", l.Days))
	} else if !vi.LockCheckedAt.IsZero() && vi.LockError == "" {
		parts = append(parts, "not locked")
	}
	return &VaultLine{Level: level, Text: strings.Join(parts, " · ")}
}

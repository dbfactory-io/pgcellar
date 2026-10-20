package store

import "time"

// Snapshot is a complete set of history rows with explicit ids. The public
// demo uses it to rebuild its fictional history from the clock: every table
// below is emptied and refilled in one transaction, sessions and settings are
// left alone.
type Snapshot struct {
	Runs       []Run
	Audit      []AuditEntry
	Drills     []Drill
	Recoveries []Recovery
	Alerts     []AlertSent
}

// ReplaceSnapshot replaces the runs, audit lines, drills, recoveries and alert
// journal by the rows of sn, all or nothing.
func (s *Store) ReplaceSnapshot(sn Snapshot) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	rollback := func(err error) error { _ = tx.Rollback(); return err }
	for _, t := range []string{"runs", "audit", "drills", "recoveries", "alerts_sent"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return rollback(err)
		}
	}
	for _, r := range sn.Runs {
		var fin, code any
		if r.FinishedAt != nil {
			fin = ts(*r.FinishedAt)
		}
		if r.ExitCode != nil {
			code = *r.ExitCode
		}
		if _, err := tx.Exec(`INSERT INTO runs (id, server, kind, started_at, finished_at, exit_code, actor, stdout_tail, stderr_tail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Server, r.Kind, ts(r.StartedAt), fin, code, r.Actor, r.StdoutTail, r.StderrTail); err != nil {
			return rollback(err)
		}
	}
	for _, e := range sn.Audit {
		if _, err := tx.Exec(`INSERT INTO audit (id, at, actor, action, target, details) VALUES (?, ?, ?, ?, ?, ?)`,
			e.ID, ts(e.At), e.Actor, e.Action, e.Target, e.Details); err != nil {
			return rollback(err)
		}
	}
	for _, d := range sn.Drills {
		ok := 0
		if d.OK {
			ok = 1
		}
		if d.Details == "" {
			d.Details = "{}"
		}
		if _, err := tx.Exec(`INSERT INTO drills (id, server, backup_id, started_at, finished_at, ok, step_failed, details, run_id, report_path) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.Server, d.BackupID, ts(d.StartedAt), ts(d.FinishedAt), ok, d.StepFailed, d.Details, d.RunID, d.ReportPath); err != nil {
			return rollback(err)
		}
	}
	for _, r := range sn.Recoveries {
		if r.CreatedAt.IsZero() {
			r.CreatedAt = time.Now()
		}
		if _, err := tx.Exec(`INSERT INTO recoveries (id, server, backup_id, target, target_time, path, archive_path, size_bytes, archive_bytes, run_id, created_at, actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Server, r.BackupID, r.Target, r.TargetTime, r.Path, r.ArchivePath, r.SizeBytes, r.ArchiveSize, r.RunID, ts(r.CreatedAt), r.Actor); err != nil {
			return rollback(err)
		}
	}
	for _, a := range sn.Alerts {
		ok := 0
		if a.OK {
			ok = 1
		}
		if _, err := tx.Exec(`INSERT INTO alerts_sent (id, at, channel, kind, server, level, text, ok, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, ts(a.At), a.Channel, a.Kind, a.Server, a.Level, a.Text, ok, a.Error); err != nil {
			return rollback(err)
		}
	}
	return tx.Commit()
}

// TrimAudit keeps the newest keep audit lines and deletes the others.
func (s *Store) TrimAudit(keep int) error {
	_, err := s.db.Exec(`DELETE FROM audit WHERE id NOT IN (SELECT id FROM audit ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

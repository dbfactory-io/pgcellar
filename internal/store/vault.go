package store

import (
	"database/sql"
	"errors"
	"time"
)

// VaultCopy is one off-site copy of a server (one restic backup), or the
// copy in progress when FinishedAt is nil.
type VaultCopy struct {
	ID         int64
	Server     string
	Trigger    string // backup, wal, manual
	Actor      string
	StartedAt  time.Time
	FinishedAt *time.Time
	OK         bool
	SnapshotID string // empty when nothing changed since the previous copy
	BytesAdded int64  // what was uploaded
	BytesTotal int64  // what the copy covers
	FilesNew   int64
	DurationMs int64
	Error      string
	Details    string // JSON owned by the application layer
}

const vaultCols = `id, server, cause, actor, started_at, finished_at, ok, snapshot_id, bytes_added, bytes_total, files_new, duration_ms, error, details`

func scanVaultCopy(sc scanner) (VaultCopy, error) {
	var c VaultCopy
	var started string
	var finished sql.NullString
	var ok int
	err := sc.Scan(&c.ID, &c.Server, &c.Trigger, &c.Actor, &started, &finished, &ok, &c.SnapshotID, &c.BytesAdded, &c.BytesTotal, &c.FilesNew, &c.DurationMs, &c.Error, &c.Details)
	c.StartedAt, c.OK = parseTS(started), ok != 0
	if finished.Valid {
		t := parseTS(finished.String)
		c.FinishedAt = &t
	}
	return c, err
}

// StartVaultCopy records a copy that starts and returns its id.
func (s *Store) StartVaultCopy(server, trigger, actor string, at time.Time) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO vault_copies (server, cause, actor, started_at) VALUES (?, ?, ?, ?)`, server, trigger, actor, ts(at))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishVaultCopy records the outcome of a copy (id, server, trigger, actor
// and start of c are ignored).
func (s *Store) FinishVaultCopy(id int64, c VaultCopy, at time.Time) error {
	if c.Details == "" {
		c.Details = "{}"
	}
	ok := 0
	if c.OK {
		ok = 1
	}
	res, err := s.db.Exec(`UPDATE vault_copies SET finished_at = ?, ok = ?, snapshot_id = ?, bytes_added = ?, bytes_total = ?, files_new = ?, duration_ms = ?, error = ?, details = ? WHERE id = ?`,
		ts(at), ok, c.SnapshotID, c.BytesAdded, c.BytesTotal, c.FilesNew, c.DurationMs, c.Error, c.Details, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetVaultCopy returns one copy.
func (s *Store) GetVaultCopy(id int64) (*VaultCopy, error) {
	c, err := scanVaultCopy(s.db.QueryRow(`SELECT `+vaultCols+` FROM vault_copies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListVaultCopies returns the copies of a server ("" = every server), newest
// first.
func (s *Store) ListVaultCopies(server string, limit int) ([]VaultCopy, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + vaultCols + ` FROM vault_copies`
	args := []any{}
	if server != "" {
		q += ` WHERE server = ?`
		args = append(args, server)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryVaultCopies(q, args...)
}

// LastOKVaultCopy returns the newest successful copy of a server, nil when
// there is none.
func (s *Store) LastOKVaultCopy(server string) (*VaultCopy, error) {
	list, err := s.queryVaultCopies(`SELECT `+vaultCols+` FROM vault_copies WHERE server = ? AND ok = 1 ORDER BY id DESC LIMIT 1`, server)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

func (s *Store) queryVaultCopies(q string, args ...any) ([]VaultCopy, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []VaultCopy
	for rows.Next() {
		c, err := scanVaultCopy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloseUnfinishedVaultCopies marks copies left open by a previous process as
// failed with the given reason.
func (s *Store) CloseUnfinishedVaultCopies(reason string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE vault_copies SET finished_at = ?, ok = 0, error = ? WHERE finished_at IS NULL`, ts(at), reason)
	return err
}

// PruneVaultCopies deletes the copies started before olderThan, except the
// newest one and the newest successful one of each server.
func (s *Store) PruneVaultCopies(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM vault_copies WHERE started_at < ?
		AND id NOT IN (SELECT max(id) FROM vault_copies GROUP BY server)
		AND id NOT IN (SELECT max(id) FROM vault_copies WHERE ok = 1 GROUP BY server)`, ts(olderThan))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

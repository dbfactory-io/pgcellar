package store

import (
	"database/sql"
	"errors"
	"time"
)

// Recovery is one finished restore: a directory (and optionally a .tar.gz
// next to it) under the recoveries directory.
type Recovery struct {
	ID          int64
	Server      string
	BackupID    string
	Target      string // latest | backup_end | time
	TargetTime  string // the --target-time given to Barman, "" otherwise
	Path        string
	ArchivePath string // "" when no archive was built
	SizeBytes   int64
	ArchiveSize int64
	RunID       int64
	CreatedAt   time.Time
	Actor       string
}

const recoveryCols = `id, server, backup_id, target, target_time, path, archive_path, size_bytes, archive_bytes, run_id, created_at, actor`

func scanRecovery(sc scanner) (Recovery, error) {
	var r Recovery
	var created string
	err := sc.Scan(&r.ID, &r.Server, &r.BackupID, &r.Target, &r.TargetTime, &r.Path, &r.ArchivePath, &r.SizeBytes, &r.ArchiveSize, &r.RunID, &created, &r.Actor)
	r.CreatedAt = parseTS(created)
	return r, err
}

// AddRecovery stores a recovery and returns its id.
func (s *Store) AddRecovery(r Recovery) (int64, error) {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.Target == "" {
		r.Target = "latest"
	}
	res, err := s.db.Exec(`INSERT INTO recoveries (server, backup_id, target, target_time, path, archive_path, size_bytes, archive_bytes, run_id, created_at, actor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Server, r.BackupID, r.Target, r.TargetTime, r.Path, r.ArchivePath, r.SizeBytes, r.ArchiveSize, r.RunID, ts(r.CreatedAt), r.Actor)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetRecovery returns one recovery, or ErrNotFound.
func (s *Store) GetRecovery(id int64) (*Recovery, error) {
	r, err := scanRecovery(s.db.QueryRow(`SELECT `+recoveryCols+` FROM recoveries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RecoveryForRun returns the recovery produced by a run, or ErrNotFound.
func (s *Store) RecoveryForRun(runID int64) (*Recovery, error) {
	r, err := scanRecovery(s.db.QueryRow(`SELECT `+recoveryCols+` FROM recoveries WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRecoveries returns the recoveries of a server (all servers when empty),
// newest first.
func (s *Store) ListRecoveries(server string) ([]Recovery, error) {
	q := `SELECT ` + recoveryCols + ` FROM recoveries`
	var args []any
	if server != "" {
		q += ` WHERE server = ?`
		args = append(args, server)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	return s.queryRecoveries(q, args...)
}

// RecoveriesOlderThan returns the recoveries created before t.
func (s *Store) RecoveriesOlderThan(t time.Time) ([]Recovery, error) {
	return s.queryRecoveries(`SELECT `+recoveryCols+` FROM recoveries WHERE created_at < ? ORDER BY id`, ts(t))
}

func (s *Store) queryRecoveries(q string, args ...any) ([]Recovery, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Recovery
	for rows.Next() {
		r, err := scanRecovery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRecovery removes the row of a recovery (not its files).
func (s *Store) DeleteRecovery(id int64) error {
	res, err := s.db.Exec(`DELETE FROM recoveries WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountRuns returns, per server, how many runs of a kind ended with exit
// code 0.
func (s *Store) CountRuns(kind string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT server, COUNT(*) FROM runs WHERE kind = ? AND exit_code = 0 GROUP BY server`, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// Package store is pgCellar's SQLite database: run history, audit log and
// sessions. Everything in it can be rebuilt or discarded; configuration lives
// in files, never here.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Run kinds.
const (
	KindBackup  = "backup"
	KindCheck   = "check"
	KindRecover = "recover"
	KindDrill   = "drill"
	KindCron    = "cron"
)

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Run is one execution of a Barman command or a drill.
type Run struct {
	ID         int64
	Server     string
	Kind       string
	StartedAt  time.Time
	FinishedAt *time.Time // nil while running
	ExitCode   *int       // nil while running
	Actor      string
	StdoutTail string
	StderrTail string
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ID      int64
	At      time.Time
	Actor   string
	Action  string
	Target  string
	Details string
}

// Session is a logged-in web session.
type Session struct {
	ID        string
	User      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

const tsLayout = time.RFC3339

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(tsLayout, s)
	return t
}

// Open opens (creating if needed) the database file at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite simple and avoids "database is locked".
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		server TEXT NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('backup','check','recover','drill','cron')),
		started_at TEXT NOT NULL,
		finished_at TEXT,
		exit_code INTEGER,
		actor TEXT NOT NULL DEFAULT '',
		stdout_tail TEXT NOT NULL DEFAULT '',
		stderr_tail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX runs_server_started ON runs (server, started_at);
	CREATE TABLE audit (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		details TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		user TEXT NOT NULL,
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	);
	CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
}

// Migrate applies pending migrations; calling it again is harmless.
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	current, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the applied schema version.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// StartRun records a run that has begun and returns its id.
func (s *Store) StartRun(server, kind, actor string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runs (server, kind, started_at, actor) VALUES (?, ?, ?, ?)`,
		server, kind, ts(time.Now()), actor)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun completes a run with its exit code and output tails.
func (s *Store) FinishRun(id int64, exitCode int, stdoutTail, stderrTail string) error {
	res, err := s.db.Exec(`UPDATE runs SET finished_at = ?, exit_code = ?, stdout_tail = ?, stderr_tail = ? WHERE id = ?`,
		ts(time.Now()), exitCode, stdoutTail, stderrTail, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const runCols = `id, server, kind, started_at, finished_at, exit_code, actor, stdout_tail, stderr_tail`

type scanner interface{ Scan(...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var started string
	var finished sql.NullString
	var code sql.NullInt64
	if err := sc.Scan(&r.ID, &r.Server, &r.Kind, &started, &finished, &code, &r.Actor, &r.StdoutTail, &r.StderrTail); err != nil {
		return r, err
	}
	r.StartedAt = parseTS(started)
	if finished.Valid {
		t := parseTS(finished.String)
		r.FinishedAt = &t
	}
	if code.Valid {
		c := int(code.Int64)
		r.ExitCode = &c
	}
	return r, nil
}

// ListRuns returns the latest runs, newest first. An empty server means all.
func (s *Store) ListRuns(server string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + runCols + ` FROM runs`
	var args []any
	if server != "" {
		q += ` WHERE server = ?`
		args = append(args, server)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRun returns the latest run of a kind for a server, or ErrNotFound.
func (s *Store) LastRun(server, kind string) (*Run, error) {
	row := s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE server = ? AND kind = ? ORDER BY started_at DESC, id DESC LIMIT 1`, server, kind)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AddAudit appends an entry to the audit log.
func (s *Store) AddAudit(actor, action, target, details string) error {
	_, err := s.db.Exec(`INSERT INTO audit (at, actor, action, target, details) VALUES (?, ?, ?, ?, ?)`,
		ts(time.Now()), actor, action, target, details)
	return err
}

// ListAudit returns the latest audit entries, newest first.
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, at, actor, action, target, details FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Details); err != nil {
			return nil, err
		}
		e.At = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateSession stores a session.
func (s *Store) CreateSession(sess Session) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO sessions (id, user, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sess.ID, sess.User, ts(sess.CreatedAt), ts(sess.ExpiresAt))
	return err
}

// GetSession returns a session that exists and has not expired, else ErrNotFound.
func (s *Store) GetSession(id string) (*Session, error) {
	var sess Session
	var created, expires string
	err := s.db.QueryRow(`SELECT id, user, created_at, expires_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.User, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.ExpiresAt = parseTS(created), parseTS(expires)
	if !sess.ExpiresAt.After(time.Now()) {
		return nil, ErrNotFound
	}
	return &sess, nil
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// PurgeExpiredSessions deletes expired sessions and returns how many.
func (s *Store) PurgeExpiredSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetSetting returns a setting value, or "" and false when unset.
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetSetting creates or replaces a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

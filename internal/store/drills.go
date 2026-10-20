package store

import (
	"database/sql"
	"errors"
	"time"
)

// Drill is the outcome of one restore drill. Details is a JSON document the
// application layer owns (steps, databases, LSN, paths).
type Drill struct {
	ID         int64
	Server     string
	BackupID   string
	StartedAt  time.Time
	FinishedAt time.Time
	OK         bool
	StepFailed string // "" when the drill passed
	Details    string // JSON
	RunID      int64
	ReportPath string // "" when no report could be written
}

const drillCols = `id, server, backup_id, started_at, finished_at, ok, step_failed, details, run_id, report_path`

func scanDrill(sc scanner) (Drill, error) {
	var d Drill
	var started, finished string
	var ok int
	err := sc.Scan(&d.ID, &d.Server, &d.BackupID, &started, &finished, &ok, &d.StepFailed, &d.Details, &d.RunID, &d.ReportPath)
	d.StartedAt, d.FinishedAt, d.OK = parseTS(started), parseTS(finished), ok != 0
	return d, err
}

// AddDrill stores a finished drill and returns its id.
func (s *Store) AddDrill(d Drill) (int64, error) {
	if d.Details == "" {
		d.Details = "{}"
	}
	ok := 0
	if d.OK {
		ok = 1
	}
	res, err := s.db.Exec(`INSERT INTO drills (server, backup_id, started_at, finished_at, ok, step_failed, details, run_id, report_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Server, d.BackupID, ts(d.StartedAt), ts(d.FinishedAt), ok, d.StepFailed, d.Details, d.RunID, d.ReportPath)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateDrillDetails replaces the details and the report path of a drill.
func (s *Store) UpdateDrillDetails(id int64, details, reportPath string) error {
	res, err := s.db.Exec(`UPDATE drills SET details = ?, report_path = ? WHERE id = ?`, details, reportPath, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDrill returns one drill, or ErrNotFound.
func (s *Store) GetDrill(id int64) (*Drill, error) {
	d, err := scanDrill(s.db.QueryRow(`SELECT `+drillCols+` FROM drills WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// DrillForRun returns the drill a run produced, or ErrNotFound.
func (s *Store) DrillForRun(runID int64) (*Drill, error) {
	d, err := scanDrill(s.db.QueryRow(`SELECT `+drillCols+` FROM drills WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDrills returns the drills of a server (all when empty), newest first.
// limit <= 0 means 50.
func (s *Store) ListDrills(server string, limit int) ([]Drill, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + drillCols + ` FROM drills`
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
	defer func() { _ = rows.Close() }()
	var out []Drill
	for rows.Next() {
		d, err := scanDrill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

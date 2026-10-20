package store

import "time"

// AlertSent is one send of one message to one channel (the alert journal).
type AlertSent struct {
	ID      int64
	At      time.Time
	Channel string
	Kind    string
	Server  string
	Level   string
	Text    string
	OK      bool
	Error   string
}

// AddAlert records a send, successful or not.
func (s *Store) AddAlert(a AlertSent) error {
	ok := 0
	if a.OK {
		ok = 1
	}
	_, err := s.db.Exec(`INSERT INTO alerts_sent (at, channel, kind, server, level, text, ok, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ts(a.At), a.Channel, a.Kind, a.Server, a.Level, a.Text, ok, a.Error)
	return err
}

// ListAlerts returns the latest sends, newest first.
func (s *Store) ListAlerts(limit int) ([]AlertSent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, at, channel, kind, server, level, text, ok, error FROM alerts_sent ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AlertSent
	for rows.Next() {
		var a AlertSent
		var at string
		var ok int
		if err := rows.Scan(&a.ID, &at, &a.Channel, &a.Kind, &a.Server, &a.Level, &a.Text, &ok, &a.Error); err != nil {
			return nil, err
		}
		a.At, a.OK = parseTS(at), ok != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlertCount is a number of sends for a channel and an outcome.
type AlertCount struct {
	Channel string
	OK      bool
	N       int64
}

// CountAlerts counts the sends since the database was created, by channel
// and outcome.
func (s *Store) CountAlerts() ([]AlertCount, error) {
	rows, err := s.db.Query(`SELECT channel, ok, COUNT(*) FROM alerts_sent GROUP BY channel, ok ORDER BY channel, ok`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AlertCount
	for rows.Next() {
		var c AlertCount
		var ok int
		if err := rows.Scan(&c.Channel, &ok, &c.N); err != nil {
			return nil, err
		}
		c.OK = ok != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneAlerts keeps the latest `keep` rows of the journal.
func (s *Store) PruneAlerts(keep int) error {
	_, err := s.db.Exec(`DELETE FROM alerts_sent WHERE id <= (SELECT COALESCE(MAX(id), 0) FROM alerts_sent) - ?`, keep)
	return err
}

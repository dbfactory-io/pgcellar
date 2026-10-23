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

// AlertDelivery is what the journal says about one channel: its last
// successful send and the failures since then.
type AlertDelivery struct {
	Channel     string
	LastOK      time.Time // zero when the channel never delivered anything
	Failures    int       // sends that failed after LastOK, in a row
	FirstFailAt time.Time // the first of those failures
	LastFailAt  time.Time
	LastError   string
}

// AlertDeliveries reads the journal channel by channel. Every row after the
// last successful one is a failure by construction, so the run of failures
// is simply what comes after it.
func (s *Store) AlertDeliveries() ([]AlertDelivery, error) {
	rows, err := s.db.Query(`SELECT channel, COALESCE(MAX(CASE WHEN ok = 1 THEN id END), 0) FROM alerts_sent GROUP BY channel ORDER BY channel`)
	if err != nil {
		return nil, err
	}
	type head struct {
		channel string
		okID    int64
	}
	var heads []head
	for rows.Next() {
		var h head
		if err := rows.Scan(&h.channel, &h.okID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		heads = append(heads, h)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]AlertDelivery, 0, len(heads))
	for _, h := range heads {
		d := AlertDelivery{Channel: h.channel}
		if h.okID > 0 {
			var at string
			if err := s.db.QueryRow(`SELECT at FROM alerts_sent WHERE id = ?`, h.okID).Scan(&at); err != nil {
				return nil, err
			}
			d.LastOK = parseTS(at)
		}
		var first, last, lastErr string
		var n int
		err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(at), ''),
			COALESCE((SELECT at FROM alerts_sent WHERE channel = ? AND id > ? ORDER BY id DESC LIMIT 1), ''),
			COALESCE((SELECT error FROM alerts_sent WHERE channel = ? AND id > ? ORDER BY id DESC LIMIT 1), '')
			FROM alerts_sent WHERE channel = ? AND id > ?`, h.channel, h.okID, h.channel, h.okID, h.channel, h.okID).Scan(&n, &first, &last, &lastErr)
		if err != nil {
			return nil, err
		}
		d.Failures, d.LastError = n, lastErr
		if n > 0 {
			d.FirstFailAt, d.LastFailAt = parseTS(first), parseTS(last)
		}
		out = append(out, d)
	}
	return out, nil
}

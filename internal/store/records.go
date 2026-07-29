package store

import "database/sql"

// LogRow is one captured output line from a worker (or the engine itself,
// stream "system").
type LogRow struct {
	Function  string `json:"function"`
	RequestID string `json:"requestId,omitempty"`
	Stream    string `json:"stream"` // stdout | stderr | system
	TS        int64  `json:"ts"`     // unix ms
	Text      string `json:"text"`
}

func (s *Store) InsertLog(r LogRow) error {
	_, err := s.db.Exec(
		`INSERT INTO logs (function, request_id, stream, ts, line) VALUES (?, ?, ?, ?, ?)`,
		r.Function, nullable(r.RequestID), r.Stream, r.TS, r.Text)
	return err
}

// RecentLogs returns the newest `limit` lines (all functions when function
// is empty), oldest-first so they print naturally.
func (s *Store) RecentLogs(function string, limit int) ([]LogRow, error) {
	rows, err := s.db.Query(
		`SELECT function, request_id, stream, ts, line FROM logs
		 WHERE (? = '' OR function = ?)
		 ORDER BY id DESC LIMIT ?`, function, function, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogRow
	for rows.Next() {
		var r LogRow
		var reqID sql.NullString
		if err := rows.Scan(&r.Function, &reqID, &r.Stream, &r.TS, &r.Text); err != nil {
			return nil, err
		}
		r.RequestID = reqID.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reverse(out)
	return out, nil
}

// InvocationRow summarizes one function invocation.
type InvocationRow struct {
	ID         string `json:"id"`
	Function   string `json:"function"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	DurationMs int64  `json:"durationMs"`
}

func (s *Store) StartInvocation(id, function, source string, payload []byte, startedAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO invocations (id, function, source, status, payload, started_at)
		 VALUES (?, ?, ?, 'running', ?, ?)`, id, function, source, payload, startedAt)
	return err
}

func (s *Store) CompleteInvocation(id, status string, result []byte, errMsg string, completedAt, durationMs int64) error {
	_, err := s.db.Exec(
		`UPDATE invocations SET status = ?, result = ?, error = ?, completed_at = ?, duration_ms = ?
		 WHERE id = ?`, status, result, nullable(errMsg), completedAt, durationMs, id)
	return err
}

func (s *Store) RecentInvocations(function string, limit int) ([]InvocationRow, error) {
	rows, err := s.db.Query(
		`SELECT id, function, source, status, error, started_at, duration_ms FROM invocations
		 WHERE (? = '' OR function = ?)
		 ORDER BY started_at DESC LIMIT ?`, function, function, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InvocationRow
	for rows.Next() {
		var r InvocationRow
		var errMsg sql.NullString
		var dur sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Function, &r.Source, &r.Status, &errMsg, &r.StartedAt, &dur); err != nil {
			return nil, err
		}
		r.Error = errMsg.String
		r.DurationMs = dur.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordEvent stores an inbound event payload for later inspection and
// replay (phase 5 builds the UX on top of these rows).
func (s *Store) RecordEvent(id, eventType, source, targetFunction string, payload []byte, createdAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO events (id, type, source, target_function, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, id, eventType, nullable(source), nullable(targetFunction), payload, createdAt)
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func reverse(rows []LogRow) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}

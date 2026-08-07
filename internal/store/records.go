package store

import (
	"database/sql"
	"fmt"
)

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

// EventRow is one recorded trigger event, joined with the outcome of the
// invocation it caused (events.id == invocations.id, the request id).
type EventRow struct {
	ID         string `json:"id"`
	Type       string `json:"type"`   // http | sqs | manual | replay | …
	Source     string `json:"source"` // free-form origin detail
	Function   string `json:"function"`
	Payload    []byte `json:"payload,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	Status     string `json:"status"` // invocation outcome; "" if unknown
	DurationMs int64  `json:"durationMs"`
}

// RecentEvents lists recorded events newest-first, with each invocation's
// outcome. Payloads are omitted (list views don't need the bytes).
func (s *Store) RecentEvents(function string, limit int) ([]EventRow, error) {
	rows, err := s.db.Query(
		`SELECT e.id, e.type, COALESCE(e.source, ''), COALESCE(e.target_function, ''),
		        e.created_at, COALESCE(i.status, ''), COALESCE(i.duration_ms, 0)
		 FROM events e LEFT JOIN invocations i ON i.id = e.id
		 WHERE (? = '' OR e.target_function = ?)
		 ORDER BY e.created_at DESC LIMIT ?`, function, function, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(&r.ID, &r.Type, &r.Source, &r.Function, &r.CreatedAt, &r.Status, &r.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventByPrefix fetches one event (payload included) by full id or unique
// prefix — the CLI shows 8-char ids, git-style.
func (s *Store) EventByPrefix(prefix string) (*EventRow, error) {
	rows, err := s.db.Query(
		`SELECT e.id, e.type, COALESCE(e.source, ''), COALESCE(e.target_function, ''), e.payload, e.created_at
		 FROM events e WHERE e.id LIKE ? ORDER BY e.created_at DESC LIMIT 2`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var found []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(&r.ID, &r.Type, &r.Source, &r.Function, &r.Payload, &r.CreatedAt); err != nil {
			return nil, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no event matches %q — run `pulse events` to see recent ones", prefix)
	case 1:
		return &found[0], nil
	default:
		return nil, fmt.Errorf("%q matches more than one event — use more characters of the id", prefix)
	}
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

// RequestStory is everything one request id touched: the invocation, its
// exact event payload, and every log line it produced.
type RequestStory struct {
	Invocation InvocationRow `json:"invocation"`
	Event      []byte        `json:"event,omitempty"`
	Result     []byte        `json:"result,omitempty"`
	Logs       []LogRow      `json:"logs"`
}

// RequestByPrefix assembles the story for a full id or unique prefix.
func (s *Store) RequestByPrefix(prefix string) (*RequestStory, error) {
	rows, err := s.db.Query(
		`SELECT id, function, source, status, COALESCE(error, ''), started_at,
		        COALESCE(duration_ms, 0), payload, result
		 FROM invocations WHERE id LIKE ? ORDER BY started_at DESC LIMIT 2`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var found []RequestStory
	for rows.Next() {
		var st RequestStory
		var payload, result []byte
		if err := rows.Scan(&st.Invocation.ID, &st.Invocation.Function, &st.Invocation.Source,
			&st.Invocation.Status, &st.Invocation.Error, &st.Invocation.StartedAt,
			&st.Invocation.DurationMs, &payload, &result); err != nil {
			return nil, err
		}
		st.Event, st.Result = payload, result
		found = append(found, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no request matches %q — run `pulse events` to see recent ids", prefix)
	case 1:
		// fall through
	default:
		return nil, fmt.Errorf("%q matches more than one request — use more characters of the id", prefix)
	}

	story := &found[0]
	lrows, err := s.db.Query(
		`SELECT function, COALESCE(request_id, ''), stream, ts, line
		 FROM logs WHERE request_id = ? ORDER BY ts, id`, story.Invocation.ID)
	if err != nil {
		return nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var l LogRow
		if err := lrows.Scan(&l.Function, &l.RequestID, &l.Stream, &l.TS, &l.Text); err != nil {
			return nil, err
		}
		story.Logs = append(story.Logs, l)
	}
	return story, lrows.Err()
}

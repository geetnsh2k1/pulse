// Package sqs is pulse's local SQS: SQLite-backed queues with visibility
// timeouts, delays, and dead-letter redrive. The wire protocol lives in
// protocol.go; the ESM poller and inspection APIs call the Go methods here
// directly.
package sqs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"pulse/internal/awsfacade"
	"pulse/internal/config"
	"pulse/internal/store"
)

const (
	maxBodyBytes    = 256 * 1024
	maxDelaySeconds = 900
	longPollTick    = 200 * time.Millisecond
)

// MessageAttribute mirrors the SQS API shape.
type MessageAttribute struct {
	DataType    string `json:"DataType"`
	StringValue string `json:"StringValue,omitempty"`
	BinaryValue string `json:"BinaryValue,omitempty"` // base64
}

// Message is the internal view handed to the ESM poller and inspection APIs.
type Message struct {
	ID            string
	Body          string
	Attributes    map[string]MessageAttribute
	SentAt        int64
	FirstReceived int64
	ReceiveCount  int
	Receipt       string
}

// Stats is a point-in-time queue depth snapshot.
type Stats struct {
	Name     string `json:"name"`
	Visible  int    `json:"visible"`
	InFlight int    `json:"inFlight"`
	Delayed  int    `json:"delayed"`
	DLQ      string `json:"dlq,omitempty"`
}

type queueInfo struct {
	name              string
	visibilityTimeout int
	dlq               string
	maxReceiveCount   int
}

type Service struct {
	st     *store.Store
	region string

	baseURL func() string // façade base URL, for QueueUrl strings
	onEvent func(string)  // optional: DLQ moves etc.
	now     func() time.Time

	mu     sync.Mutex
	queues map[string]*queueInfo
}

func New(cfg *config.Config, st *store.Store) *Service {
	s := &Service{
		st:      st,
		region:  cfg.Region,
		baseURL: func() string { return "http://127.0.0.1:0" },
		now:     time.Now,
		queues:  map[string]*queueInfo{},
	}
	for name, q := range cfg.Resources.Queues {
		s.queues[name] = &queueInfo{
			name:              name,
			visibilityTimeout: q.VisibilityTimeout,
			dlq:               q.DLQ,
			maxReceiveCount:   q.MaxReceiveCount,
		}
	}
	return s
}

// SetBaseURL wires the façade address used to mint queue URLs.
func (s *Service) SetBaseURL(fn func() string) { s.baseURL = fn }

// SetOnEvent registers a listener for human-readable happenings.
func (s *Service) SetOnEvent(fn func(string)) { s.onEvent = fn }

func (s *Service) event(format string, args ...any) {
	if s.onEvent != nil {
		s.onEvent(fmt.Sprintf(format, args...))
	}
}

func (s *Service) queue(name string) *queueInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queues[name]
}

// EnsureQueue registers a queue at runtime (CreateQueue); declared queues
// keep their configured behavior.
func (s *Service) EnsureQueue(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queues[name]; !ok {
		s.queues[name] = &queueInfo{name: name, visibilityTimeout: 30}
	}
}

// QueueNames returns all known queues, sorted.
func (s *Service) QueueNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.queues))
	for n := range s.queues {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// QueueURL mints the URL SDKs use to address a queue.
func (s *Service) QueueURL(name string) string {
	return strings.TrimSuffix(s.baseURL(), "/") + "/000000000000/" + name
}

// QueueARN mirrors AWS's ARN shape for event records.
func (s *Service) QueueARN(name string) string {
	return fmt.Sprintf("arn:aws:sqs:%s:000000000000:%s", s.region, name)
}

// queueFromURL accepts any historical URL for the queue (ports change across
// restarts) by trusting only the final path segment.
func queueFromURL(qurl string) string {
	if qurl == "" {
		return ""
	}
	if u, err := url.Parse(qurl); err == nil && u.Path != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		return parts[len(parts)-1]
	}
	parts := strings.Split(strings.Trim(qurl, "/"), "/")
	return parts[len(parts)-1]
}

func errQueueMissing(name string) *awsfacade.APIError {
	return &awsfacade.APIError{
		Type:      "com.amazonaws.sqs#QueueDoesNotExist",
		QueryCode: "AWS.SimpleQueueService.NonExistentQueue",
		Message:   fmt.Sprintf("The specified queue does not exist: %q.", name),
	}
}

func errInvalidParam(format string, args ...any) *awsfacade.APIError {
	return &awsfacade.APIError{
		Type:      "com.amazonaws.sqs#InvalidParameterValue",
		QueryCode: "InvalidParameterValue",
		Message:   fmt.Sprintf(format, args...),
	}
}

// ensureForWrite auto-creates unknown queues on write-intent operations —
// simple apps shouldn't have to declare a queue before using it. Declared
// queues keep their configured behavior; auto-created ones get defaults.
func (s *Service) ensureForWrite(name string) *queueInfo {
	if q := s.queue(name); q != nil {
		return q
	}
	s.EnsureQueue(name)
	s.event("✱ auto-created queue %q — declare it under resources.queues in pulse.yaml to configure visibility or a dead-letter queue", name)
	return s.queue(name)
}

// Send enqueues one message and returns its id.
func (s *Service) Send(queue, body string, delaySec int, attrs map[string]MessageAttribute) (string, *awsfacade.APIError) {
	s.ensureForWrite(queue)
	if len(body) > maxBodyBytes {
		return "", errInvalidParam("message body of %d bytes exceeds the %d byte limit", len(body), maxBodyBytes)
	}
	if delaySec < 0 || delaySec > maxDelaySeconds {
		return "", errInvalidParam("DelaySeconds %d is out of range [0, %d]", delaySec, maxDelaySeconds)
	}

	id := uuid.NewString()
	nowMs := s.now().UnixMilli()
	var attrJSON any
	if len(attrs) > 0 {
		b, _ := json.Marshal(attrs)
		attrJSON = string(b)
	}
	_, err := s.st.DB().Exec(
		`INSERT INTO sqs_messages (id, queue, body, attributes, sent_at, visible_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, queue, body, attrJSON, nowMs, nowMs+int64(delaySec)*1000)
	if err != nil {
		return "", internalErr(err)
	}
	return id, nil
}

// Receive claims up to max visible messages, making them invisible for the
// visibility timeout (queue default when visibilitySec <= 0). waitSec > 0
// long-polls. Messages that already burned maxReceiveCount receives are
// swept to the DLQ instead of being redelivered.
func (s *Service) Receive(queue string, max, visibilitySec, waitSec int) ([]Message, *awsfacade.APIError) {
	q := s.queue(queue)
	if q == nil {
		return nil, errQueueMissing(queue)
	}
	if max <= 0 {
		max = 1
	}
	if max > 10 {
		max = 10
	}
	vis := visibilitySec
	if vis <= 0 {
		vis = q.visibilityTimeout
	}

	deadline := s.now().Add(time.Duration(waitSec) * time.Second)
	for {
		msgs, err := s.receiveOnce(q, max, vis)
		if err != nil || len(msgs) > 0 || !s.now().Before(deadline) {
			return msgs, err
		}
		time.Sleep(longPollTick)
	}
}

func (s *Service) receiveOnce(q *queueInfo, max, visibilitySec int) ([]Message, *awsfacade.APIError) {
	nowMs := s.now().UnixMilli()
	db := s.st.DB()

	// Dead-letter sweep: anything visible again after exhausting its
	// receives moves to the DLQ rather than being delivered once more.
	if q.dlq != "" && q.maxReceiveCount > 0 {
		rows, err := db.Query(
			`SELECT id FROM sqs_messages WHERE queue = ? AND visible_at <= ? AND receive_count >= ?`,
			q.name, nowMs, q.maxReceiveCount)
		if err != nil {
			return nil, internalErr(err)
		}
		var doomed []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, internalErr(err)
			}
			doomed = append(doomed, id)
		}
		rows.Close()
		for _, id := range doomed {
			if _, err := db.Exec(
				`UPDATE sqs_messages SET queue = ?, receipt = NULL, visible_at = ? WHERE id = ?`,
				q.dlq, nowMs, id); err != nil {
				return nil, internalErr(err)
			}
			s.event("☠ %s: message moved to dead-letter queue %s after %d receives", q.name, q.dlq, q.maxReceiveCount)
		}
	}

	rows, err := db.Query(
		`SELECT id, body, attributes, sent_at, receive_count, first_received_at
		 FROM sqs_messages WHERE queue = ? AND visible_at <= ?
		 ORDER BY sent_at LIMIT ?`, q.name, nowMs, max)
	if err != nil {
		return nil, internalErr(err)
	}
	var out []Message
	for rows.Next() {
		var m Message
		var attrs sql.NullString
		var firstRecv sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Body, &attrs, &m.SentAt, &m.ReceiveCount, &firstRecv); err != nil {
			rows.Close()
			return nil, internalErr(err)
		}
		if attrs.Valid {
			_ = json.Unmarshal([]byte(attrs.String), &m.Attributes)
		}
		m.FirstReceived = firstRecv.Int64
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internalErr(err)
	}

	for i := range out {
		out[i].Receipt = uuid.NewString()
		out[i].ReceiveCount++
		if out[i].FirstReceived == 0 {
			out[i].FirstReceived = nowMs
		}
		if _, err := db.Exec(
			`UPDATE sqs_messages SET receive_count = ?, receipt = ?, visible_at = ?, first_received_at = ? WHERE id = ?`,
			out[i].ReceiveCount, out[i].Receipt, nowMs+int64(visibilitySec)*1000, out[i].FirstReceived, out[i].ID); err != nil {
			return nil, internalErr(err)
		}
	}
	return out, nil
}

// Delete removes a message by its current receipt handle.
func (s *Service) Delete(queue, receipt string) *awsfacade.APIError {
	if s.queue(queue) == nil {
		return errQueueMissing(queue)
	}
	res, err := s.st.DB().Exec(`DELETE FROM sqs_messages WHERE queue = ? AND receipt = ?`, queue, receipt)
	if err != nil {
		return internalErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &awsfacade.APIError{
			Type:      "com.amazonaws.sqs#ReceiptHandleIsInvalid",
			QueryCode: "ReceiptHandleIsInvalid",
			Message:   "The receipt handle is not valid (expired visibility or already deleted).",
		}
	}
	return nil
}

// ChangeVisibility re-times a claimed message (0 = return it immediately).
func (s *Service) ChangeVisibility(queue, receipt string, seconds int) *awsfacade.APIError {
	if s.queue(queue) == nil {
		return errQueueMissing(queue)
	}
	res, err := s.st.DB().Exec(`UPDATE sqs_messages SET visible_at = ? WHERE queue = ? AND receipt = ?`,
		s.now().UnixMilli()+int64(seconds)*1000, queue, receipt)
	if err != nil {
		return internalErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &awsfacade.APIError{
			Type:      "com.amazonaws.sqs#ReceiptHandleIsInvalid",
			QueryCode: "ReceiptHandleIsInvalid",
			Message:   "The receipt handle is not valid.",
		}
	}
	return nil
}

// Purge drops every message in the queue.
func (s *Service) Purge(queue string) *awsfacade.APIError {
	if s.queue(queue) == nil {
		return errQueueMissing(queue)
	}
	if _, err := s.st.DB().Exec(`DELETE FROM sqs_messages WHERE queue = ?`, queue); err != nil {
		return internalErr(err)
	}
	return nil
}

// QueueStats reports live depths for one queue.
func (s *Service) QueueStats(queue string) (Stats, *awsfacade.APIError) {
	q := s.queue(queue)
	if q == nil {
		return Stats{}, errQueueMissing(queue)
	}
	nowMs := s.now().UnixMilli()
	var visible, inflight, delayed int
	err := s.st.DB().QueryRow(
		`SELECT
			COALESCE(SUM(CASE WHEN visible_at <= ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN visible_at > ? AND receive_count > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN visible_at > ? AND receive_count = 0 THEN 1 ELSE 0 END), 0)
		 FROM sqs_messages WHERE queue = ?`, nowMs, nowMs, nowMs, queue).
		Scan(&visible, &inflight, &delayed)
	if err != nil {
		return Stats{}, internalErr(err)
	}
	return Stats{Name: queue, Visible: visible, InFlight: inflight, Delayed: delayed, DLQ: q.dlq}, nil
}

// AllStats reports depths for every known queue, sorted by name.
func (s *Service) AllStats() []Stats {
	out := make([]Stats, 0)
	for _, name := range s.QueueNames() {
		if st, err := s.QueueStats(name); err == nil {
			out = append(out, st)
		}
	}
	return out
}

func internalErr(err error) *awsfacade.APIError {
	return &awsfacade.APIError{
		Type:      "com.amazonaws.sqs#InternalError",
		QueryCode: "InternalError",
		Message:   err.Error(),
		Status:    500,
	}
}

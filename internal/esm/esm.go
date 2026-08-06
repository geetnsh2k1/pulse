// Package esm emulates Lambda event source mappings: per-trigger pollers
// that pull SQS batches and invoke the wired function, honoring partial
// batch failures, visibility-timeout retries, and dead-letter redrive.
package esm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"pulse/internal/config"
	"pulse/internal/logs"
	"pulse/internal/services/sqs"
	"pulse/internal/workers"
)

// pollWait is the internal long-poll per receive; it also bounds how long
// Stop() can take.
const pollWait = 1 // seconds

type Poller struct {
	cfg   *config.Config
	svc   *sqs.Service
	mgr   *workers.Manager
	sink  *logs.Sink
	event func(string)

	// CelebrateOK, when set, is called after every fully-successful batch —
	// the engine uses it to print a one-time "first background job" line.
	CelebrateOK func()

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg *config.Config, svc *sqs.Service, mgr *workers.Manager, sink *logs.Sink, eventFn func(string)) *Poller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Poller{cfg: cfg, svc: svc, mgr: mgr, sink: sink, event: eventFn, ctx: ctx, cancel: cancel}
}

// Start launches one polling loop per sqs trigger.
func (p *Poller) Start() {
	for _, t := range p.cfg.Triggers {
		if t.Type != "sqs" {
			continue
		}
		p.wg.Add(1)
		go p.loop(t)
	}
}

// Stop halts all pollers; in-flight batches finish their invocation first.
func (p *Poller) Stop() {
	p.cancel()
	p.wg.Wait()
}

func (p *Poller) loop(t *config.Trigger) {
	defer p.wg.Done()
	// Like AWS, receives use the queue's own visibility timeout — configure
	// it longer than the function's timeout to avoid mid-run redelivery.
	batch := t.BatchSize
	if batch > 10 {
		batch = 10 // one receive per batch in the MVP; >10 accumulation is backlog
	}

	for {
		if p.ctx.Err() != nil {
			return
		}
		msgs, apiErr := p.svc.Receive(t.Queue, batch, 0, pollWait)
		if apiErr != nil {
			p.sink.System(t.Function, "", "sqs poller: "+apiErr.Message, time.Now().UnixMilli())
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if len(msgs) == 0 {
			continue
		}
		p.deliver(t, msgs)
	}
}

func (p *Poller) deliver(t *config.Trigger, msgs []sqs.Message) {
	event := p.buildEvent(t.Queue, msgs)
	requestID := uuid.NewString()

	res, err := p.mgr.InvokeAs(p.ctx, requestID, t.Function, "sqs", event)
	if err != nil {
		p.sink.System(t.Function, requestID, "sqs delivery failed: "+err.Error(), time.Now().UnixMilli())
		return
	}

	outcome := ""
	switch res.Status {
	case "success":
		failed := parseBatchItemFailures(res.Payload)
		kept := 0
		for _, m := range msgs {
			if failed[m.ID] {
				kept++
				continue
			}
			if delErr := p.svc.Delete(t.Queue, m.Receipt); delErr != nil {
				p.sink.System(t.Function, requestID, "deleting processed message: "+delErr.Message, time.Now().UnixMilli())
			}
		}
		if kept == 0 {
			outcome = "ok"
		} else {
			outcome = fmt.Sprintf("ok, %d to retry", kept)
		}
	default:
		// Whole batch failed: delete nothing; visibility timeout retries it,
		// and the DLQ catches serial offenders.
		outcome = res.Status + " — batch will retry"
	}

	line := fmt.Sprintf("⚙ sqs %s → %s · batch of %d · %s", t.Queue, t.Function, len(msgs), outcome)
	if p.event != nil {
		p.event(line)
	}
	p.sink.System(t.Function, requestID, line, time.Now().UnixMilli())
	if outcome == "ok" && p.CelebrateOK != nil {
		p.CelebrateOK()
	}
}

// buildEvent renders the Lambda SQS event shape (note: lowercased keys,
// unlike the SQS API responses).
func (p *Poller) buildEvent(queue string, msgs []sqs.Message) []byte {
	records := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		attrs := map[string]any{}
		for name, a := range m.Attributes {
			attrs[name] = map[string]any{
				"stringValue": a.StringValue,
				"binaryValue": a.BinaryValue,
				"dataType":    a.DataType,
			}
		}
		records = append(records, map[string]any{
			"messageId":     m.ID,
			"receiptHandle": m.Receipt,
			"body":          m.Body,
			"attributes": map[string]string{
				"ApproximateReceiveCount":          fmt.Sprint(m.ReceiveCount),
				"SentTimestamp":                    fmt.Sprint(m.SentAt),
				"ApproximateFirstReceiveTimestamp": fmt.Sprint(m.FirstReceived),
				"SenderId":                         "000000000000",
			},
			"messageAttributes": attrs,
			"md5OfBody":         sqs.MD5Hex(m.Body),
			"eventSource":       "aws:sqs",
			"eventSourceARN":    p.svc.QueueARN(queue),
			"awsRegion":         p.cfg.Region,
		})
	}
	b, _ := json.Marshal(map[string]any{"Records": records})
	return b
}

// parseBatchItemFailures reads Lambda's partial-batch-response contract.
func parseBatchItemFailures(payload []byte) map[string]bool {
	var resp struct {
		BatchItemFailures []struct {
			ItemIdentifier string `json:"itemIdentifier"`
		} `json:"batchItemFailures"`
	}
	out := map[string]bool{}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return out
	}
	for _, f := range resp.BatchItemFailures {
		if f.ItemIdentifier != "" {
			out[f.ItemIdentifier] = true
		}
	}
	return out
}

package esm

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"pulse/internal/config"
	"pulse/internal/logs"
	sqssvc "pulse/internal/services/sqs"
	"pulse/internal/store"
	"pulse/internal/workers"
)

// The worker acks good jobs and reports jobs with {"fail":true} as batch
// item failures, exercising retry and dead-letter redrive end to end.
const workerHandler = `
export const handler = async (event) => {
  const failures = [];
  for (const r of event.Records) {
    const job = JSON.parse(r.body);
    console.log("job", job.n, "attempt", r.attributes.ApproximateReceiveCount);
    if (job.fail) failures.push({ itemIdentifier: r.messageId });
  }
  return { batchItemFailures: failures };
};
`

func TestPollerDeliversRetriesAndDeadLetters(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping")
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fn", "index.mjs"), []byte(workerHandler), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project: "esm-test",
		Region:  "us-east-1",
		Functions: map[string]*config.Function{
			"worker": {Name: "worker", Runtime: "nodejs20.x", Handler: "index.handler", CodeDir: "fn", Timeout: 5, Memory: 128},
		},
		Triggers: []*config.Trigger{
			{Type: "sqs", Queue: "jobs", Function: "worker", BatchSize: 10},
		},
		Resources: config.Resources{
			Queues: map[string]*config.Queue{
				"jobs":     {Name: "jobs", VisibilityTimeout: 1, DLQ: "jobs-dlq", MaxReceiveCount: 2},
				"jobs-dlq": {Name: "jobs-dlq", VisibilityTimeout: 30},
			},
		},
		Root: root,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sink := logs.NewSink(st)

	svc := sqssvc.New(cfg, st)
	mgr := workers.NewManager(cfg, st, sink)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)

	// Two good jobs and one that always fails.
	for _, body := range []string{`{"n":1}`, `{"n":2}`, `{"n":3,"fail":true}`} {
		if _, apiErr := svc.Send("jobs", body, 0, nil); apiErr != nil {
			t.Fatal(apiErr)
		}
	}

	p := New(cfg, svc, mgr, sink, nil)
	p.Start()
	t.Cleanup(p.Stop)

	// The failing job burns through the ESM's stretched visibility only when
	// the poller re-receives it; with maxReceiveCount=2 it should land in the
	// DLQ. Wait for the steady state: main queue drained, DLQ holding 1.
	deadline := time.Now().Add(45 * time.Second)
	for {
		main, _ := svc.QueueStats("jobs")
		dlq, _ := svc.QueueStats("jobs-dlq")
		if main.Visible+main.InFlight+main.Delayed == 0 && dlq.Visible == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached steady state: main=%+v dlq=%+v", main, dlq)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The dead-lettered message is the failing one, receive count preserved.
	msgs, apiErr := svc.Receive("jobs-dlq", 10, 0, 0)
	if apiErr != nil || len(msgs) != 1 {
		t.Fatalf("dlq receive: %v %d", apiErr, len(msgs))
	}
	if msgs[0].Body != `{"n":3,"fail":true}` || msgs[0].ReceiveCount < 2 {
		t.Errorf("dlq message = %+v", msgs[0])
	}
}

package sqs

import (
	"strings"
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/store"
)

// testService builds a service over a temp store with a controllable clock.
func testService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Region: "us-east-1",
		Resources: config.Resources{
			Queues: map[string]*config.Queue{
				"jobs":     {Name: "jobs", VisibilityTimeout: 30, DLQ: "jobs-dlq", MaxReceiveCount: 2},
				"jobs-dlq": {Name: "jobs-dlq", VisibilityTimeout: 30},
			},
		},
	}
	s := New(cfg, st)
	now := time.Unix(1_785_331_200, 0)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestSendReceiveDeleteLifecycle(t *testing.T) {
	s, _ := testService(t)

	id, apiErr := s.Send("jobs", "hello", 0, nil)
	if apiErr != nil || id == "" {
		t.Fatalf("Send: %v", apiErr)
	}

	// Sending to an undeclared queue auto-creates it (write intent).
	var events []string
	s.SetOnEvent(func(msg string) { events = append(events, msg) })
	if _, apiErr := s.Send("undeclared", "x", 0, nil); apiErr != nil {
		t.Errorf("auto-create send failed: %v", apiErr)
	}
	if st, _ := s.QueueStats("undeclared"); st.Visible != 1 {
		t.Errorf("auto-created queue stats = %+v", st)
	}
	if len(events) == 0 || !strings.Contains(events[0], "auto-created") {
		t.Errorf("no auto-create note emitted: %v", events)
	}
	// Reading a queue that was never written is still an error (typo guard).
	if _, apiErr := s.Receive("never-written", 1, 0, 0); apiErr == nil || !strings.Contains(apiErr.Type, "QueueDoesNotExist") {
		t.Errorf("receive on unknown queue = %v", apiErr)
	}

	msgs, apiErr := s.Receive("jobs", 10, 0, 0)
	if apiErr != nil || len(msgs) != 1 {
		t.Fatalf("Receive: %v %d", apiErr, len(msgs))
	}
	m := msgs[0]
	if m.Body != "hello" || m.ReceiveCount != 1 || m.Receipt == "" {
		t.Errorf("message = %+v", m)
	}
	if MD5Hex(m.Body) != "5d41402abc4b2a76b9719d911017c592" {
		t.Errorf("md5 = %s", MD5Hex(m.Body))
	}

	// In flight: invisible to a second receive.
	again, _ := s.Receive("jobs", 10, 0, 0)
	if len(again) != 0 {
		t.Errorf("received in-flight message again: %+v", again)
	}

	if apiErr := s.Delete("jobs", "bogus"); apiErr == nil || !strings.Contains(apiErr.Type, "ReceiptHandleIsInvalid") {
		t.Errorf("bogus receipt error = %v", apiErr)
	}
	if apiErr := s.Delete("jobs", m.Receipt); apiErr != nil {
		t.Fatalf("Delete: %v", apiErr)
	}
	stats, _ := s.QueueStats("jobs")
	if stats.Visible+stats.InFlight+stats.Delayed != 0 {
		t.Errorf("stats after delete = %+v", stats)
	}
}

func TestVisibilityTimeoutRedelivery(t *testing.T) {
	s, now := testService(t)
	_, _ = s.Send("jobs", "work", 0, nil)

	first, _ := s.Receive("jobs", 1, 0, 0)
	if len(first) != 1 {
		t.Fatal("no first receive")
	}

	*now = now.Add(31 * time.Second) // default visibility is 30s

	second, _ := s.Receive("jobs", 1, 0, 0)
	if len(second) != 1 || second[0].ReceiveCount != 2 {
		t.Fatalf("redelivery = %+v", second)
	}
	if apiErr := s.Delete("jobs", first[0].Receipt); apiErr == nil {
		t.Error("stale receipt should be invalid after redelivery")
	}
	if apiErr := s.Delete("jobs", second[0].Receipt); apiErr != nil {
		t.Errorf("fresh receipt delete: %v", apiErr)
	}
}

func TestDelaySeconds(t *testing.T) {
	s, now := testService(t)
	_, _ = s.Send("jobs", "later", 5, nil)

	if msgs, _ := s.Receive("jobs", 1, 0, 0); len(msgs) != 0 {
		t.Fatalf("delayed message delivered early: %+v", msgs)
	}
	stats, _ := s.QueueStats("jobs")
	if stats.Delayed != 1 {
		t.Errorf("stats = %+v, want 1 delayed", stats)
	}

	*now = now.Add(6 * time.Second)
	if msgs, _ := s.Receive("jobs", 1, 0, 0); len(msgs) != 1 {
		t.Fatal("delayed message never arrived")
	}
}

func TestDeadLetterRedrive(t *testing.T) {
	s, now := testService(t)
	var events []string
	s.SetOnEvent(func(msg string) { events = append(events, msg) })

	_, _ = s.Send("jobs", "cursed", 0, nil)

	// maxReceiveCount = 2: receive twice without deleting…
	for i := 0; i < 2; i++ {
		msgs, _ := s.Receive("jobs", 1, 1, 0)
		if len(msgs) != 1 {
			t.Fatalf("receive %d got %d messages", i+1, len(msgs))
		}
		*now = now.Add(2 * time.Second) // let visibility lapse
	}

	// …third attempt sweeps it to the DLQ instead of delivering.
	msgs, _ := s.Receive("jobs", 1, 1, 0)
	if len(msgs) != 0 {
		t.Fatalf("message delivered a third time: %+v", msgs)
	}
	dlq, _ := s.Receive("jobs-dlq", 1, 0, 0)
	if len(dlq) != 1 || dlq[0].Body != "cursed" {
		t.Fatalf("dlq contents = %+v", dlq)
	}
	if len(events) == 0 || !strings.Contains(events[0], "dead-letter") {
		t.Errorf("no dead-letter event emitted: %v", events)
	}
}

func TestPurgeAndStats(t *testing.T) {
	s, _ := testService(t)
	for i := 0; i < 3; i++ {
		_, _ = s.Send("jobs", "x", 0, nil)
	}
	_, _ = s.Receive("jobs", 1, 30, 0)

	stats, _ := s.QueueStats("jobs")
	if stats.Visible != 2 || stats.InFlight != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if apiErr := s.Purge("jobs"); apiErr != nil {
		t.Fatal(apiErr)
	}
	stats, _ = s.QueueStats("jobs")
	if stats.Visible+stats.InFlight+stats.Delayed != 0 {
		t.Errorf("stats after purge = %+v", stats)
	}
}

func TestAttributeMD5DeterministicAndOrderInsensitive(t *testing.T) {
	a := map[string]MessageAttribute{
		"beta":  {DataType: "String", StringValue: "2"},
		"alpha": {DataType: "String", StringValue: "1"},
	}
	b := map[string]MessageAttribute{
		"alpha": {DataType: "String", StringValue: "1"},
		"beta":  {DataType: "String", StringValue: "2"},
	}
	da, db := md5OfAttributes(a), md5OfAttributes(b)
	if da != db || len(da) != 32 {
		t.Errorf("digests = %q vs %q", da, db)
	}
	if md5OfAttributes(nil) != "" {
		t.Error("empty attributes should produce no digest")
	}
}

func TestProtocolDo(t *testing.T) {
	s, _ := testService(t)
	url := s.QueueURL("jobs")

	resp, apiErr := s.Do("SendMessage", []byte(`{"QueueUrl":"`+url+`","MessageBody":"hi"}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	sent := resp.(map[string]any)
	if sent["MessageId"] == "" || sent["MD5OfMessageBody"] == "" {
		t.Errorf("SendMessage resp = %v", resp)
	}

	resp, apiErr = s.Do("ReceiveMessage", []byte(`{"QueueUrl":"`+url+`","MaxNumberOfMessages":10}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	messages := resp.(map[string]any)["Messages"].([]map[string]any)
	if len(messages) != 1 || messages[0]["Body"] != "hi" {
		t.Fatalf("ReceiveMessage resp = %v", resp)
	}
	receipt := messages[0]["ReceiptHandle"].(string)

	if _, apiErr = s.Do("DeleteMessage", []byte(`{"QueueUrl":"`+url+`","ReceiptHandle":"`+receipt+`"}`)); apiErr != nil {
		t.Fatal(apiErr)
	}

	resp, apiErr = s.Do("GetQueueUrl", []byte(`{"QueueName":"jobs"}`))
	if apiErr != nil || resp.(map[string]any)["QueueUrl"] != url {
		t.Errorf("GetQueueUrl = %v (%v)", resp, apiErr)
	}

	resp, apiErr = s.Do("GetQueueAttributes", []byte(`{"QueueUrl":"`+url+`"}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	attrs := resp.(map[string]any)["Attributes"].(map[string]string)
	if attrs["QueueArn"] != "arn:aws:sqs:us-east-1:000000000000:jobs" || attrs["RedrivePolicy"] == "" {
		t.Errorf("attributes = %v", attrs)
	}

	if _, apiErr = s.Do("TagQueue", nil); apiErr == nil || !strings.Contains(apiErr.Message, "does not implement") {
		t.Errorf("unsupported action error = %v", apiErr)
	}
}

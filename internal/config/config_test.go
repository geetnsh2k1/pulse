package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
project: order-demo
region: us-east-1

functions:
  createOrder:
    runtime: nodejs20.x
    handler: src/orders.create
    codeDir: services/api
    timeout: 10
    memory: 256
    env:
      TABLE_NAME: orders
  processOrder:
    runtime: python3.12
    handler: worker.handler.process
    codeDir: services/worker

triggers:
  - { type: http, method: POST, path: /orders, function: createOrder }
  - { type: http, method: GET, path: "/orders/{id}", function: createOrder }
  - { type: sqs, queue: order-events, function: processOrder }
  - { type: sns, topic: order-published, function: processOrder }
  - { type: s3, bucket: uploads, function: processOrder }
  - { type: dynamodb-stream, table: orders, function: processOrder }

resources:
  tables:
    orders:
      pk: { name: id, type: S }
      streams: true
  buckets: [uploads]
  queues:
    order-events:
      dlq: order-events-dlq
    order-events-dlq:
  topics:
    order-published:
      subscribers: [processOrder]
`

// writeProject drops a pulse.yaml plus the given code directories into a
// temp root and returns the root path.
func writeProject(t *testing.T, yamlSrc string, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadValidAndDefaults(t *testing.T) {
	root := writeProject(t, validYAML, "services/api", "services/worker")
	cfg, err := Load(filepath.Join(root, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Project != "order-demo" || cfg.Region != "us-east-1" {
		t.Errorf("project/region = %q/%q", cfg.Project, cfg.Region)
	}
	po := cfg.Functions["processOrder"]
	if po.Timeout != 3 || po.Memory != 128 {
		t.Errorf("defaults not applied: timeout=%d memory=%d", po.Timeout, po.Memory)
	}
	if po.Name != "processOrder" {
		t.Errorf("Name not filled from map key: %q", po.Name)
	}

	var sqsTrig, s3Trig *Trigger
	for _, tr := range cfg.Triggers {
		switch tr.Type {
		case "sqs":
			sqsTrig = tr
		case "s3":
			s3Trig = tr
		}
	}
	if sqsTrig.BatchSize != 10 {
		t.Errorf("sqs batchSize default = %d, want 10", sqsTrig.BatchSize)
	}
	if len(s3Trig.Events) != 1 || s3Trig.Events[0] != "created" {
		t.Errorf("s3 events default = %v", s3Trig.Events)
	}

	q := cfg.Resources.Queues["order-events"]
	if q.VisibilityTimeout != 30 || q.MaxReceiveCount != 3 {
		t.Errorf("queue defaults: visibility=%d maxReceive=%d", q.VisibilityTimeout, q.MaxReceiveCount)
	}
	if dlq := cfg.Resources.Queues["order-events-dlq"]; dlq == nil || dlq.Name != "order-events-dlq" {
		t.Errorf("nil-bodied queue not normalized: %+v", dlq)
	}
}

func TestFindWalksUp(t *testing.T) {
	root := writeProject(t, validYAML, "services/api", "services/worker")
	nested := filepath.Join(root, "services", "api")
	got, err := Find(nested)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	want := filepath.Join(root, FileName)
	// t.TempDir may sit behind a symlink (macOS /var → /private/var);
	// compare resolved paths.
	rGot, _ := filepath.EvalSymlinks(got)
	rWant, _ := filepath.EvalSymlinks(want)
	if rGot != rWant {
		t.Errorf("Find = %q, want %q", got, want)
	}

	if _, err := Find(t.TempDir()); err == nil {
		t.Error("Find in empty dir: want error, got nil")
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	_, err := Parse([]byte("project: x\nfunctons: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "functons") {
		t.Errorf("want unknown-field error mentioning 'functons', got: %v", err)
	}
}

func TestEmptyFile(t *testing.T) {
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want empty-file error, got: %v", err)
	}
}

const minimalBase = `
project: demo
functions:
  handle:
    runtime: nodejs20.x
    handler: index.handler
    codeDir: fn
`

func TestValidationProblems(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		dirs []string
		want []string // substrings that must all appear in the error
	}{
		{
			name: "runtime typo gets suggestion",
			yaml: strings.Replace(minimalBase, "nodejs20.x", "nodejs20x", 1),
			dirs: []string{"fn"},
			want: []string{"not a supported runtime", `did you mean "nodejs20.x"`},
		},
		{
			name: "unknown trigger function gets suggestion",
			yaml: minimalBase + `
triggers:
  - { type: http, method: GET, path: /x, function: handl }
`,
			dirs: []string{"fn"},
			want: []string{`unknown function "handl"`, `did you mean "handle"`},
		},
		{
			name: "duplicate route",
			yaml: minimalBase + `
triggers:
  - { type: http, method: GET, path: /x, function: handle }
  - { type: http, method: GET, path: /x, function: handle }
`,
			dirs: []string{"fn"},
			want: []string{"duplicate route", "GET /x"},
		},
		{
			name: "unknown trigger type gets suggestion",
			yaml: minimalBase + `
triggers:
  - { type: htp, method: GET, path: /x, function: handle }
`,
			dirs: []string{"fn"},
			want: []string{"not a known trigger type", `did you mean "http"`},
		},
		{
			name: "sqs trigger with undeclared queue",
			yaml: minimalBase + `
triggers:
  - { type: sqs, queue: jobs, function: handle }
`,
			dirs: []string{"fn"},
			want: []string{`unknown queue "jobs"`, "resources.queues"},
		},
		{
			name: "dlq must be declared",
			yaml: minimalBase + `
resources:
  queues:
    jobs:
      dlq: missing
`,
			dirs: []string{"fn"},
			want: []string{`unknown queue "missing"`},
		},
		{
			name: "queue cannot dlq itself",
			yaml: minimalBase + `
resources:
  queues:
    jobs:
      dlq: jobs
`,
			dirs: []string{"fn"},
			want: []string{"cannot be its own dead-letter queue"},
		},
		{
			name: "stream trigger needs streams enabled",
			yaml: minimalBase + `
triggers:
  - { type: dynamodb-stream, table: orders, function: handle }
resources:
  tables:
    orders:
      pk: { name: id, type: S }
`,
			dirs: []string{"fn"},
			want: []string{"streams disabled", "streams: true"},
		},
		{
			name: "missing code directory",
			yaml: strings.Replace(minimalBase, "codeDir: fn", "codeDir: nope", 1),
			dirs: []string{"fn"},
			want: []string{`directory "nope" not found`},
		},
		{
			name: "bad python handler shape",
			yaml: `
project: demo
functions:
  worker:
    runtime: python3.12
    handler: worker/handler.process
    codeDir: fn
`,
			dirs: []string{"fn"},
			want: []string{"not a valid Python handler"},
		},
		{
			name: "timeout out of range",
			yaml: strings.Replace(minimalBase, "codeDir: fn", "codeDir: fn\n    timeout: 1000", 1),
			dirs: []string{"fn"},
			want: []string{"out of range", "900"},
		},
		{
			name: "topic subscriber must exist",
			yaml: minimalBase + `
resources:
  topics:
    news:
      subscribers: [nobody]
`,
			dirs: []string{"fn"},
			want: []string{`unknown function "nobody"`},
		},
		{
			name: "bucket naming rules",
			yaml: minimalBase + `
resources:
  buckets: [UPLOADS]
`,
			dirs: []string{"fn"},
			want: []string{"not a valid bucket name"},
		},
		{
			name: "at least one function",
			yaml: "project: demo\n",
			want: []string{"at least one function"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeProject(t, tc.yaml, tc.dirs...)
			_, err := Load(filepath.Join(root, FileName))
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestEditDistance(t *testing.T) {
	if d := editDistance("nodejs20x", "nodejs20.x"); d != 1 {
		t.Errorf("editDistance = %d, want 1", d)
	}
	if s := closest("zzzzzz", []string{"http", "sqs"}); s != "" {
		t.Errorf("closest matched something absurd: %q", s)
	}
}

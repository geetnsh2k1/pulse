package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/dotenv"
)

// fullPlan is a realistic import: one function behind a route, fed by a
// queue, writing to a table.
func fullPlan(t *testing.T) *Plan {
	t.Helper()
	d := Discovery{
		Region: "eu-west-1",
		Function: Function{
			Name: "createOrder", Runtime: "python3.12", Handler: "handler.handler",
			TimeoutSec: 15, MemoryMB: 512, PackageType: "Zip", CodeSize: 1 << 20,
			Env: map[string]string{
				"TABLE_NAME":     "orders",
				"STRIPE_API_KEY": "sk_live_super_secret",
				"AWS_REGION":     "eu-west-1", // reserved; must be dropped
			},
		},
		Routes:       []HTTPRoute{{Method: "POST", Path: "/orders", PayloadFormat: "1.0"}},
		EventSources: []EventSource{{Kind: "sqs", ARN: "arn:aws:sqs:::order-events", BatchSize: 10, Enabled: true}},
		AllQueues:    []Queue{{Name: "order-events", VisibilityTimeout: 30, DLQName: "order-events-dlq", MaxReceiveCount: 3}},
	}
	p, err := BuildPlan(d, "shop")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	sk := Key{Name: "createdAt", Type: "S"}
	p.AddTable(Table{Name: "orders", PK: Key{Name: "id", Type: "S"}, SK: &sk}, Picked, []string{"env TABLE_NAME"})
	return p
}

// The strongest guarantee in the importer: whatever the plan produces must
// pass the same validator pulse.yaml goes through, so an import can never
// write a project that won't load.
func TestToConfigValidates(t *testing.T) {
	p := fullPlan(t)
	cfg := p.ToConfig()

	// Validate needs the code directories to exist, as they will after the
	// bundle is unzipped.
	root := t.TempDir()
	cfg.Root = root
	for _, f := range cfg.Functions {
		mkdirAll(t, root, f.CodeDir)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a plan must always produce a valid project: %v", err)
	}

	fn := cfg.Functions["createOrder"]
	if fn == nil || fn.Runtime != "python3.12" || fn.Timeout != 15 || fn.Memory != 512 {
		t.Errorf("function not carried over: %+v", fn)
	}
	// Secrets must never reach the committed file.
	if len(fn.Env) != 0 {
		t.Errorf("pulse.yaml must not carry env values, got %v", fn.Env)
	}
	if q := cfg.Resources.Queues["order-events"]; q == nil || q.DLQ != "order-events-dlq" || q.MaxReceiveCount != 3 {
		t.Errorf("queue not carried over: %+v", q)
	}
	if cfg.Resources.Queues["order-events-dlq"] == nil {
		t.Error("the DLQ itself must exist locally")
	}
	if tbl := cfg.Resources.Tables["orders"]; tbl == nil || tbl.PK.Name != "id" || tbl.SK == nil || tbl.SK.Name != "createdAt" {
		t.Errorf("table schema not carried over: %+v", tbl)
	}
	// payloadFormat 1.0 is meaningful and must be recorded; 2.0 is the default.
	if cfg.Triggers[0].PayloadFormat != "1.0" {
		t.Errorf("REST payload format lost: %+v", cfg.Triggers[0])
	}
}

// Placeholders by default: an import must not drop live keys onto disk.
func TestDotEnvPlaceholdersByDefault(t *testing.T) {
	p := fullPlan(t)
	body := p.DotEnvLines(false)

	if strings.Contains(body, "sk_live_super_secret") {
		t.Fatal("a real secret leaked into .env without --with-values")
	}
	if !strings.Contains(body, "STRIPE_API_KEY="+placeholder) {
		t.Errorf("expected a placeholder for STRIPE_API_KEY, got:\n%s", body)
	}
	if strings.Contains(body, "AWS_REGION") {
		t.Error("reserved variables must not be written to .env")
	}
	if !strings.Contains(body, "--with-values") {
		t.Error(".env should say how to get the real values")
	}

	// And the file must parse with pulse's own reader.
	vars, err := dotenv.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("generated .env must parse: %v", err)
	}
	if vars["TABLE_NAME"] != placeholder {
		t.Errorf("TABLE_NAME = %q", vars["TABLE_NAME"])
	}
}

func TestDotEnvWithValuesRoundTrips(t *testing.T) {
	p := fullPlan(t)
	body := p.DotEnvLines(true)

	vars, err := dotenv.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("generated .env must parse: %v", err)
	}
	if vars["STRIPE_API_KEY"] != "sk_live_super_secret" {
		t.Errorf("value not carried with --with-values: %q", vars["STRIPE_API_KEY"])
	}
	if !strings.Contains(body, "treat this file as secret") {
		t.Error("with real values, the file should say so")
	}
}

// Values with spaces or a # would otherwise re-parse as something else.
func TestDotEnvQuotesAwkwardValues(t *testing.T) {
	p := &Plan{Functions: []PlannedFunction{{
		Name:      "fn",
		EnvNames:  []string{"MESSAGE", "PLAIN"},
		EnvValues: map[string]string{"MESSAGE": "hello # world", "PLAIN": "simple"},
	}}}
	vars, err := dotenv.Parse(strings.NewReader(p.DotEnvLines(true)))
	if err != nil {
		t.Fatalf("must parse: %v", err)
	}
	if vars["MESSAGE"] != "hello # world" {
		t.Errorf("awkward value mangled: %q", vars["MESSAGE"])
	}
	if vars["PLAIN"] != "simple" {
		t.Errorf("plain value should stay unquoted: %q", vars["PLAIN"])
	}
}

func TestDotEnvExampleHasNamesNeverValues(t *testing.T) {
	p := fullPlan(t)
	body := p.DotEnvExampleLines()
	if strings.Contains(body, "sk_live") || strings.Contains(body, placeholder) {
		t.Errorf(".env.example must carry names only, got:\n%s", body)
	}
	if !strings.Contains(body, "STRIPE_API_KEY=") {
		t.Error("names must be listed so a teammate knows what to set")
	}
}

// IMPORT-NOTES.md is the honest record: it must distinguish facts from
// guesses and name everything that didn't come across.
func TestNotesRecordsProvenanceAndGaps(t *testing.T) {
	p := fullPlan(t)
	p.Unsupported = append(p.Unsupported, Note{Subject: "s3 trigger", Detail: "not imported"})
	notes := p.Notes("prod", "111122223333")

	for _, want := range []string{
		"account 111122223333", "prod", "eu-west-1",
		"createOrder", "POST /orders", "order-events",
		"confirmed by AWS configuration", // the queue
		"chosen by you from the account", // the table
		"s3 trigger",                     // the gap
		"only ever read from",            // the read-only reassurance
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("IMPORT-NOTES should mention %q", want)
		}
	}
}

func TestSummaryCounts(t *testing.T) {
	got := fullPlan(t).Summary()
	for _, want := range []string{"1 function", "2 triggers", "1 table", "2 queues"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q should contain %q", got, want)
		}
	}
}

// A key with no declared type must default to S, matching pulse.yaml's own
// shorthand, or Validate rejects it.
func TestToConfigDefaultsKeyType(t *testing.T) {
	p := &Plan{Project: "x", Region: "us-east-1"}
	p.AddTable(Table{Name: "t", PK: Key{Name: "id"}}, Confirmed, nil)
	if got := p.ToConfig().Resources.Tables["t"].PK.Type; got != "S" {
		t.Errorf("key type = %q, want S", got)
	}
}

func mkdirAll(t *testing.T, root, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
}

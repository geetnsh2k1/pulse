package importer

import (
	"errors"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// zipFn is a minimal importable function; tests tweak one thing at a time.
func zipFn() Function {
	return Function{
		Name: "createOrder", Runtime: "python3.12", Handler: "handler.handler",
		TimeoutSec: 10, MemoryMB: 512, PackageType: "Zip", CodeSize: 4 << 20,
		Env: map[string]string{"TABLE_NAME": "orders"},
	}
}

func mustPlan(t *testing.T, d Discovery, project string) *Plan {
	t.Helper()
	p, err := BuildPlan(d, project)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestBuildPlanMapsTheFunction(t *testing.T) {
	p := mustPlan(t, Discovery{Region: "eu-west-1", Function: zipFn()}, "shop")

	if p.Project != "shop" || p.Region != "eu-west-1" {
		t.Errorf("project/region = %q/%q", p.Project, p.Region)
	}
	if len(p.Functions) != 1 {
		t.Fatalf("want 1 function, got %d", len(p.Functions))
	}
	f := p.Functions[0]
	if f.Name != "createOrder" || f.Runtime != "python3.12" || f.Handler != "handler.handler" {
		t.Errorf("function = %+v", f)
	}
	if f.CodeDir != "functions/createOrder" {
		t.Errorf("codeDir = %q", f.CodeDir)
	}
	if f.TimeoutSec != 10 || f.MemoryMB != 512 {
		t.Errorf("timeout/memory = %d/%d", f.TimeoutSec, f.MemoryMB)
	}
	if f.Provenance != Confirmed {
		t.Errorf("the function itself is a fact, got %q", f.Provenance)
	}
	if len(f.EnvNames) != 1 || f.EnvNames[0] != "TABLE_NAME" {
		t.Errorf("env names = %v", f.EnvNames)
	}
}

// Every refusal must name the function and offer a next step — these are the
// cases where pulse says no, so the message is all the user gets.
func TestBuildPlanRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Function)
		wantIn string
		fixHas string
	}{
		{"container image", func(f *Function) { f.PackageType = "Image" }, "container-image", "zip-based"},
		// The fix states the FLOOR, not an enumeration: a fixed list refused a
		// real python3.14 function the day AWS shipped it.
		{"java runtime", func(f *Function) { f.Runtime = "java17" }, "runtime java17", "Python 3.10+"},
		{"go runtime", func(f *Function) { f.Runtime = "provided.al2" }, "provided.al2", "Node 18+"},
		{"retired python", func(f *Function) { f.Runtime = "python3.9" }, "python3.9", "Python 3.10+"},
		{"oversized", func(f *Function) { f.CodeSize = 300 << 20 }, "300 MB", "deployment artifacts"},
		{"no handler", func(f *Function) { f.Handler = "" }, "no handler", "handler.handler"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := zipFn()
			c.mutate(&fn)
			_, err := BuildPlan(Discovery{Function: fn}, "shop")
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("want *Refusal, got %v", err)
			}
			if r.Function != "createOrder" {
				t.Errorf("refusal should name the function, got %q", r.Function)
			}
			if !strings.Contains(r.Reason, c.wantIn) {
				t.Errorf("reason %q should mention %q", r.Reason, c.wantIn)
			}
			if !strings.Contains(r.Fix, c.fixHas) {
				t.Errorf("fix %q should mention %q", r.Fix, c.fixHas)
			}
			if !strings.Contains(r.Error(), "fix:") {
				t.Error("Error() should surface the fix")
			}
		})
	}
}

func TestBuildPlanHTTPRoutes(t *testing.T) {
	d := Discovery{Function: zipFn(), Routes: []HTTPRoute{
		{Method: "post", Path: "orders/", PayloadFormat: "2.0"},
		{Method: "GET", Path: "/orders/{id}", PayloadFormat: "2.0"},
	}}
	p := mustPlan(t, d, "shop")

	if len(p.Triggers) != 2 {
		t.Fatalf("want 2 triggers, got %d: %+v", len(p.Triggers), p.Triggers)
	}
	// method upper-cased, path normalized (leading slash, no trailing slash)
	if p.Triggers[0].Method != "POST" || p.Triggers[0].Path != "/orders" {
		t.Errorf("first trigger = %+v", p.Triggers[0])
	}
	if p.Triggers[1].Path != "/orders/{id}" {
		t.Errorf("path param mangled: %q", p.Triggers[1].Path)
	}
	for _, tr := range p.Triggers {
		if tr.Provenance != Confirmed {
			t.Errorf("routes come from AWS and are facts, got %q", tr.Provenance)
		}
	}
}

// ANY is real in API Gateway but pulse routes one method at a time —
// expanding it loudly beats dropping methods silently.
func TestBuildPlanExpandsANY(t *testing.T) {
	d := Discovery{Function: zipFn(), Routes: []HTTPRoute{{Method: "ANY", Path: "/webhook"}}}
	p := mustPlan(t, d, "shop")

	if len(p.Triggers) != len(anyMethods) {
		t.Fatalf("want %d triggers for ANY, got %d", len(anyMethods), len(p.Triggers))
	}
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w.Subject, "ANY") {
			found = true
		}
	}
	if !found {
		t.Error("expanding ANY must be announced, not silent")
	}
}

func TestBuildPlanSQSTriggerAndQueue(t *testing.T) {
	d := Discovery{
		Function: zipFn(),
		EventSources: []EventSource{
			{Kind: "sqs", ARN: "arn:aws:sqs:eu-west-1:1234:order-events", BatchSize: 5, Enabled: true},
		},
		AllQueues: []Queue{
			{Name: "order-events", VisibilityTimeout: 45, DLQName: "order-events-dlq", MaxReceiveCount: 3},
		},
	}
	p := mustPlan(t, d, "shop")

	if len(p.Triggers) != 1 || p.Triggers[0].Kind != "sqs" {
		t.Fatalf("triggers = %+v", p.Triggers)
	}
	if p.Triggers[0].Queue != "order-events" || p.Triggers[0].BatchSize != 5 {
		t.Errorf("sqs trigger = %+v", p.Triggers[0])
	}
	// The real attributes must come across, and the DLQ must exist locally
	// or the retry path has nowhere to land.
	if len(p.Queues) != 2 {
		t.Fatalf("want queue + dlq, got %+v", p.Queues)
	}
	if p.Queues[0].VisibilityTimeout != 45 || p.Queues[0].MaxReceiveCount != 3 {
		t.Errorf("queue attributes not carried over: %+v", p.Queues[0])
	}
	if p.Queues[1].Name != "order-events-dlq" {
		t.Errorf("dlq missing, got %+v", p.Queues)
	}
}

func TestBuildPlanUnsupportedTriggersAreListed(t *testing.T) {
	d := Discovery{Function: zipFn(), EventSources: []EventSource{
		{Kind: "kinesis", ARN: "arn:aws:kinesis:::stream/clicks"},
		{Kind: "dynamodb-stream", ARN: "arn:aws:dynamodb:::table/orders/stream/x"},
	}}
	p := mustPlan(t, d, "shop")

	if len(p.Triggers) != 0 {
		t.Errorf("unsupported sources must not become triggers: %+v", p.Triggers)
	}
	if len(p.Unsupported) != 2 {
		t.Fatalf("both must be reported, got %+v", p.Unsupported)
	}
}

func TestBuildPlanSQSExtrasAreFlagged(t *testing.T) {
	d := Discovery{
		Function: zipFn(),
		EventSources: []EventSource{
			{Kind: "sqs", ARN: "arn:aws:sqs:::jobs", Enabled: false, HasFilter: true},
		},
		AllQueues: []Queue{{Name: "jobs", FIFO: true, VisibilityTimeout: 30}},
	}
	p := mustPlan(t, d, "shop")

	joined := notesText(p.Warnings) + notesText(p.Unsupported)
	for _, want := range []string{"DISABLED", "FIFO", "filter"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a note mentioning %q, got: %s", want, joined)
		}
	}
}

func TestBuildPlanCaveats(t *testing.T) {
	conc := int32(20)
	fn := zipFn()
	fn.Layers = []Layer{{ARN: "arn:aws:lambda:::layer:deps:3", Name: "deps", CodeURL: "https://presigned/layer.zip"}}
	fn.VPCAttached = true
	fn.Concurrency = &conc

	p := mustPlan(t, Discovery{Function: fn}, "shop")
	joined := notesText(p.Warnings) + notesText(p.Unsupported)
	for _, want := range []string{"layer", "VPC", "concurrency"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q to be reported, got: %s", want, joined)
		}
	}
	// Layers are a warning, not a refusal: the function may still run.
	if len(p.Functions) != 1 {
		t.Error("layers must not stop the import")
	}
}

// Out-of-range values from AWS are clamped to what pulse.yaml accepts, so a
// plan always validates.
func TestBuildPlanClampsBounds(t *testing.T) {
	fn := zipFn()
	fn.TimeoutSec = 0
	fn.MemoryMB = 64
	p := mustPlan(t, Discovery{Function: fn}, "shop")
	if p.Functions[0].TimeoutSec != 3 {
		t.Errorf("timeout = %d, want the 3s default", p.Functions[0].TimeoutSec)
	}
	if p.Functions[0].MemoryMB != 128 {
		t.Errorf("memory = %d, want clamped to 128", p.Functions[0].MemoryMB)
	}
}

func TestBuildPlanNamesAreSafe(t *testing.T) {
	fn := zipFn()
	fn.Name = "my-stack_Create Order.v2"
	p := mustPlan(t, Discovery{Function: fn}, "")
	got := p.Functions[0].Name
	if strings.ContainsAny(got, " .") {
		t.Errorf("name %q still has characters pulse.yaml/dirs shouldn't carry", got)
	}
	if p.Project == "" {
		t.Error("project must fall back to the function name")
	}
	// A project name is stricter than a function name — config.Validate wants
	// lowercase letters, digits and hyphens only.
	if !config.ValidProjectName(p.Project) {
		t.Errorf("project %q would fail pulse.yaml validation", p.Project)
	}
}

func TestSanitizeProjectAlwaysProducesAValidName(t *testing.T) {
	for _, in := range []string{
		"createOrder", "my-stack_Create Order.v2", "SHOUTING", "__weird__",
		"9lives", "-leading-dash-", "a", "····", "Order.Service--v2",
	} {
		got := sanitizeProject("", in)
		if !config.ValidProjectName(got) {
			t.Errorf("sanitizeProject(%q) = %q, which pulse.yaml would reject", in, got)
		}
	}
	if got := sanitizeProject("MyApp", "ignored"); got != "myapp" {
		t.Errorf("an explicit --name should still be normalized, got %q", got)
	}
}

// Reserved variables can't be carried across — AWS wouldn't allow them and
// pulse sets them itself — but the user must be told, not left guessing.
func TestBuildPlanFlagsReservedEnv(t *testing.T) {
	fn := zipFn()
	fn.Env = map[string]string{"AWS_REGION": "eu-west-1", "MY_KEY": "x"}
	p := mustPlan(t, Discovery{Function: fn}, "shop")
	if !strings.Contains(notesText(p.Warnings), "AWS_REGION") {
		t.Errorf("reserved env must be reported, got: %s", notesText(p.Warnings))
	}
}

// ---- inference ----

func TestInferResourcesStacksEvidence(t *testing.T) {
	tables := []Table{{Name: "orders"}, {Name: "sessions"}, {Name: "audit"}}
	queues := []Queue{{Name: "emails"}}
	env := map[string]string{
		"TABLE_NAME":  "orders",
		"EMAIL_QUEUE": "https://sqs.eu-west-1.amazonaws.com/1234/emails",
		"UNRELATED":   "hello",
	}
	policy := []PolicyStatement{
		{Effect: "Allow", Actions: []string{"dynamodb:PutItem"}, Resources: []string{"arn:aws:dynamodb:::table/orders"}},
		{Effect: "Allow", Actions: []string{"dynamodb:Scan"}, Resources: []string{"*"}}, // says nothing
		{Effect: "Deny", Actions: []string{"dynamodb:PutItem"}, Resources: []string{"arn:aws:dynamodb:::table/sessions"}},
	}
	code := `table = os.environ["TABLE_NAME"]  # "audit" is only mentioned here`

	got := InferResources(env, policy, code, tables, queues, nil)

	byName := map[string]Guess{}
	for _, g := range got {
		byName[g.Name] = g
	}

	orders, ok := byName["orders"]
	if !ok {
		t.Fatal("orders should be guessed from env + IAM")
	}
	if !orders.Strong || len(orders.Signals) < 2 {
		t.Errorf("orders should be a strong guess with both signals, got %+v", orders)
	}
	audit, ok := byName["audit"]
	if !ok {
		t.Fatal("audit should be guessed from the code mention")
	}
	if audit.Strong {
		t.Errorf("a code-only mention is weak evidence, got %+v", audit)
	}
	if _, leaked := byName["sessions"]; leaked {
		t.Error("a Deny statement must not produce a guess")
	}
	if q, ok := byName["emails"]; !ok || q.Kind != "queue" || !q.Strong {
		t.Errorf("emails queue should be strong from its URL env var, got %+v", q)
	}
	// Strong guesses sort first because they arrive pre-selected.
	if len(got) > 1 && !got[0].Strong {
		t.Errorf("strong guesses should sort first, got %+v", got)
	}
}

func TestInferResourcesSkipsWhatIsAlreadyCertain(t *testing.T) {
	got := InferResources(
		map[string]string{"QUEUE_NAME": "jobs"}, nil, "", nil,
		[]Queue{{Name: "jobs"}},
		map[string]bool{"jobs": true}, // already a confirmed trigger
	)
	if len(got) != 0 {
		t.Errorf("confirmed resources must not be offered as guesses: %+v", got)
	}
}

// A name that isn't in the account can't be described, so it must not be
// proposed — that's how you end up with a project that doesn't match AWS.
func TestInferResourcesIgnoresUnknownNames(t *testing.T) {
	got := InferResources(
		map[string]string{"TABLE_NAME": "from-another-region"}, nil, "",
		[]Table{{Name: "orders"}}, nil, nil,
	)
	for _, g := range got {
		if g.Name == "from-another-region" {
			t.Error("must not guess a resource that doesn't exist in this account/region")
		}
	}
}

func TestMentionsNameRespectsWordBoundaries(t *testing.T) {
	code := `dynamodb.Table("orders")  # orders_archive is a different table`
	if !mentionsName(code, "orders") {
		t.Error(`"orders" should match the quoted token`)
	}
	if mentionsName(`x = orders_archive_v2`, "orders") {
		t.Error(`"orders" must not match inside orders_archive_v2`)
	}
}

func TestArnTail(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sqs:eu-west-1:1234:order-events":       "order-events",
		"arn:aws:dynamodb:eu-west-1:1234:table/orders":  "orders",
		"https://sqs.eu-west-1.amazonaws.com/1234/jobs": "jobs",
		"plain-name": "plain-name",
		"":           "",
	}
	for in, want := range cases {
		if got := arnTail(in); got != want {
			t.Errorf("arnTail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAddTableCarriesRealSchemaAndFlagsIndexes(t *testing.T) {
	p := &Plan{}
	sk := Key{Name: "createdAt", Type: "S"}
	p.AddTable(Table{
		Name: "orders", PK: Key{Name: "customerId", Type: "S"}, SK: &sk,
		GSICount: 2, Streams: true,
	}, Picked, []string{"chosen from the account"})

	if len(p.Tables) != 1 {
		t.Fatalf("want 1 table, got %d", len(p.Tables))
	}
	got := p.Tables[0]
	if got.PK.Name != "customerId" || got.SK == nil || got.SK.Name != "createdAt" {
		t.Errorf("real schema not carried over: %+v", got)
	}
	if got.Provenance != Picked {
		t.Errorf("provenance = %q, want picked", got.Provenance)
	}
	joined := notesText(p.Unsupported)
	if !strings.Contains(joined, "index") || !strings.Contains(joined, "stream") {
		t.Errorf("indexes and streams must be reported, got: %s", joined)
	}

	p.AddTable(Table{Name: "orders"}, Guessed, nil) // duplicate
	if len(p.Tables) != 1 {
		t.Error("adding the same table twice must not duplicate it")
	}
}

func notesText(ns []Note) string {
	var b strings.Builder
	for _, n := range ns {
		b.WriteString(n.Subject + " " + n.Detail + "\n")
	}
	return b.String()
}

// A runtime newer than the CI matrix is imported, not refused — but it must
// arrive with a caveat rather than a silent assumption. This is the real case
// from P6 R0: a deployed function on python3.14.
func TestBuildPlanAcceptsNewerRuntimeWithACaveat(t *testing.T) {
	fn := zipFn()
	fn.Runtime = "python3.14"
	p, err := BuildPlan(Discovery{Function: fn, Region: "ap-south-1"}, "shop")
	if err != nil {
		t.Fatalf("a runtime above the floor must import, got: %v", err)
	}
	if p.Functions[0].Runtime != "python3.14" {
		t.Errorf("runtime = %q, want the real one preserved", p.Functions[0].Runtime)
	}
	notes := notesText(p.Warnings)
	if !strings.Contains(notes, "python3.14") || !strings.Contains(notes, "CI") {
		t.Errorf("an untested runtime should be flagged, got: %s", notes)
	}
	if !strings.Contains(notes, "interpreter") {
		t.Errorf("the note should mention needing a local interpreter, got: %s", notes)
	}
}

// The note has to distinguish a layer pulse merged from one it couldn't read:
// the first means the function should run, the second means it won't, and the
// user needs to know which they have.
func TestBuildPlanDistinguishesMergedAndUnreadableLayers(t *testing.T) {
	fn := zipFn()
	fn.Layers = []Layer{
		{ARN: "arn:…:layer:deps:9", Name: "deps", CodeURL: "https://presigned/deps.zip"},
		// Discovery knows WHY, and the reason is not always a permission —
		// telling someone to grant an IAM action for a layer another account
		// never shared with them is advice that cannot work.
		{ARN: "arn:…:layer:vendor:2", Name: "vendor", Unreadable: "that layer version no longer exists"},
		{ARN: "arn:…:layer:secret:2", Name: "secret"}, // no reason recorded
	}
	p := mustPlan(t, Discovery{Function: fn}, "shop")

	warnings, unsupported := notesText(p.Warnings), notesText(p.Unsupported)
	if !strings.Contains(warnings, "merged") || !strings.Contains(warnings, "deps") {
		t.Errorf("a merged layer should be reported as merged, got: %s", warnings)
	}
	// Each unreadable layer gets its own note carrying its own reason, so
	// two layers that failed differently don't get one blended explanation.
	for _, c := range []struct{ layer, reason string }{
		{"vendor", "no longer exists"},       // the reason discovery recorded
		{"secret", "lambda:GetLayerVersion"}, // none recorded — fall back to the permission
	} {
		n := findNote(p.Unsupported, c.layer)
		if n == nil {
			t.Errorf("no note for layer %s, got: %s", c.layer, unsupported)
			continue
		}
		if !strings.Contains(n.Detail, c.reason) {
			t.Errorf("note for %s should explain %q, got %q", c.layer, c.reason, n.Detail)
		}
	}
	if strings.Contains(warnings, "secret") || strings.Contains(warnings, "vendor") {
		t.Error("an unreadable layer must not be described as merged")
	}
}

// findNote returns the note whose subject mentions want, or nil.
func findNote(notes []Note, want string) *Note {
	for i := range notes {
		if strings.Contains(notes[i].Subject, want) {
			return &notes[i]
		}
	}
	return nil
}

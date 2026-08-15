// Package importer turns a real AWS account into a local pulse project.
//
// It is split deliberately: the types here and the mapper in plan.go are
// SDK-free and do no I/O, so the risky part — deciding what a Lambda
// actually is — is testable without credentials or a network. discover.go
// is the only file that talks to AWS, and it only ever reads.
//
// The central idea (PLAN §12.1) is provenance. AWS records what *triggers*
// a function but nothing about which tables its code touches, so every item
// pulse writes says how it was learned:
//
//	confirmed — AWS states it (event source mappings, API Gateway routes)
//	guessed   — inferred from IAM/env/code, shown with its evidence
//	picked    — chosen by the user from the real list in their account
//
// A guess is never presented as a fact, and nothing found-but-unsupported
// is ever dropped in silence.
package importer

// Provenance says how pulse learned about an item.
type Provenance string

const (
	Confirmed Provenance = "confirmed"
	Guessed   Provenance = "guessed"
	Picked    Provenance = "picked"
)

// ---- what discovery finds (neutral, SDK-free) ----

// Function is one Lambda as AWS describes it.
type Function struct {
	Name        string
	Runtime     string // e.g. python3.12, nodejs20.x, java17
	Handler     string
	TimeoutSec  int
	MemoryMB    int
	Env         map[string]string
	RoleARN     string
	PackageType string // "Zip" or "Image"
	CodeSize    int64  // bytes, as reported by AWS
	CodeURL     string // presigned download URL (empty for images)
	Layers      []string
	VPCAttached bool
	Concurrency *int32 // reserved concurrency, if set
}

// EventSource is one event source mapping (ListEventSourceMappings).
type EventSource struct {
	Kind      string // sqs | dynamodb-stream | kinesis | kafka | mq
	ARN       string // the source ARN
	BatchSize int
	Enabled   bool
	HasFilter bool // FilterCriteria set — pulse can't express these yet
}

// HTTPRoute is one API Gateway route pointing at the function.
type HTTPRoute struct {
	Method        string // GET, POST … ("ANY" is expanded by the mapper)
	Path          string // /orders/{id}
	APIID         string
	PayloadFormat string // "2.0" (HTTP API) or "1.0" (REST)
}

// Queue is an SQS queue as AWS describes it.
type Queue struct {
	Name              string
	ARN               string
	VisibilityTimeout int
	DLQName           string // resolved from the redrive policy, if any
	MaxReceiveCount   int
	FIFO              bool
}

// Table is a DynamoDB table as AWS describes it.
type Table struct {
	Name     string
	PK       Key
	SK       *Key
	GSICount int
	LSICount int
	Streams  bool
}

// Key is a DynamoDB key attribute.
type Key struct {
	Name string
	Type string // S | N | B
}

// PolicyStatement is the slice of an IAM policy the guesser needs: which
// actions are allowed on which resources.
type PolicyStatement struct {
	Effect    string // Allow | Deny
	Actions   []string
	Resources []string
}

// Discovery is everything read from one account+region for one function.
type Discovery struct {
	Region       string
	Function     Function
	EventSources []EventSource
	Routes       []HTTPRoute
	// Everything visible in the account, so the picker can offer reality
	// rather than asking the user to remember names (PLAN §12.10).
	AllTables []Table
	AllQueues []Queue
	// Optional extra signals; absent when permissions don't allow reading.
	RolePolicy []PolicyStatement
	CodeText   string // concatenated handler source, for a last-resort scan
}

// ---- what the mapper produces ----

// Note is something worth saying out loud: an unsupported feature, or a
// caveat about something that did import. Both end up on screen and in
// IMPORT-NOTES.md.
type Note struct {
	Subject string // what it's about ("layers", "runtime java17")
	Detail  string // what pulse will and won't do about it
}

// Guess is a resource the code probably uses, with the evidence for it.
// Strong guesses arrive pre-selected; weak ones are shown unchecked.
type Guess struct {
	Name    string
	Kind    string // table | queue
	Signals []string
	Strong  bool
}

// PlannedFunction is one function destined for pulse.yaml.
type PlannedFunction struct {
	Name       string
	Runtime    string
	Handler    string
	CodeDir    string
	TimeoutSec int
	MemoryMB   int
	// EnvNames are the variable names; values live in .env (placeholdered
	// unless --with-values), so secrets never land in the committed file.
	EnvNames   []string
	EnvValues  map[string]string
	CodeURL    string
	Provenance Provenance
}

// PlannedTrigger is one wiring entry (http or sqs).
type PlannedTrigger struct {
	Kind          string // http | sqs
	Function      string
	Method        string
	Path          string
	PayloadFormat string
	Queue         string
	BatchSize     int
	Provenance    Provenance
}

// PlannedQueue / PlannedTable carry the real definitions read from AWS.
type PlannedQueue struct {
	Name              string
	DLQ               string
	MaxReceiveCount   int
	VisibilityTimeout int
	Provenance        Provenance
	Signals           []string
}

type PlannedTable struct {
	Name       string
	PK         Key
	SK         *Key
	Provenance Provenance
	Signals    []string
}

// Plan is the reviewable result: exactly what pulse would write, why, and
// what it could not represent. Nothing is written until this is approved.
type Plan struct {
	Project   string
	Region    string
	Functions []PlannedFunction
	Triggers  []PlannedTrigger
	Tables    []PlannedTable
	Queues    []PlannedQueue
	Guesses   []Guess // unconfirmed candidates for the picker
	// RuntimeProvided are packages AWS's own runtime supplies (boto3,
	// @aws-sdk/*) that a laptop must install before the function will run.
	RuntimeProvided []string
	Unsupported     []Note // found, cannot be represented
	Warnings        []Note // imported, but with a caveat
}

// Refusal is returned when a function cannot be imported at all — a
// container image, an unsupported runtime, an oversized bundle. It carries
// the same shape as every other pulse error: what and what to do.
type Refusal struct {
	Function string
	Reason   string
	Fix      string
}

func (r *Refusal) Error() string {
	if r.Fix == "" {
		return r.Reason
	}
	return r.Reason + "\n    fix: " + r.Fix
}

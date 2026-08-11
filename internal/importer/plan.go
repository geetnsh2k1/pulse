package importer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// maxCodeSize refuses bundles pulse can't sensibly unpack into a project.
// AWS's own unzipped limit is 250 MB, so anything at that size is a
// deployment artifact, not something to develop against locally.
const maxCodeSize = 250 << 20

// BuildPlan turns one discovered function into a reviewable plan. It is
// pure: same input, same output, no I/O, no clock, no network — which is
// what makes the risky decisions here cheap to test exhaustively.
//
// A *Refusal is returned when the function cannot run locally at all.
// Everything softer becomes a Warning or an Unsupported note, so the user
// always sees the difference between "pulse won't do this" and "pulse did
// this, with a caveat".
func BuildPlan(d Discovery, project string) (*Plan, error) {
	fn := d.Function

	if err := refuse(fn); err != nil {
		return nil, err
	}

	p := &Plan{
		Project: sanitizeProject(project, fn.Name),
		Region:  d.Region,
	}

	// ---- the function itself ----
	envNames := sortedKeys(fn.Env)
	p.Functions = append(p.Functions, PlannedFunction{
		Name:       localName(fn.Name),
		Runtime:    fn.Runtime,
		Handler:    fn.Handler,
		CodeDir:    "functions/" + localName(fn.Name),
		TimeoutSec: clampTimeout(fn.TimeoutSec),
		MemoryMB:   clampMemory(fn.MemoryMB),
		EnvNames:   envNames,
		EnvValues:  fn.Env,
		CodeURL:    fn.CodeURL,
		Provenance: Confirmed,
	})

	// Reserved variables can't be carried over: AWS wouldn't let them be set
	// in the first place, and pulse sets them itself.
	for _, k := range envNames {
		if config.ReservedEnvKeys[k] {
			p.Warnings = append(p.Warnings, Note{
				Subject: "env " + k,
				Detail:  "reserved by the Lambda runtime — skipped; pulse sets it for you",
			})
		}
	}

	// ---- what AWS states as fact ----
	p.mapRoutes(d.Routes, localName(fn.Name))
	p.mapEventSources(d.EventSources, localName(fn.Name), d.AllQueues)

	// ---- what we can only infer ----
	p.Guesses = InferResources(fn.Env, d.RolePolicy, d.CodeText, d.AllTables, d.AllQueues, p.claimedQueues())

	// ---- caveats that don't stop the import ----
	if len(fn.Layers) > 0 {
		p.Warnings = append(p.Warnings, Note{
			Subject: fmt.Sprintf("%d layer(s)", len(fn.Layers)),
			Detail:  "layer contents are NOT merged — if dependencies live in a layer, install them into the function's directory before running",
		})
	}
	if fn.VPCAttached {
		p.Warnings = append(p.Warnings, Note{
			Subject: "VPC configuration",
			Detail:  "ignored locally — anything only reachable inside the VPC won't be reachable from your laptop",
		})
	}
	if fn.Concurrency != nil {
		p.Unsupported = append(p.Unsupported, Note{
			Subject: "reserved concurrency",
			Detail:  fmt.Sprintf("%d configured in AWS; pulse runs up to 4 workers per function", *fn.Concurrency),
		})
	}
	return p, nil
}

// refuse enforces the hard limits: a function pulse cannot run is refused
// with the reason and what to do, never imported into something broken.
func refuse(fn Function) error {
	if strings.EqualFold(fn.PackageType, "Image") {
		return &Refusal{Function: fn.Name,
			Reason: fmt.Sprintf("%q is a container-image function", fn.Name),
			Fix:    "pulse runs zip-based functions; import a zip-packaged function, or track container support on the roadmap"}
	}
	if !contains(config.SupportedRuntimes, fn.Runtime) {
		return &Refusal{Function: fn.Name,
			Reason: fmt.Sprintf("%q uses runtime %s, which pulse can't run", fn.Name, fn.Runtime),
			Fix:    "pulse supports " + strings.Join(config.SupportedRuntimes, ", ") + " today"}
	}
	if fn.CodeSize > maxCodeSize {
		return &Refusal{Function: fn.Name,
			Reason: fmt.Sprintf("%q ships %.0f MB of code (limit %d MB)", fn.Name, float64(fn.CodeSize)/(1<<20), maxCodeSize>>20),
			Fix:    "bundles this large are deployment artifacts — import a smaller function, or trim the package"}
	}
	if fn.Handler == "" {
		return &Refusal{Function: fn.Name,
			Reason: fmt.Sprintf("%q has no handler configured", fn.Name),
			Fix:    "check the function in the AWS console — pulse needs a handler like handler.handler"}
	}
	return nil
}

// anyMethods is what pulse writes when API Gateway says "ANY": one route
// per method it can actually express, rather than silently dropping some.
var anyMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

func (p *Plan) mapRoutes(routes []HTTPRoute, fnName string) {
	seen := map[string]bool{}
	for _, r := range routes {
		path := normalizePath(r.Path)
		methods := []string{strings.ToUpper(r.Method)}
		if methods[0] == "ANY" || methods[0] == "*" {
			methods = anyMethods
			p.Warnings = append(p.Warnings, Note{
				Subject: "route ANY " + path,
				Detail:  "expanded to " + strings.Join(anyMethods, ", ") + " — pulse routes one method at a time",
			})
		}
		for _, m := range methods {
			key := m + " " + path
			if seen[key] {
				continue
			}
			seen[key] = true
			p.Triggers = append(p.Triggers, PlannedTrigger{
				Kind: "http", Function: fnName, Method: m, Path: path,
				PayloadFormat: r.PayloadFormat, Provenance: Confirmed,
			})
		}
	}
}

func (p *Plan) mapEventSources(sources []EventSource, fnName string, allQueues []Queue) {
	for _, es := range sources {
		switch es.Kind {
		case "sqs":
			name := arnTail(es.ARN)
			p.Triggers = append(p.Triggers, PlannedTrigger{
				Kind: "sqs", Function: fnName, Queue: name,
				BatchSize: es.BatchSize, Provenance: Confirmed,
			})
			// The queue is a fact too, so take its real definition.
			if q, ok := findQueue(allQueues, name); ok {
				p.addQueue(q, Confirmed, []string{"event source mapping"})
			} else {
				p.Queues = append(p.Queues, PlannedQueue{
					Name: name, VisibilityTimeout: 30, Provenance: Confirmed,
					Signals: []string{"event source mapping (definition not readable)"},
				})
				p.Warnings = append(p.Warnings, Note{
					Subject: "queue " + name,
					Detail:  "triggers this function but its attributes weren't readable — defaults applied",
				})
			}
			if !es.Enabled {
				p.Warnings = append(p.Warnings, Note{
					Subject: "queue " + name,
					Detail:  "its event source mapping is DISABLED in AWS; locally it will deliver",
				})
			}
			if es.HasFilter {
				p.Unsupported = append(p.Unsupported, Note{
					Subject: "filter criteria on " + name,
					Detail:  "pulse delivers every message — filters aren't expressible yet, so local behavior is broader than production",
				})
			}
		default:
			p.Unsupported = append(p.Unsupported, Note{
				Subject: es.Kind + " trigger (" + arnTail(es.ARN) + ")",
				Detail:  "pulse supports http and sqs triggers today — this one is not imported",
			})
		}
	}
}

func (p *Plan) addQueue(q Queue, prov Provenance, signals []string) {
	for _, existing := range p.Queues {
		if existing.Name == q.Name {
			return
		}
	}
	if q.FIFO {
		p.Warnings = append(p.Warnings, Note{
			Subject: "queue " + q.Name,
			Detail:  "FIFO queue — pulse delivers in order but does not enforce message groups or deduplication",
		})
	}
	p.Queues = append(p.Queues, PlannedQueue{
		Name: q.Name, DLQ: q.DLQName, MaxReceiveCount: q.MaxReceiveCount,
		VisibilityTimeout: q.VisibilityTimeout, Provenance: prov, Signals: signals,
	})
	// A dead-letter queue referenced by a redrive policy must exist locally
	// too, or the retry path has nowhere to land.
	if q.DLQName != "" {
		already := false
		for _, existing := range p.Queues {
			if existing.Name == q.DLQName {
				already = true
			}
		}
		if !already {
			p.Queues = append(p.Queues, PlannedQueue{
				Name: q.DLQName, Provenance: prov,
				Signals: []string{"dead-letter queue of " + q.Name},
			})
		}
	}
}

// AddTable records a table the user confirmed or picked, using the real
// definition read from AWS.
func (p *Plan) AddTable(t Table, prov Provenance, signals []string) {
	for _, existing := range p.Tables {
		if existing.Name == t.Name {
			return
		}
	}
	if t.GSICount > 0 || t.LSICount > 0 {
		p.Unsupported = append(p.Unsupported, Note{
			Subject: fmt.Sprintf("%d secondary index(es) on %s", t.GSICount+t.LSICount, t.Name),
			Detail:  "pulse queries the base table only — code that queries an index will fail loudly, not silently",
		})
	}
	if t.Streams {
		p.Unsupported = append(p.Unsupported, Note{
			Subject: "stream on " + t.Name,
			Detail:  "DynamoDB Streams aren't supported — functions triggered by this stream won't fire locally",
		})
	}
	p.Tables = append(p.Tables, PlannedTable{
		Name: t.Name, PK: t.PK, SK: t.SK, Provenance: prov, Signals: signals,
	})
}

// AddQueue is AddTable's twin for queues chosen by the user.
func (p *Plan) AddQueue(q Queue, prov Provenance, signals []string) {
	p.addQueue(q, prov, signals)
}

// claimedQueues lists queues already in the plan as facts, so the guesser
// doesn't offer what is already certain.
func (p *Plan) claimedQueues() map[string]bool {
	out := map[string]bool{}
	for _, q := range p.Queues {
		out[q.Name] = true
	}
	return out
}

// ---- inference: the "guessed" tier ----

// tableEnvHint and queueEnvHint recognize the naming conventions people
// actually use for wiring resource names into a function's environment.
var (
	tableEnvHint = regexp.MustCompile(`(?i)(^|_)(table|ddb|dynamo)(_|$)|_TABLE$|^TABLE_`)
	queueEnvHint = regexp.MustCompile(`(?i)(^|_)(queue|sqs)(_|$)|_QUEUE(_URL|_NAME)?$|^QUEUE_`)
)

// InferResources proposes resources the function's code probably uses.
// AWS records nothing about this, so the evidence is stacked and reported:
// an IAM grant naming the resource is strong, an environment variable
// holding its name is strong, code mentioning it alone is weak. Two signals
// make a strong guess (pre-checked); one weak signal does not.
func InferResources(env map[string]string, policy []PolicyStatement, code string,
	allTables []Table, allQueues []Queue, alreadyCertain map[string]bool) []Guess {

	signals := map[string]map[string]bool{} // name -> set of signals
	kinds := map[string]string{}

	note := func(name, kind, signal string) {
		if name == "" || alreadyCertain[name] {
			return
		}
		if signals[name] == nil {
			signals[name] = map[string]bool{}
		}
		signals[name][signal] = true
		kinds[name] = kind
	}

	tableNames := map[string]bool{}
	for _, t := range allTables {
		tableNames[t.Name] = true
	}
	queueNames := map[string]bool{}
	for _, q := range allQueues {
		queueNames[q.Name] = true
	}

	// 1. Environment variables: the value usually *is* the resource name.
	for k, v := range env {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		name := v
		if strings.Contains(v, "/") { // queue URLs end in the queue name
			name = v[strings.LastIndex(v, "/")+1:]
		}
		switch {
		case tableNames[name]:
			note(name, "table", "env "+k)
		case queueNames[name]:
			note(name, "queue", "env "+k)
		case tableEnvHint.MatchString(k) || queueEnvHint.MatchString(k):
			// The name doesn't exist in this account/region — worth saying,
			// since it usually means a different region or a typo.
			continue
		}
	}

	// 2. IAM: an execution role that can write to a specific table is a
	// strong hint. Wildcards (Resource "*") say nothing, so they're ignored.
	for _, st := range policy {
		if !strings.EqualFold(st.Effect, "Allow") {
			continue
		}
		kind := ""
		for _, a := range st.Actions {
			switch {
			case strings.HasPrefix(strings.ToLower(a), "dynamodb:"):
				kind = "table"
			case strings.HasPrefix(strings.ToLower(a), "sqs:"):
				kind = "queue"
			}
		}
		if kind == "" {
			continue
		}
		for _, r := range st.Resources {
			if r == "*" || strings.HasSuffix(r, ":*") {
				continue
			}
			name := arnTail(r)
			if kind == "table" && tableNames[name] {
				note(name, "table", "role grants "+strings.Join(st.Actions[:min(2, len(st.Actions))], ", "))
			}
			if kind == "queue" && queueNames[name] {
				note(name, "queue", "role grants "+strings.Join(st.Actions[:min(2, len(st.Actions))], ", "))
			}
		}
	}

	// 3. The code itself: weakest signal, and only for names that exist.
	if code != "" {
		for name := range tableNames {
			if mentionsName(code, name) {
				note(name, "table", "name appears in the code")
			}
		}
		for name := range queueNames {
			if mentionsName(code, name) {
				note(name, "queue", "name appears in the code")
			}
		}
	}

	out := make([]Guess, 0, len(signals))
	for name, set := range signals {
		list := make([]string, 0, len(set))
		for s := range set {
			list = append(list, s)
		}
		sort.Strings(list)
		out = append(out, Guess{
			Name: name, Kind: kinds[name], Signals: list,
			Strong: len(list) > 1 || hasStrongSignal(list),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strong != out[j].Strong {
			return out[i].Strong // strong first: they're pre-selected
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// hasStrongSignal treats a single env-var or IAM hit as strong on its own —
// both mean someone deliberately wired that resource to this function.
func hasStrongSignal(list []string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, "env ") || strings.HasPrefix(s, "role grants") {
			return true
		}
	}
	return false
}

// mentionsName looks for the name as a quoted or delimited token, so
// "orders" doesn't match "orders_archive" or a stray substring.
func mentionsName(code, name string) bool {
	i := strings.Index(code, name)
	for i >= 0 {
		before := byte(' ')
		if i > 0 {
			before = code[i-1]
		}
		after := byte(' ')
		if end := i + len(name); end < len(code) {
			after = code[end]
		}
		if !isNameByte(before) && !isNameByte(after) {
			return true
		}
		next := strings.Index(code[i+1:], name)
		if next < 0 {
			return false
		}
		i = i + 1 + next
	}
	return false
}

func isNameByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ---- helpers ----

// localName keeps AWS names usable as pulse function names: AWS allows
// characters pulse.yaml keys and directories shouldn't carry.
var nonName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func localName(awsName string) string {
	// Strip the common "stack-Function-HASH" shape down to something a human
	// would type, but never to nothing.
	n := nonName.ReplaceAllString(awsName, "-")
	n = strings.Trim(n, "-")
	if n == "" {
		return "imported"
	}
	return n
}

func sanitizeProject(project, fnName string) string {
	if project != "" {
		return localName(project)
	}
	return localName(fnName)
}

// normalizePath makes API Gateway paths match pulse's router: a leading
// slash, no trailing slash, {proxy+} preserved.
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		p = "/"
	}
	return p
}

// arnTail returns the last meaningful segment of an ARN or URL: the queue or
// table name.
func arnTail(s string) string {
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

func findQueue(qs []Queue, name string) (Queue, bool) {
	for _, q := range qs {
		if q.Name == name {
			return q, true
		}
	}
	return Queue{}, false
}

func clampTimeout(v int) int {
	switch {
	case v < config.MinTimeout:
		return 3
	case v > config.MaxTimeout:
		return config.MaxTimeout
	}
	return v
}

func clampMemory(v int) int {
	switch {
	case v < config.MinMemory:
		return config.MinMemory
	case v > config.MaxMemory:
		return config.MaxMemory
	}
	return v
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

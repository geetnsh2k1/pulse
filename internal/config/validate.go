package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Problem is a single validation finding, pointing at a config path.
type Problem struct {
	Path string // e.g. "functions.createOrder.runtime"
	Msg  string
}

// ValidationError aggregates every problem found in one pass, so users fix
// their config once instead of playing whack-a-mole.
type ValidationError struct {
	File     string
	Problems []Problem
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	plural := "s"
	if len(e.Problems) == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "%s: %d problem%s found", e.File, len(e.Problems), plural)
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  ✗ %s: %s", p.Path, p.Msg)
	}
	return b.String()
}

var (
	projectNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	nodeHandlerRe = regexp.MustCompile(`^[\w./-]+\.[\w$]+$`)
	pyHandlerRe   = regexp.MustCompile(`^[A-Za-z_]\w*(\.[A-Za-z_]\w*)+$`)
	envNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	bucketNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

	httpMethods  = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true, "ANY": true}
	triggerTypes = []string{"http", "sqs", "sns", "s3", "dynamodb-stream"}
	s3EventKinds = map[string]bool{"created": true, "removed": true}
	keyTypes     = map[string]bool{"S": true, "N": true, "B": true}
)

// ValidProjectName reports whether s can be used as a project name
// (lowercase letters, digits, hyphens).
func ValidProjectName(s string) bool { return projectNameRe.MatchString(s) }

// Validate checks the whole config. Root must be set for codeDir existence
// checks (Load sets it; tests set it explicitly).
func (c *Config) Validate() error {
	v := &validator{cfg: c}
	v.run()
	if len(v.problems) == 0 {
		return nil
	}
	file := c.Path
	if file == "" {
		file = FileName
	}
	return &ValidationError{File: file, Problems: v.problems}
}

type validator struct {
	cfg      *Config
	problems []Problem
}

func (v *validator) addf(path, format string, args ...any) {
	v.problems = append(v.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) run() {
	c := v.cfg
	if c.Project == "" {
		v.addf("project", "required — a short name like %q", "order-demo")
	} else if !projectNameRe.MatchString(c.Project) {
		v.addf("project", "%q must be lowercase letters, digits, and hyphens", c.Project)
	}
	if len(c.Functions) == 0 {
		v.addf("functions", "at least one function is required")
	}
	if c.API.Port < 0 || c.API.Port > 65535 {
		v.addf("api.port", "%d is out of range [0, 65535]", c.API.Port)
	}
	v.functions()
	v.triggers()
	v.resources()
}

func (v *validator) functions() {
	c := v.cfg
	for _, name := range c.FunctionNames() {
		fn := c.Functions[name]
		p := "functions." + name

		if fn.Runtime == "" {
			v.addf(p+".runtime", "required — one of: %s", strings.Join(SupportedRuntimes, ", "))
		} else if !contains(SupportedRuntimes, fn.Runtime) {
			v.addf(p+".runtime", "%q is not a supported runtime%s (supported: %s)",
				fn.Runtime, didYouMean(fn.Runtime, SupportedRuntimes), strings.Join(SupportedRuntimes, ", "))
		}

		family := RuntimeFamily(fn.Runtime)
		switch {
		case fn.Handler == "":
			v.addf(p+".handler", "required — e.g. %q for Node or %q for Python", "index.handler", "handler.handler")
		case family == "node" && !nodeHandlerRe.MatchString(fn.Handler):
			v.addf(p+".handler", "%q is not a valid Node handler (expected file.export, e.g. %q)", fn.Handler, "src/orders.create")
		case family == "python" && !pyHandlerRe.MatchString(fn.Handler):
			v.addf(p+".handler", "%q is not a valid Python handler (expected module.function, e.g. %q)", fn.Handler, "worker.handler.process")
		}

		switch {
		case filepath.IsAbs(fn.CodeDir):
			v.addf(p+".codeDir", "must be relative to the project root, got absolute path %q", fn.CodeDir)
		case strings.HasPrefix(filepath.Clean(fn.CodeDir), ".."):
			v.addf(p+".codeDir", "%q escapes the project root", fn.CodeDir)
		case c.Root != "":
			dir := filepath.Join(c.Root, fn.CodeDir)
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				v.addf(p+".codeDir", "directory %q not found in project", fn.CodeDir)
			}
		}

		// AWS rejects reserved variables in function configuration; so do we,
		// and for a sharper reason: AWS_ENDPOINT_URL is what points the SDK
		// at the local façade. Silently ignoring the key would leave someone
		// debugging why their override "did nothing".
		for _, k := range sortedKeys(fn.Env) {
			if ReservedEnvKeys[k] {
				v.addf(p+".env."+k, "%q is reserved by the Lambda runtime and cannot be set (AWS rejects it too) — remove it; pulse sets it for you", k)
			}
		}

		if fn.Timeout < MinTimeout || fn.Timeout > MaxTimeout {
			v.addf(p+".timeout", "%d is out of range [%d, %d] seconds", fn.Timeout, MinTimeout, MaxTimeout)
		}
		if fn.Memory < MinMemory || fn.Memory > MaxMemory {
			v.addf(p+".memory", "%d is out of range [%d, %d] MB", fn.Memory, MinMemory, MaxMemory)
		}
		for k := range fn.Env {
			if !envNameRe.MatchString(k) {
				v.addf(p+".env", "%q is not a valid environment variable name", k)
			}
		}
	}
}

func (v *validator) triggers() {
	c := v.cfg
	fnNames := c.FunctionNames()
	seenRoutes := map[string]int{}

	for i, t := range c.Triggers {
		p := fmt.Sprintf("triggers[%d]", i)
		if t == nil {
			v.addf(p, "empty trigger entry")
			continue
		}
		if t.Type == "" {
			v.addf(p+".type", "required — one of: %s", strings.Join(triggerTypes, ", "))
			continue
		}
		if !contains(triggerTypes, t.Type) {
			v.addf(p+".type", "%q is not a known trigger type%s (known: %s)",
				t.Type, didYouMean(t.Type, triggerTypes), strings.Join(triggerTypes, ", "))
			continue
		}

		if t.Function == "" {
			v.addf(p+".function", "required")
		} else if _, ok := c.Functions[t.Function]; !ok {
			v.addf(p+".function", "unknown function %q%s", t.Function, didYouMean(t.Function, fnNames))
		}

		switch t.Type {
		case "http":
			if t.Method == "" {
				v.addf(p+".method", "required for http triggers (GET, POST, ..., or ANY)")
			} else if !httpMethods[t.Method] {
				v.addf(p+".method", "%q is not a valid HTTP method", t.Method)
			}
			if t.PayloadFormat != "2.0" && t.PayloadFormat != "1.0" {
				v.addf(p+".payloadFormat", "%q is not valid (valid: \"2.0\", \"1.0\")", t.PayloadFormat)
			}
			if t.Path == "" {
				v.addf(p+".path", "required for http triggers, e.g. %q", "/orders")
			} else if msg := validHTTPPath(t.Path); msg != "" {
				v.addf(p+".path", "%q: %s", t.Path, msg)
			} else if t.Method != "" {
				route := t.Method + " " + t.Path
				if prev, dup := seenRoutes[route]; dup {
					v.addf(p, "duplicate route %q (already used by triggers[%d])", route, prev)
				} else {
					seenRoutes[route] = i
				}
			}
		case "sqs":
			if t.Queue == "" {
				v.addf(p+".queue", "required for sqs triggers")
			} else if _, ok := c.Resources.Queues[t.Queue]; !ok {
				v.addf(p+".queue", "unknown queue %q%s — declare it under resources.queues", t.Queue, didYouMean(t.Queue, keys(c.Resources.Queues)))
			}
			if t.BatchSize < 1 || t.BatchSize > 10000 {
				v.addf(p+".batchSize", "%d is out of range [1, 10000]", t.BatchSize)
			}
		case "sns":
			if t.Topic == "" {
				v.addf(p+".topic", "required for sns triggers")
			} else if _, ok := c.Resources.Topics[t.Topic]; !ok {
				v.addf(p+".topic", "unknown topic %q%s — declare it under resources.topics", t.Topic, didYouMean(t.Topic, keys(c.Resources.Topics)))
			}
		case "s3":
			if t.Bucket == "" {
				v.addf(p+".bucket", "required for s3 triggers")
			} else if !contains(c.Resources.Buckets, t.Bucket) {
				v.addf(p+".bucket", "unknown bucket %q%s — declare it under resources.buckets", t.Bucket, didYouMean(t.Bucket, c.Resources.Buckets))
			}
			for _, ev := range t.Events {
				if !s3EventKinds[ev] {
					v.addf(p+".events", "%q is not a valid s3 event (valid: created, removed)", ev)
				}
			}
		case "dynamodb-stream":
			if t.Table == "" {
				v.addf(p+".table", "required for dynamodb-stream triggers")
			} else if tb, ok := c.Resources.Tables[t.Table]; !ok {
				v.addf(p+".table", "unknown table %q%s — declare it under resources.tables", t.Table, didYouMean(t.Table, keys(c.Resources.Tables)))
			} else if !tb.Streams {
				v.addf(p+".table", "table %q has streams disabled — set resources.tables.%s.streams: true", t.Table, t.Table)
			}
		}
	}
}

func (v *validator) resources() {
	c := v.cfg
	for name, tb := range c.Resources.Tables {
		p := "resources.tables." + name
		if tb.PK.Type == "" {
			tb.PK.Type = "S"
		}
		if tb.SK != nil && tb.SK.Type == "" {
			tb.SK.Type = "S"
		}
		if tb.PK.Name == "" {
			v.addf(p+".pk", "required — shorthand `pk: id` or full `pk: { name: id, type: S }`")
		} else if !keyTypes[tb.PK.Type] {
			v.addf(p+".pk.type", "%q is not a valid key type (valid: S, N, B)", tb.PK.Type)
		}
		if tb.SK != nil {
			if tb.SK.Name == "" {
				v.addf(p+".sk.name", "required when sk is set")
			} else if !keyTypes[tb.SK.Type] {
				v.addf(p+".sk.type", "%q is not a valid key type (valid: S, N, B)", tb.SK.Type)
			}
		}
	}

	seenBuckets := map[string]bool{}
	for i, b := range c.Resources.Buckets {
		p := fmt.Sprintf("resources.buckets[%d]", i)
		if !bucketNameRe.MatchString(b) {
			v.addf(p, "%q is not a valid bucket name (lowercase letters, digits, dots, hyphens; 3–63 chars)", b)
		}
		if seenBuckets[b] {
			v.addf(p, "duplicate bucket %q", b)
		}
		seenBuckets[b] = true
	}

	for name, q := range c.Resources.Queues {
		p := "resources.queues." + name
		if q.DLQ != "" {
			if q.DLQ == name {
				v.addf(p+".dlq", "queue cannot be its own dead-letter queue")
			} else if _, ok := c.Resources.Queues[q.DLQ]; !ok {
				v.addf(p+".dlq", "unknown queue %q%s — declare it under resources.queues", q.DLQ, didYouMean(q.DLQ, keys(c.Resources.Queues)))
			}
			if q.MaxReceiveCount < 1 {
				v.addf(p+".maxReceiveCount", "must be ≥ 1 when a dlq is set")
			}
		}
		if q.VisibilityTimeout < 0 || q.VisibilityTimeout > 43200 {
			v.addf(p+".visibilityTimeout", "%d is out of range [0, 43200] seconds", q.VisibilityTimeout)
		}
	}

	fnNames := v.cfg.FunctionNames()
	for name, tp := range c.Resources.Topics {
		p := "resources.topics." + name
		for _, sub := range tp.Subscribers {
			if _, ok := c.Functions[sub]; !ok {
				v.addf(p+".subscribers", "unknown function %q%s", sub, didYouMean(sub, fnNames))
			}
		}
	}
}

func validHTTPPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "must start with /"
	}
	if strings.ContainsAny(p, " \t") {
		return "must not contain whitespace"
	}
	if p == "/" {
		return ""
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, seg := range segs {
		if seg == "" {
			return "has an empty path segment (double or trailing slash?)"
		}
		open := strings.HasPrefix(seg, "{")
		closed := strings.HasSuffix(seg, "}")
		if open != closed || (open && len(seg) < 3) {
			return fmt.Sprintf("malformed parameter segment %q (expected {name})", seg)
		}
		if open && strings.HasSuffix(seg, "+}") && i != len(segs)-1 {
			return fmt.Sprintf("greedy segment %q must be the last path segment", seg)
		}
	}
	return ""
}

// sortedKeys keeps validation messages deterministic across runs.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

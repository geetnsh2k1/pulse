// Package config loads and validates pulse.yaml, the source of truth for a
// pulse project: its functions, triggers, and local AWS resources.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/geetnsh2k1/pulse/internal/dotenv"
)

// FileName is the project definition file pulse looks for.
const FileName = "pulse.yaml"

// DotEnvFile holds local, uncommitted values (secrets, per-machine
// overrides) beside pulse.yaml. Optional — projects work without it.
const DotEnvFile = ".env"

// SupportedRuntimes is the set of Lambda runtimes pulse certifies — the
// same matrix scripts/e2e.sh exercises in CI (Python 3.10+, Node 18+).
// python3.9 was dropped once AWS retired it; python3.13 is current.
var SupportedRuntimes = []string{
	"nodejs18.x", "nodejs20.x", "nodejs22.x",
	"python3.10", "python3.11", "python3.12", "python3.13",
}

// ReservedEnvKeys are variables a project may not set. AWS Lambda rejects
// these in function configuration, and pulse has the same reason to: they
// carry the runtime protocol and the local AWS façade. Letting a project
// file override AWS_ENDPOINT_URL would quietly point the SDK away from the
// local cloud — exactly the kind of silent wrongness pulse refuses.
var ReservedEnvKeys = map[string]bool{
	// reserved by AWS Lambda itself
	"_HANDLER": true, "_X_AMZN_TRACE_ID": true,
	"AWS_REGION": true, "AWS_DEFAULT_REGION": true, "AWS_EXECUTION_ENV": true,
	"AWS_LAMBDA_FUNCTION_NAME": true, "AWS_LAMBDA_FUNCTION_MEMORY_SIZE": true,
	"AWS_LAMBDA_FUNCTION_VERSION": true, "AWS_LAMBDA_INITIALIZATION_TYPE": true,
	"AWS_LAMBDA_LOG_GROUP_NAME": true, "AWS_LAMBDA_LOG_STREAM_NAME": true,
	"AWS_LAMBDA_RUNTIME_API": true, "LAMBDA_TASK_ROOT": true,
	"LAMBDA_RUNTIME_DIR": true,
	"AWS_ACCESS_KEY":     true, "AWS_ACCESS_KEY_ID": true,
	"AWS_SECRET_KEY": true, "AWS_SECRET_ACCESS_KEY": true,
	"AWS_SESSION_TOKEN": true,
	// pulse's local wiring
	"AWS_ENDPOINT_URL": true, "AWS_ENDPOINT_URL_SQS": true,
	"AWS_ENDPOINT_URL_DYNAMODB": true, "PULSE_WORKER_ID": true,
}

// Bounds mirroring AWS Lambda's own limits.
const (
	MinTimeout = 1
	MaxTimeout = 900 // seconds
	MinMemory  = 128
	MaxMemory  = 10240 // MB
)

type Config struct {
	Project   string               `yaml:"project" json:"project"`
	Region    string               `yaml:"region" json:"region"`
	API       APIConfig            `yaml:"api" json:"api"`
	Functions map[string]*Function `yaml:"functions" json:"functions"`
	Triggers  []*Trigger           `yaml:"triggers" json:"triggers"`
	Resources Resources            `yaml:"resources" json:"resources"`

	// Root is the absolute path of the project directory (dir of pulse.yaml).
	Root string `yaml:"-" json:"root"`

	// DotEnv holds variables read from .env next to pulse.yaml. It is never
	// serialized (yaml:"-") — secrets must not leak into the committed file
	// when `pulse add`/`remove` rewrite it. Functions receive these as a
	// base layer; a function's own `env:` overrides them.
	DotEnv map[string]string `yaml:"-" json:"-"`
	// Path is the absolute path of the loaded pulse.yaml.
	Path string `yaml:"-" json:"-"`
}

// APIConfig configures the local HTTP front door (phase 2+).
type APIConfig struct {
	// Port for the local API server. Defaults to 3000; 0 picks a random
	// free port (useful in tests). Only bound when http triggers exist.
	Port int `yaml:"port" json:"port"`
}

type Function struct {
	Name    string            `yaml:"-" json:"name"` // filled from the map key
	Runtime string            `yaml:"runtime" json:"runtime"`
	Handler string            `yaml:"handler" json:"handler"`
	CodeDir string            `yaml:"codeDir" json:"codeDir"`
	Timeout int               `yaml:"timeout" json:"timeout"` // seconds; enforced at invoke
	Memory  int               `yaml:"memory" json:"memory"`   // MB; env/context parity in MVP
	Env     map[string]string `yaml:"env" json:"env,omitempty"`
}

// Trigger wires an event source to a function. Only the fields belonging to
// its Type may be set; validation rejects everything else.
type Trigger struct {
	Type     string `yaml:"type" json:"type"` // http | sqs | sns | s3 | dynamodb-stream
	Function string `yaml:"function" json:"function"`

	// http
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	// PayloadFormat picks the proxy-event shape handed to the function:
	// "2.0" (HTTP API, default) or "1.0" (REST API).
	PayloadFormat string `yaml:"payloadFormat,omitempty" json:"payloadFormat,omitempty"`

	// sqs
	Queue     string `yaml:"queue,omitempty" json:"queue,omitempty"`
	BatchSize int    `yaml:"batchSize,omitempty" json:"batchSize,omitempty"`

	// sns
	Topic string `yaml:"topic,omitempty" json:"topic,omitempty"`

	// s3
	Bucket string   `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Events []string `yaml:"events,omitempty" json:"events,omitempty"` // created | removed

	// dynamodb-stream
	Table string `yaml:"table,omitempty" json:"table,omitempty"`
}

type Resources struct {
	Tables  map[string]*Table `yaml:"tables" json:"tables,omitempty"`
	Buckets []string          `yaml:"buckets" json:"buckets,omitempty"`
	Queues  map[string]*Queue `yaml:"queues" json:"queues,omitempty"`
	Topics  map[string]*Topic `yaml:"topics" json:"topics,omitempty"`
}

type Table struct {
	Name    string  `yaml:"-" json:"name"`
	PK      KeyDef  `yaml:"pk" json:"pk"`
	SK      *KeyDef `yaml:"sk,omitempty" json:"sk,omitempty"`
	Streams bool    `yaml:"streams" json:"streams"`
}

type KeyDef struct {
	Name string `yaml:"name" json:"name"`
	Type string `yaml:"type" json:"type"` // S | N | B
}

// UnmarshalYAML accepts both the full form `{ name: id, type: S }` and the
// shorthand `pk: id` (type defaults to S) — most tables only need the latter.
func (k *KeyDef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		k.Name = value.Value
		k.Type = "S"
		return nil
	}
	type plain KeyDef
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*k = KeyDef(p)
	if k.Type == "" {
		k.Type = "S"
	}
	return nil
}

type Queue struct {
	Name              string `yaml:"-" json:"name"`
	DLQ               string `yaml:"dlq,omitempty" json:"dlq,omitempty"`
	MaxReceiveCount   int    `yaml:"maxReceiveCount,omitempty" json:"maxReceiveCount,omitempty"`
	VisibilityTimeout int    `yaml:"visibilityTimeout,omitempty" json:"visibilityTimeout,omitempty"` // seconds
}

type Topic struct {
	Name        string   `yaml:"-" json:"name"`
	Subscribers []string `yaml:"subscribers" json:"subscribers,omitempty"`
}

// Find walks from dir upward looking for pulse.yaml and returns its path.
func Find(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(abs, FileName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no %s found in %s or any parent directory — run `pulse init <name>` to create a project", FileName, dir)
		}
		abs = parent
	}
}

// Load reads, strictly decodes, defaults, and validates a pulse.yaml.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	cfg.Path = abs
	cfg.Root = filepath.Dir(abs)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// .env sits beside pulse.yaml and holds what pulse.yaml must not: local
	// secrets. Kept separate from Function.Env so a value can never be
	// written back into the committed file (see DotEnv).
	vars, err := dotenv.Load(filepath.Join(cfg.Root, DotEnvFile))
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(vars) {
		if ReservedEnvKeys[k] {
			return nil, fmt.Errorf("%s: %q is reserved by the Lambda runtime and cannot be set (AWS rejects it too) — remove it; pulse sets it for you", DotEnvFile, k)
		}
	}
	cfg.DotEnv = vars
	return cfg, nil
}

// Parse decodes (rejecting unknown keys) and applies defaults, but does not
// validate. Set Root before calling Validate; Load does the whole dance.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty", FileName)
		}
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	for name, fn := range c.Functions {
		if fn == nil {
			fn = &Function{}
			c.Functions[name] = fn
		}
		fn.Name = name
		if fn.CodeDir == "" {
			fn.CodeDir = "."
		}
		if fn.Timeout == 0 {
			fn.Timeout = 3
		}
		if fn.Memory == 0 {
			fn.Memory = 128
		}
	}
	if c.API.Port == 0 {
		c.API.Port = 3000
	}
	for _, t := range c.Triggers {
		if t == nil {
			continue
		}
		t.Type = strings.ToLower(strings.TrimSpace(t.Type))
		t.Method = strings.ToUpper(strings.TrimSpace(t.Method))
		if t.Type == "http" && t.PayloadFormat == "" {
			t.PayloadFormat = "2.0"
		}
		if t.Type == "sqs" && t.BatchSize == 0 {
			t.BatchSize = 10
		}
		if t.Type == "s3" && len(t.Events) == 0 {
			t.Events = []string{"created"}
		}
	}
	for name, tb := range c.Resources.Tables {
		if tb == nil {
			tb = &Table{}
			c.Resources.Tables[name] = tb
		}
		tb.Name = name
	}
	for name, q := range c.Resources.Queues {
		if q == nil {
			q = &Queue{}
			c.Resources.Queues[name] = q
		}
		q.Name = name
		if q.VisibilityTimeout == 0 {
			q.VisibilityTimeout = 30
		}
		if q.DLQ != "" && q.MaxReceiveCount == 0 {
			q.MaxReceiveCount = 3
		}
	}
	for name, tp := range c.Resources.Topics {
		if tp == nil {
			tp = &Topic{}
			c.Resources.Topics[name] = tp
		}
		tp.Name = name
	}
}

// FunctionNames returns all function names, sorted.
func (c *Config) FunctionNames() []string {
	names := make([]string, 0, len(c.Functions))
	for n := range c.Functions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RuntimeFamily maps a runtime identifier to its adapter family.
func RuntimeFamily(runtime string) string {
	switch {
	case strings.HasPrefix(runtime, "nodejs"):
		return "node"
	case strings.HasPrefix(runtime, "python"):
		return "python"
	}
	return ""
}

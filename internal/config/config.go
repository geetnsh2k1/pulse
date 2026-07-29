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
)

// FileName is the project definition file pulse looks for.
const FileName = "pulse.yaml"

// SupportedRuntimes is the set of Lambda runtimes certified for the MVP.
var SupportedRuntimes = []string{
	"nodejs18.x", "nodejs20.x", "nodejs22.x",
	"python3.9", "python3.10", "python3.11", "python3.12",
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
	Functions map[string]*Function `yaml:"functions" json:"functions"`
	Triggers  []*Trigger           `yaml:"triggers" json:"triggers"`
	Resources Resources            `yaml:"resources" json:"resources"`

	// Root is the absolute path of the project directory (dir of pulse.yaml).
	Root string `yaml:"-" json:"root"`
	// Path is the absolute path of the loaded pulse.yaml.
	Path string `yaml:"-" json:"-"`
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
	for _, t := range c.Triggers {
		if t == nil {
			continue
		}
		t.Type = strings.ToLower(strings.TrimSpace(t.Type))
		t.Method = strings.ToUpper(strings.TrimSpace(t.Method))
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

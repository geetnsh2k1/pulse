package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EditYAML surgically modifies pulse.yaml: it parses the file into a YAML
// node tree (comments and ordering survive), applies fn to the root mapping,
// validates the result through the normal strict loader, and only then
// writes it back. On any failure the file is left untouched.
func EditYAML(path string, fn func(root *yaml.Node) error) error {
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a YAML mapping", path)
	}
	if err := fn(doc.Content[0]); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	_ = enc.Close()

	// Round-trip through the strict loader before committing.
	cfg, err := Parse(buf.Bytes())
	if err != nil {
		return fmt.Errorf("the edit would produce an invalid config: %w", err)
	}
	cfg.Path = path
	cfg.Root = filepath.Dir(path)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("the edit would produce an invalid config:\n%w", err)
	}

	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// TopMap finds (or appends) a top-level mapping section like "functions".
func TopMap(root *yaml.Node, key string) *yaml.Node {
	if n := mapValue(root, key); n != nil {
		if n.Kind == yaml.MappingNode {
			return n
		}
		// e.g. `resources:` written as null — replace with a mapping.
		*n = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		return n
	}
	val := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendMapEntry(root, key, val)
	return val
}

// TopSeq finds (or appends) a top-level sequence section like "triggers".
func TopSeq(root *yaml.Node, key string) *yaml.Node {
	if n := mapValue(root, key); n != nil {
		if n.Kind == yaml.SequenceNode {
			return n
		}
		*n = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		return n
	}
	val := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	appendMapEntry(root, key, val)
	return val
}

// HasMapKey reports whether a mapping already contains key.
func HasMapKey(m *yaml.Node, key string) bool { return mapValue(m, key) != nil }

// SetMapEntry appends key: value (encoded from v) to a mapping node.
func SetMapEntry(m *yaml.Node, key string, v any) error {
	val, err := NodeOf(v)
	if err != nil {
		return err
	}
	appendMapEntry(m, key, val)
	return nil
}

// AppendSeq appends an element (encoded from v) to a sequence node.
func AppendSeq(seq *yaml.Node, v any) error {
	val, err := NodeOf(v)
	if err != nil {
		return err
	}
	seq.Content = append(seq.Content, val)
	return nil
}

// NodeOf encodes any Go value as a YAML node.
func NodeOf(v any) (*yaml.Node, error) {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return n, nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func appendMapEntry(m *yaml.Node, key string, val *yaml.Node) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val)
}

// RemoveMapEntry deletes key (and its value) from a mapping node.
// Returns false when the key wasn't present.
func RemoveMapEntry(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// FilterSeq keeps only the elements keep() approves and returns how many
// were removed.
func FilterSeq(seq *yaml.Node, keep func(*yaml.Node) bool) int {
	kept := seq.Content[:0]
	removed := 0
	for _, n := range seq.Content {
		if keep(n) {
			kept = append(kept, n)
		} else {
			removed++
		}
	}
	seq.Content = kept
	return removed
}

// MapScalar reads a mapping node's scalar field ("" when absent) — for
// inspecting trigger entries during removals.
func MapScalar(m *yaml.Node, key string) string {
	if m.Kind != yaml.MappingNode {
		return ""
	}
	if v := mapValue(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

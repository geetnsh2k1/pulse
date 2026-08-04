package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKeyShorthand(t *testing.T) {
	root := writeProject(t, `
project: short
functions:
  fn:
    runtime: nodejs20.x
    handler: index.handler
    codeDir: fn
resources:
  tables:
    users:
      pk: id
    events:
      pk: userId
      sk: seq
`, "fn")
	cfg, err := Load(filepath.Join(root, FileName))
	if err != nil {
		t.Fatalf("shorthand keys rejected: %v", err)
	}
	users := cfg.Resources.Tables["users"]
	if users.PK.Name != "id" || users.PK.Type != "S" {
		t.Errorf("users pk = %+v", users.PK)
	}
	events := cfg.Resources.Tables["events"]
	if events.SK == nil || events.SK.Name != "seq" || events.SK.Type != "S" {
		t.Errorf("events sk = %+v", events.SK)
	}
}

func TestEditYAMLPreservesCommentsAndValidates(t *testing.T) {
	root := writeProject(t, `# my precious comment
project: editable

functions:
  fn:
    runtime: nodejs20.x   # keep me too
    handler: index.handler
    codeDir: fn
`, "fn", "services/newfn")
	path := filepath.Join(root, FileName)

	err := EditYAML(path, func(rootNode *yaml.Node) error {
		functions := TopMap(rootNode, "functions")
		if err := SetMapEntry(functions, "newfn", map[string]any{
			"runtime": "python3.12", "handler": "handler.handle", "codeDir": "services/newfn",
		}); err != nil {
			return err
		}
		return AppendSeq(TopSeq(rootNode, "triggers"), map[string]any{
			"type": "http", "method": "GET", "path": "/new", "function": "newfn",
		})
	})
	if err != nil {
		t.Fatalf("EditYAML: %v", err)
	}

	raw, _ := os.ReadFile(path)
	text := string(raw)
	if !strings.Contains(text, "my precious comment") || !strings.Contains(text, "keep me too") {
		t.Errorf("comments were lost:\n%s", text)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("edited config invalid: %v", err)
	}
	if _, ok := cfg.Functions["newfn"]; !ok || len(cfg.Triggers) != 1 {
		t.Errorf("edit not applied: %+v", cfg)
	}

	// An edit that would break the config must leave the file untouched.
	before, _ := os.ReadFile(path)
	err = EditYAML(path, func(rootNode *yaml.Node) error {
		return AppendSeq(TopSeq(rootNode, "triggers"), map[string]any{
			"type": "http", "method": "GET", "path": "/bad", "function": "ghost",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "invalid config") {
		t.Fatalf("bad edit not rejected: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("file changed despite a rejected edit")
	}
}

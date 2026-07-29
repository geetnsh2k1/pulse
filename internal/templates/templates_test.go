package templates

import (
	"path/filepath"
	"testing"

	"pulse/internal/config"
)

// Every starter template must render into a project that passes strict
// config validation. A broken starter is broken for every new user, so CI
// enforces this invariant forever.
func TestAllTemplatesRenderValidProjects(t *testing.T) {
	all := List()
	if len(all) < 3 {
		t.Fatalf("expected at least 3 templates, got %d", len(all))
	}
	for _, info := range all {
		t.Run(info.Name, func(t *testing.T) {
			if info.Description == "" {
				t.Errorf("template %s has no description", info.Name)
			}
			dst := t.TempDir()
			written, err := Render(info.Name, dst, Data{Project: "sample-" + info.Name})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if len(written) == 0 {
				t.Fatal("Render wrote no files")
			}

			cfg, err := config.Load(filepath.Join(dst, config.FileName))
			if err != nil {
				t.Fatalf("rendered project does not validate:\n%v", err)
			}
			if cfg.Project != "sample-"+info.Name {
				t.Errorf("project name not substituted: %q", cfg.Project)
			}
			if len(cfg.Functions) == 0 {
				t.Error("template declares no functions")
			}
		})
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	if _, err := Render("nope", t.TempDir(), Data{Project: "x"}); err == nil {
		t.Fatal("want error for unknown template")
	}
}

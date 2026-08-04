package templates

import (
	"os"
	"path/filepath"
	"testing"

	"pulse/internal/config"
)

// Every starter template must render into a project that passes strict
// config validation — in every language variant it offers. A broken starter
// is broken for every new user, so CI enforces this invariant forever.
func TestAllTemplatesRenderValidProjects(t *testing.T) {
	all := List()
	if len(all) < 3 {
		t.Fatalf("expected at least 3 templates, got %d", len(all))
	}
	for _, info := range all {
		variants := info.Variants
		if len(variants) == 0 {
			variants = []string{""} // single-flavor template
		}
		for _, lang := range variants {
			name := info.Name
			if lang != "" {
				name += "/" + lang
			}
			t.Run(name, func(t *testing.T) {
				if info.Description == "" {
					t.Errorf("template %s has no description", info.Name)
				}
				dst := t.TempDir()
				written, err := Render(info.Name, dst, Data{Project: "sample-app", Lang: lang})
				if err != nil {
					t.Fatalf("Render: %v", err)
				}
				if len(written) == 0 {
					t.Fatal("Render wrote no files")
				}
				for _, rel := range written {
					if rel[0] == '_' {
						t.Errorf("variant prefix leaked into output: %s", rel)
					}
				}

				cfg, err := config.Load(filepath.Join(dst, config.FileName))
				if err != nil {
					t.Fatalf("rendered project does not validate:\n%v", err)
				}
				if cfg.Project != "sample-app" {
					t.Errorf("project name not substituted: %q", cfg.Project)
				}
				if len(cfg.Functions) == 0 {
					t.Error("template declares no functions")
				}
			})
		}
	}
}

func TestVariantFilesMatchLanguage(t *testing.T) {
	dst := t.TempDir()
	if _, err := Render("order-pipeline", dst, Data{Project: "py-app", Lang: "python"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "services", "api", "src", "api.py")); err != nil {
		t.Error("python variant missing api.py")
	}
	if _, err := os.Stat(filepath.Join(dst, "services", "api", "src", "api.mjs")); !os.IsNotExist(err) {
		t.Error("python variant leaked node files")
	}

	dst = t.TempDir()
	if _, err := Render("order-pipeline", dst, Data{Project: "node-app", Lang: "node"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "services", "worker", "handler.mjs")); err != nil {
		t.Error("node variant missing worker handler.mjs")
	}
}

func TestRenderErrors(t *testing.T) {
	if _, err := Render("nope", t.TempDir(), Data{Project: "x"}); err == nil {
		t.Fatal("want error for unknown template")
	}
	if _, err := Render("order-pipeline", t.TempDir(), Data{Project: "x", Lang: "rust"}); err == nil {
		t.Fatal("want error for unknown variant")
	}
	if _, err := Render("order-pipeline", t.TempDir(), Data{Project: "x"}); err == nil {
		t.Fatal("want error when a variant template gets no lang")
	}
}

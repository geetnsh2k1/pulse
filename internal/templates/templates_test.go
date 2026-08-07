package templates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// Every starter template must render into a project that passes strict
// config validation — in every language variant it offers. A broken starter
// is broken for every new user, so CI enforces this invariant forever.
func TestAllTemplatesRenderValidProjects(t *testing.T) {
	all := List()
	if len(all) < 2 {
		t.Fatalf("expected at least 2 templates, got %d", len(all))
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
	if _, err := Render("api-and-worker", dst, Data{Project: "py-app", Lang: "python"}); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"create-order", "get-order", "worker"} {
		if _, err := os.Stat(filepath.Join(dst, "services", fn, "handler.py")); err != nil {
			t.Errorf("python variant missing services/%s/handler.py", fn)
		}
		if _, err := os.Stat(filepath.Join(dst, "services", fn, "handler.mjs")); !os.IsNotExist(err) {
			t.Errorf("python variant leaked node files into services/%s", fn)
		}
	}

	dst = t.TempDir()
	if _, err := Render("api-and-worker", dst, Data{Project: "node-app", Lang: "node"}); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"create-order", "get-order", "worker"} {
		if _, err := os.Stat(filepath.Join(dst, "services", fn, "handler.mjs")); err != nil {
			t.Errorf("node variant missing services/%s/handler.mjs", fn)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "package.json")); err != nil {
		t.Error("node variant missing root package.json")
	}
}

func TestRenderErrors(t *testing.T) {
	if _, err := Render("nope", t.TempDir(), Data{Project: "x"}); err == nil {
		t.Fatal("want error for unknown template")
	}
	if _, err := Render("api-and-worker", t.TempDir(), Data{Project: "x", Lang: "rust"}); err == nil {
		t.Fatal("want error for unknown variant")
	}
	if _, err := Render("api-and-worker", t.TempDir(), Data{Project: "x"}); err == nil {
		t.Fatal("want error when a variant template gets no lang")
	}
}

// Package templates embeds the starter projects served by `pulse init`.
//
// A template may offer language variants: top-level directories named
// _node/, _python/, … hold per-language files; everything else is shared.
// Render includes exactly one variant (stripping the prefix), chosen by
// Data.Lang.
package templates

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"embed"
)

//go:embed all:templates
var templatesFS embed.FS

// Info describes one starter template.
type Info struct {
	Name        string
	Description string
	Variants    []string // language variants, empty when single-flavor
}

var descriptions = map[string]string{
	"node-api":       "Minimal Node.js function behind GET /hello (default)",
	"python-api":     "Minimal Python function behind GET /hello",
	"order-pipeline": "Full MVP demo: API → SQS worker (DynamoDB at phase 4)",
}

// List returns the available templates, sorted by name.
func List() []Info {
	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		return nil
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, Info{
			Name:        e.Name(),
			Description: descriptions[e.Name()],
			Variants:    Variants(e.Name()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Variants lists a template's language variants (empty = no variants).
func Variants(name string) []string {
	entries, err := fs.ReadDir(templatesFS, "templates/"+name)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "_") {
			out = append(out, strings.TrimPrefix(e.Name(), "_"))
		}
	}
	sort.Strings(out)
	return out
}

// Data is what template files may substitute.
type Data struct {
	Project string
	Lang    string // for templates with variants: "node", "python", …
}

// Render copies template name into dst. Files ending in .tmpl go through
// text/template with Data (suffix stripped); everything else is copied
// verbatim. Variant directories (_node/, _python/) are filtered by
// Data.Lang and their prefix stripped. Returns the relative paths written.
func Render(name, dst string, data Data) ([]string, error) {
	rootDir := "templates/" + name
	if _, err := fs.Stat(templatesFS, rootDir); err != nil {
		return nil, fmt.Errorf("unknown template %q — run `pulse init --list` to see what's available", name)
	}
	variants := Variants(name)
	if len(variants) > 0 && !contains(variants, data.Lang) {
		return nil, fmt.Errorf("template %q needs a language — pass --lang %s", name, strings.Join(variants, "|"))
	}

	var written []string
	err := fs.WalkDir(templatesFS, rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, rootDir+"/")

		// Variant filtering: _<lang>/... is included only for the chosen
		// language, with the prefix stripped.
		if first, rest, ok := strings.Cut(rel, "/"); ok && strings.HasPrefix(first, "_") {
			if first != "_"+data.Lang {
				return nil
			}
			rel = rest
		} else if strings.HasPrefix(first, "_") {
			return nil // stray top-level _file; never emitted
		}

		src, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return err
		}

		out := src
		if strings.HasSuffix(rel, ".tmpl") {
			tpl, err := template.New(rel).Parse(string(src))
			if err != nil {
				return fmt.Errorf("template %s is broken: %w", rel, err)
			}
			var buf bytes.Buffer
			if err := tpl.Execute(&buf, data); err != nil {
				return fmt.Errorf("rendering %s: %w", rel, err)
			}
			out = buf.Bytes()
			rel = strings.TrimSuffix(rel, ".tmpl")
		}

		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, out, 0o644); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(written)
	return written, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

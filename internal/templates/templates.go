// Package templates embeds the starter projects served by `pulse init`.
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
}

var descriptions = map[string]string{
	"node-api":       "Minimal Node.js function behind GET /hello (default)",
	"python-api":     "Minimal Python function behind GET /hello",
	"order-pipeline": "Full MVP demo: API → DynamoDB/S3/SQS → Python worker (lights up across M2–M4)",
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
		out = append(out, Info{Name: e.Name(), Description: descriptions[e.Name()]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Data is what template files may substitute.
type Data struct {
	Project string
}

// Render copies template name into dst. Files ending in .tmpl go through
// text/template with Data (suffix stripped); everything else is copied
// verbatim. Returns the relative paths written.
func Render(name, dst string, data Data) ([]string, error) {
	rootDir := "templates/" + name
	if _, err := fs.Stat(templatesFS, rootDir); err != nil {
		return nil, fmt.Errorf("unknown template %q — run `pulse init --list` to see what's available", name)
	}

	var written []string
	err := fs.WalkDir(templatesFS, rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, rootDir+"/")
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

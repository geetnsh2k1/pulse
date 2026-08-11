package importer

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// Written is what an import produced, for the console summary.
type Written struct {
	Root  string
	Files []string
}

// WriteOptions controls the one destructive-ish step in the importer:
// putting files on the caller's own disk. Nothing in AWS is touched.
type WriteOptions struct {
	// Dest is the project directory to create. It must not exist, or must be
	// empty — an import never writes over someone's work.
	Dest string
	// WithValues carries real environment values into .env instead of
	// placeholders. Explicit by design (PLAN §12.3 rule 4).
	WithValues bool
	// Profile and Account are recorded in IMPORT-NOTES.md so the project
	// remembers where it came from.
	Profile string
	Account string
	// Code holds packages already fetched with FetchCode, matched to
	// functions by name. Fetching before the user is asked anything means a
	// presigned URL can't expire mid-conversation, and the same bytes feed
	// the code scan that sharpens resource guesses.
	Code []*CodePackage
	// HTTPClient is swappable so tests never reach the network. Only used for
	// functions with no pre-fetched package.
	HTTPClient *http.Client
}

// maxUnzipped is a second line of defence behind the CodeSize refusal: a
// zip can lie about its contents, so extraction is capped too.
const maxUnzipped = maxCodeSize

// Write materializes the plan. Everything is assembled inside a temporary
// directory and moved into place only once pulse.yaml has been re-parsed and
// validated — a failed or interrupted import leaves no half-written project
// behind (PLAN §12.3 rule 3).
func (p *Plan) Write(ctx context.Context, o WriteOptions) (*Written, error) {
	existedEmpty, err := destUsable(o.Dest)
	if err != nil {
		return nil, err
	}

	// Stage as a sibling of the destination so the final move is a rename on
	// the same filesystem, which is atomic. /tmp may well be another device.
	parent := filepath.Dir(o.Dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", parent, err)
	}
	stage, err := os.MkdirTemp(parent, ".pulse-import-*")
	if err != nil {
		return nil, fmt.Errorf("creating a staging directory: %w", err)
	}
	// Any early return removes the staging directory; success renames it away
	// first, so this becomes a no-op.
	defer os.RemoveAll(stage)
	files, err := p.writeInto(ctx, stage, o)
	if err != nil {
		return nil, err
	}

	// The strongest check available: load what we just wrote exactly as
	// `pulse start` would. An import that wouldn't run is caught here.
	if _, err := config.Load(filepath.Join(stage, config.FileName)); err != nil {
		return nil, fmt.Errorf("the imported project didn't validate (this is a pulse bug, please report it): %w", err)
	}

	// Renaming onto an existing empty directory succeeds on Linux and fails on
	// macOS, so clear it first. os.Remove refuses a directory that isn't empty,
	// which makes this the safe order: if something appeared in there while we
	// were downloading, the import stops instead of eating it.
	if existedEmpty {
		if err := os.Remove(o.Dest); err != nil {
			return nil, fmt.Errorf("%s is no longer empty — nothing was written: %w", o.Dest, err)
		}
	}
	if err := os.Rename(stage, o.Dest); err != nil {
		return nil, fmt.Errorf("moving the project into %s: %w", o.Dest, err)
	}
	return &Written{Root: o.Dest, Files: files}, nil
}

// destUsable refuses anything that would destroy existing work. Default is a
// new project directory (PLAN §12.3 rule 2); the only concession is an empty
// directory, since `mkdir shop && cd shop` is how half of us start. The bool
// reports that concession, which the caller has to handle at rename time.
func destUsable(dest string) (existedEmpty bool, err error) {
	if dest == "" {
		return false, fmt.Errorf("no destination directory given")
	}
	st, err := os.Stat(dest)
	switch {
	case os.IsNotExist(err):
		return false, nil // the normal case: we create it
	case err != nil:
		return false, fmt.Errorf("checking %s: %w", dest, err)
	case !st.IsDir():
		return false, fmt.Errorf("%s exists and is a file, not a directory", dest)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", dest, err)
	}
	if len(entries) == 0 {
		return true, nil
	}
	return false, fmt.Errorf("%s already exists and isn't empty — pulse won't write over it\n"+
		"    fix: pick another directory with --name, or import into an empty one", dest)
}

func (p *Plan) writeInto(ctx context.Context, root string, o WriteOptions) ([]string, error) {
	var files []string
	write := func(name, body string, mode os.FileMode) error {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
		files = append(files, name)
		return nil
	}

	// pulse.yaml, rendered from the same struct the loader parses.
	if err := write(config.FileName, p.ConfigYAML(), 0o644); err != nil {
		return nil, err
	}

	// .env holds values (or placeholders) and must never be committed;
	// .env.example documents the names and is safe to commit.
	if err := write(config.DotEnvFile, p.DotEnvLines(o.WithValues), 0o600); err != nil {
		return nil, err
	}
	if err := write(".env.example", p.DotEnvExampleLines(), 0o644); err != nil {
		return nil, err
	}
	if err := write(".gitignore", importGitignore, 0o644); err != nil {
		return nil, err
	}
	if err := write("IMPORT-NOTES.md", p.Notes(o.Profile, o.Account), 0o644); err != nil {
		return nil, err
	}

	// The handler code itself.
	for _, f := range p.Functions {
		dir := filepath.Join(root, f.CodeDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}

		pkg := o.packageFor(f.Name)
		if pkg == nil && f.CodeURL != "" {
			// Nothing pre-fetched (a library caller, or a plan built without a
			// fetch step): download now.
			var err error
			pkg, err = FetchCode(ctx, o.client(), f.Name, f.CodeURL)
			if err != nil {
				return nil, err
			}
			defer pkg.Close()
		}
		if pkg == nil {
			// No package at all — AWS withheld the location, or the caller
			// planned without code. Leave a marker rather than an empty
			// directory that fails mysteriously at `pulse start`.
			if err := write(filepath.Join(f.CodeDir, "MISSING-CODE.md"), missingCode(f.Name), 0o644); err != nil {
				return nil, err
			}
			continue
		}
		names, err := pkg.extractTo(dir)
		if err != nil {
			return nil, fmt.Errorf("unpacking code for %s: %w", f.Name, err)
		}
		for _, n := range names {
			files = append(files, filepath.Join(f.CodeDir, n))
		}
	}
	return files, nil
}

func (o WriteOptions) packageFor(fn string) *CodePackage {
	for _, c := range o.Code {
		if c != nil && c.Function == fn {
			return c
		}
	}
	return nil
}

func (o WriteOptions) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Minute} // bundles can be large
}

// ConfigYAML renders pulse.yaml by hand, in the same shape and order as the
// files `pulse init` scaffolds — an imported project should be
// indistinguishable from one a person wrote, comments and all. yaml.Marshal
// would emit `env: {}`, `buckets: []` and `api: {port: 0}`, which is noise a
// reader has to ignore.
//
// Drift between this renderer and the validator is caught by loading the
// result back through config.Load before anything moves into place.
func (p *Plan) ConfigYAML() string {
	cfg := p.ToConfig()
	var b strings.Builder

	b.WriteString("# Imported from AWS by `pulse import aws`.\n")
	b.WriteString("# Environment values live in .env — this file is safe to commit.\n")
	b.WriteString("# Anything that couldn't come across is listed in IMPORT-NOTES.md.\n\n")
	fmt.Fprintf(&b, "project: %s\n", yamlScalar(cfg.Project))
	fmt.Fprintf(&b, "region: %s\n", yamlScalar(cfg.Region))

	b.WriteString("\nfunctions:\n")
	for _, f := range p.Functions { // plan order, not map order: stable output
		fn := cfg.Functions[f.Name]
		fmt.Fprintf(&b, "  %s:\n", yamlKey(f.Name))
		fmt.Fprintf(&b, "    runtime: %s\n", fn.Runtime)
		fmt.Fprintf(&b, "    handler: %s\n", yamlScalar(fn.Handler))
		fmt.Fprintf(&b, "    codeDir: %s\n", yamlScalar(fn.CodeDir))
		fmt.Fprintf(&b, "    timeout: %d\n", fn.Timeout)
		fmt.Fprintf(&b, "    memory: %d\n", fn.Memory)
		if n := envCount(f); n > 0 {
			fmt.Fprintf(&b, "    # %s in .env\n", plural(n, "variable", "variables"))
		}
	}

	if len(cfg.Triggers) > 0 {
		b.WriteString("\ntriggers:\n")
		for _, t := range cfg.Triggers {
			switch t.Type {
			case "http":
				fmt.Fprintf(&b, "  - type: http\n    method: %s\n    path: %s\n    function: %s\n",
					t.Method, yamlScalar(t.Path), yamlScalar(t.Function))
				if t.PayloadFormat != "" {
					fmt.Fprintf(&b, "    payloadFormat: %q   # REST API event shape\n", t.PayloadFormat)
				}
			case "sqs":
				fmt.Fprintf(&b, "  - type: sqs\n    queue: %s\n    function: %s\n",
					yamlScalar(t.Queue), yamlScalar(t.Function))
				if t.BatchSize > 0 {
					fmt.Fprintf(&b, "    batchSize: %d\n", t.BatchSize)
				}
			}
		}
	}

	if len(cfg.Resources.Tables) == 0 && len(cfg.Resources.Queues) == 0 {
		return b.String()
	}
	b.WriteString("\nresources:\n")
	if len(cfg.Resources.Tables) > 0 {
		b.WriteString("  tables:\n")
		for _, t := range p.Tables {
			table := cfg.Resources.Tables[t.Name]
			fmt.Fprintf(&b, "    %s:\n", yamlKey(t.Name))
			fmt.Fprintf(&b, "      pk: %s\n", keyLine(table.PK))
			if table.SK != nil {
				fmt.Fprintf(&b, "      sk: %s\n", keyLine(*table.SK))
			}
		}
	}
	if len(cfg.Resources.Queues) > 0 {
		b.WriteString("  queues:\n")
		for _, q := range p.Queues {
			queue := cfg.Resources.Queues[q.Name]
			// A queue with no settings of its own still has to exist as a key.
			if queue.DLQ == "" && queue.MaxReceiveCount == 0 && queue.VisibilityTimeout == 0 {
				fmt.Fprintf(&b, "    %s: {}\n", yamlKey(q.Name))
				continue
			}
			fmt.Fprintf(&b, "    %s:\n", yamlKey(q.Name))
			if queue.DLQ != "" {
				fmt.Fprintf(&b, "      dlq: %s\n", yamlScalar(queue.DLQ))
			}
			if queue.MaxReceiveCount > 0 {
				fmt.Fprintf(&b, "      maxReceiveCount: %d\n", queue.MaxReceiveCount)
			}
			if queue.VisibilityTimeout > 0 {
				fmt.Fprintf(&b, "      visibilityTimeout: %d\n", queue.VisibilityTimeout)
			}
		}
	}
	return b.String()
}

// keyLine prefers the shorthand `pk: id` the templates use, and falls back to
// the explicit form when the key isn't a string.
func keyLine(k config.KeyDef) string {
	if k.Type == "" || k.Type == "S" {
		return yamlScalar(k.Name)
	}
	return fmt.Sprintf("{ name: %s, type: %s }", yamlScalar(k.Name), k.Type)
}

func envCount(f PlannedFunction) int {
	n := 0
	for _, k := range f.EnvNames {
		if !config.ReservedEnvKeys[k] {
			n++
		}
	}
	return n
}

// yamlScalar quotes anything a YAML parser would read as structure rather
// than text — `{id}` in a route path is the common one.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, `{}[]&*#?|<>=!%@,'":`+"\n\t") ||
		strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") ||
		strings.HasPrefix(s, "-") {
		return strconv.Quote(s)
	}
	return s
}

// yamlKey is the same rule for map keys. AWS names are conservative, but a
// function called "true" or "12" would otherwise decode as a bool or an int.
func yamlKey(s string) string {
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return strconv.Quote(s)
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.Quote(s)
	}
	return yamlScalar(s)
}

const importGitignore = `.pulse/
node_modules/
__pycache__/
.venv/

# local secrets and per-machine overrides — never commit
.env
`

func missingCode(fn string) string {
	return fmt.Sprintf(`# Code not downloaded

pulse couldn't fetch the deployment package for %q, so this directory is
empty. Copy your handler here (matching the handler path in pulse.yaml), or
re-run the import.
`, fn)
}

// CodePackage is a Lambda deployment package downloaded to local disk. It
// exists as a type because the same bytes serve two purposes at two moments:
// the code scan that sharpens resource guesses (before the user is asked
// anything) and the extraction into the new project (after they approve).
// Fetching once means the presigned URL cannot expire in between.
type CodePackage struct {
	Function string
	path     string
	size     int64
}

// FetchCode downloads a deployment package to a temp file. The caller owns
// it and must Close it.
func FetchCode(ctx context.Context, client *http.Client, fn, url string) (*CodePackage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading the code for %s: %w", fn, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading the code for %s: AWS returned %s\n"+
			"    fix: presigned links are short-lived — run the import again", fn, res.Status)
	}

	// zip.OpenReader needs random access, so the archive lands on disk rather
	// than in memory: a 200 MB bundle shouldn't cost 200 MB of RAM.
	tmp, err := os.CreateTemp("", "pulse-code-*.zip")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(tmp, io.LimitReader(res.Body, maxUnzipped+1))
	closeErr := tmp.Close()
	switch {
	case err != nil:
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("downloading the code for %s: %w", fn, err)
	case closeErr != nil:
		os.Remove(tmp.Name())
		return nil, closeErr
	case n > maxUnzipped:
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("the package for %s exceeds %d MB", fn, maxUnzipped>>20)
	}
	return &CodePackage{Function: fn, path: tmp.Name(), size: n}, nil
}

// Close removes the downloaded archive.
func (c *CodePackage) Close() error {
	if c == nil || c.path == "" {
		return nil
	}
	return os.Remove(c.path)
}

// sourceLimits keep the code scan bounded: a bundled handler is a few
// hundred KB, and reading a 50 MB minified blob to look for table names
// buys nothing.
const (
	maxScanFile  = 512 << 10
	maxScanTotal = 4 << 20
)

var scanExts = map[string]bool{
	".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".jsx": true, ".tsx": true,
	".py": true, ".json": true, ".yaml": true, ".yml": true,
}

// SourceText concatenates the handler's own source for the last-resort code
// scan in InferResources. Vendored dependencies are skipped: a resource name
// appearing inside node_modules is noise, and matching it would propose
// tables the application never touches.
func (c *CodePackage) SourceText() string {
	if c == nil {
		return ""
	}
	zr, err := zip.OpenReader(c.path)
	if err != nil {
		return "" // the scan is optional; extraction will report the real error
	}
	defer zr.Close()

	var b strings.Builder
	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || isVendored(entry.Name) ||
			!scanExts[strings.ToLower(filepath.Ext(entry.Name))] ||
			entry.UncompressedSize64 > maxScanFile {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			continue
		}
		n, _ := io.Copy(&limitWriter{w: &b, left: maxScanTotal - b.Len()}, rc)
		rc.Close()
		if n > 0 {
			b.WriteString("\n")
		}
		if b.Len() >= maxScanTotal {
			break
		}
	}
	return b.String()
}

func isVendored(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "node_modules", "site-packages", "dist-info", ".venv", "venv":
			return true
		}
	}
	return false
}

// limitWriter caps how much of a file reaches the scan buffer.
type limitWriter struct {
	w    *strings.Builder
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.left <= 0 {
		return len(p), nil // swallow the rest; the cap is intentional
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	n, err := l.w.Write(p)
	l.left -= n
	return len(p), err
}

// extractTo unpacks the package into dir. It is strict about the two things
// that make unzipping dangerous: entries that escape the destination
// (zip-slip) and a total expanded size far past what AWS reported.
func (c *CodePackage) extractTo(dir string) ([]string, error) {
	zr, err := zip.OpenReader(c.path)
	if err != nil {
		return nil, fmt.Errorf("the package isn't a readable zip: %w", err)
	}
	defer zr.Close()

	var written []string
	var total int64
	for _, entry := range zr.File {
		name, err := safeJoin(dir, entry.Name)
		if err != nil {
			return nil, err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(name, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return nil, err
		}
		total += int64(entry.UncompressedSize64)
		if total > maxUnzipped {
			return nil, fmt.Errorf("the package expands past %d MB", maxUnzipped>>20)
		}
		if err := extractOne(entry, name); err != nil {
			return nil, err
		}
		written = append(written, entry.Name)
	}
	return written, nil
}

func extractOne(entry *zip.File, dest string) error {
	rc, err := entry.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// Keep the executable bit (bootstrap scripts rely on it) but never more
	// than the owner needs.
	mode := entry.Mode().Perm() & 0o755
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(rc, maxUnzipped))
	return err
}

// safeJoin blocks the zip-slip class of bug: an archive entry named
// "../../.ssh/authorized_keys" must never resolve outside the destination.
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("the package contains an unsafe path %q — refusing to extract it", name)
	}
	full := filepath.Join(dir, clean)
	// Belt and braces: compare the resolved prefix too, in case Clean was
	// defeated by an unusual separator.
	if rel, err := filepath.Rel(dir, full); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("the package contains an unsafe path %q — refusing to extract it", name)
	}
	return full, nil
}

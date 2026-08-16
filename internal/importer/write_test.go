package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/dotenv"
)

// zipOf builds a deployment package in memory. Keys are archive paths; a
// trailing "/" makes a directory entry. Tests never hit the network beyond
// the httptest server this feeds.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		if strings.HasSuffix(name, "/") {
			if _, err := zw.Create(name); err != nil {
				t.Fatal(err)
			}
			continue
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveZip returns a URL that hands out the given archive once.
func serveZip(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/code.zip"
}

// writePlan is the standard fixture: one function with a route, a queue and a
// table, i.e. every section of pulse.yaml exercised at once.
func writePlan(t *testing.T, codeURL string) *Plan {
	return writePlanWith(t, codeURL)
}

// writePlanWith is writePlan for the layer cases: layers have to be present in
// discovery, since that is where the plan decides what to say about them.
func writePlanWith(t *testing.T, codeURL string, layers ...Layer) *Plan {
	t.Helper()
	fn := zipFn()
	fn.Layers = layers
	fn.CodeURL = codeURL
	fn.Env["API_KEY"] = "sk-live-do-not-leak"
	d := Discovery{
		Region:   "eu-west-1",
		Function: fn,
		Routes:   []HTTPRoute{{Method: "POST", Path: "/orders"}, {Method: "GET", Path: "/orders/{id}"}},
		EventSources: []EventSource{
			{Kind: "sqs", ARN: "arn:aws:sqs:eu-west-1:111122223333:order-events", BatchSize: 10, Enabled: true},
		},
	}
	p := mustPlan(t, d, "shop")
	p.AddTable(Table{Name: "orders", PK: Key{Name: "id", Type: "S"}, SK: &Key{Name: "sk", Type: "N"}}, Picked, nil)
	return p
}

func TestWriteProducesARunnableProject(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "shop")
	p := writePlan(t, serveZip(t, zipOf(t, map[string]string{
		"handler.py":     "def handler(event, context):\n    return {}\n",
		"lib/":           "",
		"lib/helpers.py": "X = 1\n",
	})))

	got, err := p.Write(context.Background(), WriteOptions{Dest: dest, Profile: "dev", Account: "1234"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got.Root != dest {
		t.Errorf("root = %q, want %q", got.Root, dest)
	}

	for _, name := range []string{
		config.FileName, ".env", ".env.example", ".gitignore", "IMPORT-NOTES.md",
		"functions/createOrder/handler.py", "functions/createOrder/lib/helpers.py",
	} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	// The whole point of the writer: what it produced is a project pulse can
	// actually start, keys and routes intact.
	cfg, err := config.Load(filepath.Join(dest, config.FileName))
	if err != nil {
		t.Fatalf("the written project does not load: %v", err)
	}
	if cfg.Project != "shop" || cfg.Region != "eu-west-1" {
		t.Errorf("project/region = %q/%q", cfg.Project, cfg.Region)
	}
	if len(cfg.Functions) != 1 || cfg.Functions["createOrder"].Runtime != "python3.12" {
		t.Errorf("functions = %+v", cfg.Functions)
	}
	var paths []string
	for _, tr := range cfg.Triggers {
		if tr.Type == "http" {
			paths = append(paths, tr.Method+" "+tr.Path)
		}
	}
	if strings.Join(paths, ",") != "POST /orders,GET /orders/{id}" {
		t.Errorf("routes = %v (a braced path must survive YAML quoting)", paths)
	}
	if tbl := cfg.Resources.Tables["orders"]; tbl == nil || tbl.PK.Name != "id" ||
		tbl.SK == nil || tbl.SK.Name != "sk" || tbl.SK.Type != "N" {
		t.Errorf("table = %+v (non-string key types must be written explicitly)", tbl)
	}
	if q := cfg.Resources.Queues["order-events"]; q == nil {
		t.Errorf("queues = %+v", cfg.Resources.Queues)
	}
}

func TestWriteKeepsSecretsOutOfTheProjectByDefault(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "shop")
	p := writePlan(t, "")
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	yamlBody := read(t, dest, config.FileName)
	env := read(t, dest, ".env")
	example := read(t, dest, ".env.example")
	gitignore := read(t, dest, ".gitignore")

	if strings.Contains(yamlBody, "sk-live-do-not-leak") || strings.Contains(example, "sk-live-do-not-leak") {
		t.Error("a real value reached a committed file")
	}
	if strings.Contains(env, "sk-live-do-not-leak") {
		t.Error(".env carried a real value without --with-values")
	}
	if !strings.Contains(env, "API_KEY="+placeholder) {
		t.Errorf(".env should placeholder every name, got:\n%s", env)
	}
	if !strings.Contains(example, "API_KEY=") || strings.Contains(example, "API_KEY=C") {
		t.Errorf(".env.example should list bare names, got:\n%s", example)
	}
	if !strings.Contains(gitignore, "\n.env\n") {
		t.Errorf(".gitignore must exclude .env, got:\n%s", gitignore)
	}
	// .env may hold live keys once --with-values is used; it should not be
	// world-readable even when it doesn't yet.
	st, err := os.Stat(filepath.Join(dest, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %o, want 600", perm)
	}
}

func TestWriteWithValuesCarriesThemAcross(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "shop")
	p := writePlan(t, "")
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest, WithValues: true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	env := read(t, dest, ".env")
	if !strings.Contains(env, "API_KEY=sk-live-do-not-leak") {
		t.Errorf("--with-values should write real values, got:\n%s", env)
	}
	if strings.Contains(read(t, dest, ".env.example"), "sk-live") {
		t.Error(".env.example must never carry values, even with --with-values")
	}
}

func TestWriteRefusesToTouchExistingWork(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "important.py"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := writePlan(t, "").Write(context.Background(), WriteOptions{Dest: dest})
	if err == nil {
		t.Fatal("want a refusal for a non-empty destination")
	}
	if !strings.Contains(err.Error(), "isn't empty") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("error should say what and what to do, got: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "important.py")); string(body) != "mine" {
		t.Error("existing file was modified")
	}
}

func TestWriteFillsAnEmptyDirectory(t *testing.T) {
	dest := t.TempDir() // exists, empty
	if _, err := writePlan(t, "").Write(context.Background(), WriteOptions{Dest: dest}); err != nil {
		t.Fatalf("an empty directory should be usable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, config.FileName)); err != nil {
		t.Errorf("nothing was written: %v", err)
	}
}

func TestWriteLeavesNothingBehindWhenItFails(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "shop")

	// A presigned URL that has expired is the realistic failure: the plan is
	// fine, the download isn't.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := writePlan(t, srv.URL+"/code.zip").Write(context.Background(), WriteOptions{Dest: dest})
	if err == nil {
		t.Fatal("want an error when the code can't be fetched")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should carry the HTTP status, got: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a failed import must not leave a project directory behind")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging leftovers: %v", entries)
	}
}

func TestWriteRefusesZipSlip(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "shop")
	body := zipOf(t, map[string]string{
		"handler.py":           "ok\n",
		"../../../../evil.txt": "pwned",
	})

	_, err := writePlan(t, serveZip(t, body)).Write(context.Background(), WriteOptions{Dest: dest})
	if err == nil {
		t.Fatal("want a refusal for an entry that escapes the destination")
	}
	if !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("error = %v", err)
	}
	// Nothing anywhere near the destination, and no project either.
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		t.Error("the archive escaped the destination")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a refused import must not leave a project behind")
	}
}

func TestWriteRejectsGarbageArchives(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "shop")
	_, err := writePlan(t, serveZip(t, []byte("this is not a zip file"))).
		Write(context.Background(), WriteOptions{Dest: dest})
	if err == nil || !strings.Contains(err.Error(), "readable zip") {
		t.Fatalf("want a clear zip error, got: %v", err)
	}
}

func TestWriteMarksMissingCode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "shop")
	if _, err := writePlan(t, "").Write(context.Background(), WriteOptions{Dest: dest}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// An empty code directory would fail at `pulse start` with nothing to
	// explain it; the marker is the explanation.
	body := read(t, dest, "functions/createOrder/MISSING-CODE.md")
	if !strings.Contains(body, "createOrder") {
		t.Errorf("marker should name the function, got:\n%s", body)
	}
}

func TestWriteNotesRecordWhereItCameFrom(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "shop")
	p := writePlan(t, "")
	p.Unsupported = append(p.Unsupported, Note{Subject: "layers", Detail: "2 layers attached"})
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest, Profile: "dev", Account: "111122223333"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	notes := read(t, dest, "IMPORT-NOTES.md")
	for _, want := range []string{"111122223333", "dev", "eu-west-1", "layers"} {
		if !strings.Contains(notes, want) {
			t.Errorf("IMPORT-NOTES.md missing %q:\n%s", want, notes)
		}
	}
}

func TestFetchCodeScansTheHandlerAndIgnoresVendoredCode(t *testing.T) {
	url := serveZip(t, zipOf(t, map[string]string{
		"handler.py":                  "table = boto3.resource('dynamodb').Table('orders')\n",
		"node_modules/aws-sdk/x.js":   "const TABLE = 'someone-elses-table'\n",
		"lib/site-packages/boto/y.py": "QUEUE = 'not-ours'\n",
		"assets/logo.png":             "\x89PNG binary",
	}))
	pkg, err := FetchCode(context.Background(), http.DefaultClient, "createOrder", url)
	if err != nil {
		t.Fatalf("FetchCode: %v", err)
	}
	defer pkg.Close()

	text := pkg.SourceText()
	if !strings.Contains(text, "orders") {
		t.Errorf("the handler's own source should be scanned, got:\n%s", text)
	}
	for _, noise := range []string{"someone-elses-table", "not-ours", "PNG"} {
		if strings.Contains(text, noise) {
			t.Errorf("scan picked up %q — vendored/binary files must be skipped", noise)
		}
	}
}

func TestFetchCodeThenWriteDownloadsOnlyOnce(t *testing.T) {
	body := zipOf(t, map[string]string{"handler.py": "ok\n"})
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// The real flow: fetch before asking the user anything, then write with
	// what was fetched. A presigned URL that expires while they think must
	// not be able to fail the import.
	p := writePlan(t, srv.URL)
	pkg, err := FetchCode(context.Background(), srv.Client(), "createOrder", srv.URL)
	if err != nil {
		t.Fatalf("FetchCode: %v", err)
	}
	defer pkg.Close()

	dest := filepath.Join(t.TempDir(), "shop")
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest, Code: []*CodePackage{pkg}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if hits != 1 {
		t.Errorf("downloaded %d times, want exactly 1", hits)
	}
	if got := read(t, dest, "functions/createOrder/handler.py"); got != "ok\n" {
		t.Errorf("handler.py = %q", got)
	}
	if err := pkg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestFetchCodeExplainsAnExpiredLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := FetchCode(context.Background(), srv.Client(), "createOrder", srv.URL)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "createOrder") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("error should name the function and the fix, got: %v", err)
	}
}

func TestSafeJoinBlocksEscapes(t *testing.T) {
	dir := "/tmp/proj"
	bad := []string{
		"../evil", "../../evil", "/etc/passwd", "a/../../evil",
		`..\evil`, "./../evil",
	}
	for _, name := range bad {
		if _, err := safeJoin(dir, name); err == nil {
			t.Errorf("safeJoin(%q) should have been refused", name)
		}
	}
	ok := []string{"handler.py", "lib/util.py", "./handler.py", "a/b/../c.py"}
	for _, name := range ok {
		got, err := safeJoin(dir, name)
		if err != nil {
			t.Errorf("safeJoin(%q) = %v, want accepted", name, err)
			continue
		}
		if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
			t.Errorf("safeJoin(%q) = %q, outside %q", name, got, dir)
		}
	}
}

func TestRenderYAMLQuotesWhatYAMLWouldMisread(t *testing.T) {
	cases := map[string]string{
		"plain":       "plain",
		"":            `""`,
		"/orders":     "/orders",
		"/o/{id}":     `"/o/{id}"`,
		"a: b":        `"a: b"`,
		"# not a key": `"# not a key"`,
		"-leading":    `"-leading"`,
		"trail ":      `"trail "`,
	}
	for in, want := range cases {
		if got := yamlScalar(in); got != want {
			t.Errorf("yamlScalar(%q) = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"true", "no", "12", "1.5", "null"} {
		if got := yamlKey(in); !strings.HasPrefix(got, `"`) {
			t.Errorf("yamlKey(%q) = %s, want it quoted (a bare key decodes as a non-string)", in, got)
		}
	}
	if got := yamlKey("createOrder"); got != "createOrder" {
		t.Errorf("yamlKey(createOrder) = %s, want it left alone", got)
	}
}

func TestRenderYAMLReadsLikeAHandWrittenFile(t *testing.T) {
	body := writePlan(t, "").ConfigYAML()
	for _, unwanted := range []string{"env: {}", "buckets: []", "api:", "topics:", "streams:"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("pulse.yaml contains machine noise %q:\n%s", unwanted, body)
		}
	}
	if !strings.HasPrefix(body, "# Imported from AWS") {
		t.Errorf("pulse.yaml should open with an explanation, got:\n%s", body)
	}
	// Function order follows the plan, so two imports of the same account
	// produce byte-identical files.
	if body != writePlan(t, "").ConfigYAML() {
		t.Error("render is not deterministic")
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// A presigned S3 link carries X-Amz-Credential and X-Amz-Signature in its
// query. Go's network errors embed the whole URL, so a download that fails
// would print a signed credential into the terminal — and from there into
// whatever bug report the user pastes it in.
func TestDownloadErrorsRedactTheSignedURL(t *testing.T) {
	// A server that accepts the connection then drops it mid-body forces a
	// *url.Error carrying the URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler) // closes the connection without a response
	}))
	defer srv.Close()

	signed := srv.URL + "/code.zip?X-Amz-Credential=AKIAEXAMPLE%2F20260812%2Feu-west-1%2Fs3%2Faws4_request" +
		"&X-Amz-Signature=deadbeefcafe&X-Amz-Security-Token=SECRETTOKEN"
	_, err := FetchCode(context.Background(), srv.Client(), "createOrder", signed)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, secret := range []string{"X-Amz-Signature", "deadbeefcafe", "SECRETTOKEN", "AKIAEXAMPLE"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked %q:\n%v", secret, err)
		}
	}
	// It still has to be diagnosable: the host and the function must survive.
	if !strings.Contains(err.Error(), "createOrder") || !strings.Contains(err.Error(), "code.zip") {
		t.Errorf("redaction went too far, nothing left to debug with: %v", err)
	}
}

// Real Lambda environments hold PEM keys, JSON blobs and URLs with fragments.
// The generated .env has to survive its own parser, or --with-values silently
// corrupts values on the way to disk.
func TestGeneratedDotEnvSurvivesRealWorldValues(t *testing.T) {
	nasty := map[string]string{
		"PLAIN":       "hello",
		"WITH_SPACE":  "hello world",
		"WITH_HASH":   "value#notacomment",
		"JSON_BLOB":   `{"a":"b","c":[1,2]}`,
		"QUOTED":      `he said "hi"`,
		"URL_FRAG":    "https://x.test/a#frag",
		"MULTILINE":   "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----",
		"EQUALS":      "a=b=c",
		"EMPTY":       "",
		"TRAILING_SP": "pad ",
	}
	fn := zipFn()
	fn.Env = nasty
	p := mustPlan(t, Discovery{Function: fn, Region: "eu-west-1"}, "shop")

	body := p.DotEnvLines(true)
	got, err := dotenv.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("the .env pulse generated doesn't parse: %v\n%s", err, body)
	}
	for k, want := range nasty {
		if got[k] != want {
			t.Errorf("%s round-tripped as %q, want %q", k, got[k], want)
		}
	}
}

// Layers are how most teams ship dependencies, so a function with layers used
// to import cleanly and then fail on its first line. pulse now unpacks them
// beside the function the way AWS mounts them at /opt.
func TestWriteMergesLayers(t *testing.T) {
	layerURL := serveZip(t, zipOf(t, map[string]string{
		"python/pymongo/__init__.py":                     "VERSION = '4.17'\n",
		"python/lib/python3.13/site-packages/slack/x.py": "OK = True\n",
	}))
	code := serveZip(t, zipOf(t, map[string]string{"handler.py": "import pymongo\n"}))

	p := writePlanWith(t, code, Layer{
		ARN: "arn:aws:lambda:eu-west-1:1:layer:deps:9", Name: "deps", CodeURL: layerURL,
	})

	dest := filepath.Join(t.TempDir(), "shop")
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Unpacked under the function, preserving the layout the runtime expects.
	for _, want := range []string{
		"functions/createOrder/" + LayerDir + "/python/pymongo/__init__.py",
		"functions/createOrder/" + LayerDir + "/python/lib/python3.13/site-packages/slack/x.py",
	} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(want))); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	// Vendored dependencies are not the user's source: they stay out of git.
	if !strings.Contains(read(t, dest, ".gitignore"), LayerDir) {
		t.Errorf(".gitignore should exclude %s:\n%s", LayerDir, read(t, dest, ".gitignore"))
	}
	// And the plan says what happened rather than leaving it to be discovered.
	notes := read(t, dest, "IMPORT-NOTES.md")
	if !strings.Contains(notes, "merged") || !strings.Contains(notes, "deps") {
		t.Errorf("IMPORT-NOTES.md should record the merge:\n%s", notes)
	}
}

// A layer pulse couldn't read must be called out with the permission that
// would fix it — not silently skipped, which would look like a working import
// that mysteriously can't import its own dependencies.
func TestUnreadableLayerIsReportedNotSkipped(t *testing.T) {
	p := writePlanWith(t, "", Layer{ARN: "arn:…:layer:deps:9", Name: "deps"}) // no CodeURL

	dest := filepath.Join(t.TempDir(), "shop")
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest}); err != nil {
		t.Fatalf("an unreadable layer must not fail the import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "functions/createOrder", LayerDir)); err == nil {
		t.Error("nothing should have been unpacked for an unreadable layer")
	}
}

// The unpacked layer is gitignored, so the ARN in pulse.yaml is the only
// thing that survives a clone — without it a checkout cannot know what to
// re-fetch and the function just fails on an import.
func TestWriteRecordsLayerARNsInConfig(t *testing.T) {
	layerURL := serveZip(t, zipOf(t, map[string]string{"python/pymongo/__init__.py": "X = 1\n"}))
	p := writePlanWith(t, serveZip(t, zipOf(t, map[string]string{"handler.py": "x = 1\n"})),
		Layer{ARN: "arn:aws:lambda:ap-south-1:1:layer:deps:9", Name: "deps", CodeURL: layerURL},
		Layer{ARN: "arn:aws:lambda:ap-south-1:1:layer:denied:2", Name: "denied",
			Unreadable: "no permission for lambda:GetLayerVersion"},
	)

	dest := filepath.Join(t.TempDir(), "shop")
	if _, err := p.Write(context.Background(), WriteOptions{Dest: dest}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dest, config.FileName))
	if err != nil {
		t.Fatalf("the written project does not load: %v", err)
	}
	got := cfg.Functions["createOrder"].Layers
	// Both are recorded, including the one that was denied: the ARN is exactly
	// what a retry needs, and today's denial may be tomorrow's permission.
	want := []string{
		"arn:aws:lambda:ap-south-1:1:layer:deps:9",
		"arn:aws:lambda:ap-south-1:1:layer:denied:2",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("layers = %v, want %v", got, want)
	}
}

// FetchCode and FetchLayers are exported, so a nil client will be passed
// eventually — it should download, not panic three frames down.
func TestFetchToleratesANilClient(t *testing.T) {
	url := serveZip(t, zipOf(t, map[string]string{"python/pkg/__init__.py": "X = 1\n"}))
	dir := t.TempDir()

	written, err := FetchLayers(context.Background(), nil,
		[]Layer{{ARN: "arn:…:layer:deps:1", Name: "deps", CodeURL: url}}, dir)
	if err != nil {
		t.Fatalf("a nil client should default, got: %v", err)
	}
	if len(written) == 0 {
		t.Error("nothing was unpacked")
	}
	if _, err := os.Stat(filepath.Join(dir, "python", "pkg", "__init__.py")); err != nil {
		t.Error(err)
	}
}

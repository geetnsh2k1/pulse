package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// End-to-end coverage for `pulse import aws` with no AWS account involved: a
// local server speaks just enough of STS, Lambda, SQS, DynamoDB and IAM for
// the real SDK clients to talk to it (AWS_ENDPOINT_URL points them here).
// This is what proves the wiring — the pieces are unit-tested elsewhere, but
// only a full run catches a mis-shaped request or a step in the wrong order.

// fakeAWS records what was asked of it, so a test can assert that import
// really is read-only.
type fakeAWS struct {
	t *testing.T
	// Discovery reads four APIs at once, so the recorder is touched from
	// several handler goroutines.
	mu    sync.Mutex
	calls []string
	// noIAM makes the role read fail, exercising graceful degradation.
	noIAM bool
	// denyGetFunction makes the one mandatory read fail with AccessDenied.
	denyGetFunction bool
	code            []byte
	host            string
}

func (f *fakeAWS) log(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
}

func (f *fakeAWS) called(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == op {
			return true
		}
	}
	return false
}

func (f *fakeAWS) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	// Any mutating verb here would be a bug in pulse, not in the fake.
	if r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch {
		f.t.Errorf("import made a %s request to %s — it must be read-only", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	switch {
	case r.URL.Path == "/code.zip":
		f.log("DownloadCode")
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(f.code)

	// ---- Lambda: REST-JSON ----
	case r.URL.Path == "/2015-03-31/functions" || r.URL.Path == "/2015-03-31/functions/":
		f.log("ListFunctions")
		f.json(w, map[string]any{"Functions": []any{
			lambdaConfig(), map[string]any{
				"FunctionName": "legacyReport", "Runtime": "java17",
				"Handler": "com.acme.Report::handle", "PackageType": "Zip", "CodeSize": 9 << 20,
			},
		}})
	case r.URL.Path == "/2015-03-31/functions/createOrder":
		f.log("GetFunction")
		if reg := regionOf(r); reg != "" && reg != homeRegion {
			// Right name, wrong region — the case the probe exists for.
			w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
			w.WriteHeader(http.StatusNotFound)
			f.json(w, map[string]any{"message": "Function not found in " + reg})
			return
		}
		if f.denyGetFunction {
			// How Lambda actually reports a denial: 403 plus the error type in
			// a header, which is what the SDK reads.
			w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			f.json(w, map[string]any{"message": "User is not authorized to perform: lambda:GetFunction"})
			return
		}
		f.json(w, map[string]any{
			"Configuration": lambdaConfig(),
			"Code":          map[string]any{"Location": f.codeURL(), "RepositoryType": "S3"},
		})
	case strings.HasPrefix(r.URL.Path, "/2015-03-31/event-source-mappings"):
		f.log("ListEventSourceMappings")
		f.json(w, map[string]any{"EventSourceMappings": []any{map[string]any{
			"UUID":           "1",
			"EventSourceArn": "arn:aws:sqs:eu-west-1:111122223333:order-events",
			"FunctionArn":    "arn:aws:lambda:eu-west-1:111122223333:function:createOrder",
			"BatchSize":      10, "State": "Enabled",
		}}})
	case r.URL.Path == "/2015-03-31/functions/createOrder/policy":
		f.log("GetPolicy")
		policy, _ := json.Marshal(map[string]any{
			"Version": "2012-10-17",
			"Statement": []any{map[string]any{
				"Effect": "Allow", "Principal": map[string]any{"Service": "apigateway.amazonaws.com"},
				"Action": "lambda:InvokeFunction",
				"Condition": map[string]any{"ArnLike": map[string]any{
					"AWS:SourceArn": "arn:aws:execute-api:eu-west-1:111122223333:abc123/*/POST/orders",
				}},
			}},
		})
		f.json(w, map[string]any{"Policy": string(policy)})

	// Any other function name: what a typo looks like coming back from AWS.
	case strings.HasPrefix(r.URL.Path, "/2015-03-31/functions/"):
		w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
		w.WriteHeader(http.StatusNotFound)
		f.json(w, map[string]any{"message": "Function not found: " + r.URL.Path})

	// ---- API Gateway: REST-JSON. Nothing to add; the policy was precise. ----
	case strings.HasPrefix(r.URL.Path, "/v2/apis"):
		f.log("GetApis")
		f.json(w, map[string]any{"items": []any{}})

	// ---- JSON-protocol services are told apart by X-Amz-Target ----
	case strings.HasPrefix(r.Header.Get("X-Amz-Target"), "AmazonSQS."):
		f.sqs(w, strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonSQS."), body)
	case strings.HasPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810."):
		f.dynamo(w, strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810."), body)

	// ---- query-protocol services (STS, IAM) name the action in the body ----
	default:
		form, _ := url.ParseQuery(string(body))
		f.query(w, form.Get("Action"), form)
	}
}

func (f *fakeAWS) codeURL() string { return "http://" + f.host + "/code.zip" }

// homeRegion is where the fake's function actually lives. Requests signed for
// any other region get a 404, which is what AWS does — and what makes the
// "it's in another region" probe testable.
const homeRegion = "eu-west-1"

// regionOf reads the region out of the SigV4 credential scope:
//
//	Authorization: AWS4-HMAC-SHA256 Credential=AKIA/20260812/eu-west-1/lambda/aws4_request, ...
func regionOf(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	i := strings.Index(auth, "Credential=")
	if i < 0 {
		return ""
	}
	parts := strings.Split(auth[i+len("Credential="):], "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func (f *fakeAWS) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// jsonCRC answers the way DynamoDB does: with an x-amz-crc32 of the body,
// which its client validates. Without the header the SDK logs a warning
// about the response body, which would be noise in every test run.
func (f *fakeAWS) jsonCRC(w http.ResponseWriter, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.Header().Set("x-amz-crc32", strconv.FormatUint(uint64(crc32.ChecksumIEEE(body)), 10))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func lambdaConfig() map[string]any {
	return map[string]any{
		"FunctionName": "createOrder",
		"Runtime":      "python3.12",
		"Handler":      "handler.handler",
		"Timeout":      15,
		"MemorySize":   512,
		"CodeSize":     2048,
		"PackageType":  "Zip",
		"Role":         "arn:aws:iam::111122223333:role/createOrder-role",
		"Environment": map[string]any{"Variables": map[string]string{
			"ORDERS_TABLE": "orders",
			"STRIPE_KEY":   "sk_live_51REAL",
		}},
		"Layers": []any{map[string]any{
			"Arn": "arn:aws:lambda:eu-west-1:111122223333:layer:deps:4", "CodeSize": 1 << 20,
		}},
	}
}

func (f *fakeAWS) sqs(w http.ResponseWriter, op string, _ []byte) {
	f.log("sqs:" + op)
	switch op {
	case "ListQueues":
		f.json(w, map[string]any{"QueueUrls": []string{
			"https://sqs.eu-west-1.amazonaws.com/111122223333/order-events",
			"https://sqs.eu-west-1.amazonaws.com/111122223333/order-events-dlq",
		}})
	case "GetQueueUrl":
		f.json(w, map[string]any{"QueueUrl": "https://sqs.eu-west-1.amazonaws.com/111122223333/order-events"})
	case "GetQueueAttributes":
		redrive, _ := json.Marshal(map[string]any{
			"deadLetterTargetArn": "arn:aws:sqs:eu-west-1:111122223333:order-events-dlq",
			"maxReceiveCount":     "5",
		})
		f.json(w, map[string]any{"Attributes": map[string]string{
			"QueueArn":          "arn:aws:sqs:eu-west-1:111122223333:order-events",
			"VisibilityTimeout": "45",
			"RedrivePolicy":     string(redrive),
		}})
	default:
		http.Error(w, "unexpected sqs op "+op, http.StatusBadRequest)
	}
}

func (f *fakeAWS) dynamo(w http.ResponseWriter, op string, body []byte) {
	f.log("dynamodb:" + op)
	switch op {
	case "ListTables":
		f.jsonCRC(w, map[string]any{"TableNames": []string{"orders", "audit-log"}})
	case "DescribeTable":
		var in struct{ TableName string }
		_ = json.Unmarshal(body, &in)
		f.jsonCRC(w, map[string]any{"Table": map[string]any{
			"TableName": in.TableName,
			"KeySchema": []any{
				map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
				map[string]any{"AttributeName": "createdAt", "KeyType": "RANGE"},
			},
			"AttributeDefinitions": []any{
				map[string]any{"AttributeName": "pk", "AttributeType": "S"},
				map[string]any{"AttributeName": "createdAt", "AttributeType": "N"},
			},
		}})
	default:
		http.Error(w, "unexpected dynamodb op "+op, http.StatusBadRequest)
	}
}

func (f *fakeAWS) query(w http.ResponseWriter, action string, form url.Values) {
	f.log(action)
	w.Header().Set("Content-Type", "text/xml")
	switch action {
	case "GetCallerIdentity":
		fmt.Fprint(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">`+
			`<GetCallerIdentityResult><Arn>arn:aws:iam::111122223333:user/dev</Arn>`+
			`<UserId>AIDAEXAMPLE</UserId><Account>111122223333</Account></GetCallerIdentityResult>`+
			`<ResponseMetadata><RequestId>r1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
	case "ListRolePolicies":
		if f.noIAM {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<ErrorResponse><Error><Code>AccessDenied</Code>`+
				`<Message>not authorized to perform iam:ListRolePolicies</Message></Error></ErrorResponse>`)
			return
		}
		fmt.Fprint(w, `<ListRolePoliciesResponse><ListRolePoliciesResult><PolicyNames>`+
			`<member>inline</member></PolicyNames><IsTruncated>false</IsTruncated>`+
			`</ListRolePoliciesResult></ListRolePoliciesResponse>`)
	case "GetRolePolicy":
		doc := url.QueryEscape(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["dynamodb:PutItem","dynamodb:GetItem"],` +
			`"Resource":"arn:aws:dynamodb:eu-west-1:111122223333:table/orders"}]}`)
		fmt.Fprintf(w, `<GetRolePolicyResponse><GetRolePolicyResult><RoleName>%s</RoleName>`+
			`<PolicyName>inline</PolicyName><PolicyDocument>%s</PolicyDocument>`+
			`</GetRolePolicyResult></GetRolePolicyResponse>`, form.Get("RoleName"), doc)
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<ErrorResponse><Error><Code>InvalidAction</Code><Message>%s</Message></Error></ErrorResponse>`, action)
	}
}

// startFakeAWS points the SDK at a local server and hands back the recorder.
func startFakeAWS(t *testing.T) *fakeAWS {
	t.Helper()
	f := &fakeAWS{t: t, code: handlerZip(t)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.host = strings.TrimPrefix(srv.URL, "http://")

	// Static throwaway credentials and an endpoint override: the real SDK
	// signs and sends, this server answers. Nothing can reach AWS.
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_PROFILE", "")
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return f
}

// handlerZip is the deployment package: a handler that names one table the
// IAM policy also grants, and one the policy says nothing about.
func handlerZip(t *testing.T) []byte {
	t.Helper()
	files := map[string]string{
		"handler.py": "import boto3, os\n" +
			"ddb = boto3.resource('dynamodb')\n" +
			"orders = ddb.Table(os.environ['ORDERS_TABLE'])\n" +
			"audit = ddb.Table('audit-log')\n" +
			"def handler(event, context):\n    return {'statusCode': 201}\n",
		"requirements.txt": "", // empty: pip has nothing to fetch, so the test stays offline
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
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

// runImport drives the command the way a shell would, in a scratch directory.
func runImport(t *testing.T, answers string, args ...string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()

	resetImportFlags(t)
	old := flagChdir
	flagChdir = dir
	t.Cleanup(func() { flagChdir = old })

	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(answers))
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	addImportFlags(cmd) // the same registration the real command uses
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	err := runImportAWS(cmd, cmd.Flags().Args())
	return out.String(), dir, err
}

// resetImportFlags keeps the package-level flag globals from leaking between
// tests (cobra binds them once, tests run in one process).
func resetImportFlags(t *testing.T) {
	t.Helper()
	awsProfile, awsRegion = "", ""
	flagImportFunction, flagImportName = "", ""
	flagImportDryRun, flagImportYes, flagImportValues, flagImportPolicy = false, false, false, false
}

func TestImportAWSGoldenPath(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	fake := startFakeAWS(t)

	// Answers: Enter accepts the pre-checked guesses, then y to write.
	screen, dir, err := runImport(t, "\ny\n", "--function", "createOrder")
	if err != nil {
		t.Fatalf("import failed: %v\n%s", err, screen)
	}

	// Read-only, proven by what the fake was asked for.
	for _, want := range []string{
		"GetCallerIdentity", "GetFunction", "ListEventSourceMappings", "GetPolicy",
		"sqs:ListQueues", "dynamodb:ListTables", "dynamodb:DescribeTable",
		"sqs:GetQueueAttributes", "ListRolePolicies", "GetRolePolicy", "DownloadCode",
	} {
		if !fake.called(want) {
			t.Errorf("expected a %s call; got %v", want, fake.seen())
		}
	}
	if fake.called("ListFunctions") {
		t.Error("--function was given — pulse shouldn't list the whole account")
	}

	root := filepath.Join(dir, "createorder")
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatalf("the imported project doesn't load: %v", err)
	}

	// The function, verbatim from AWS.
	fn := cfg.Functions["createOrder"]
	if fn == nil {
		t.Fatalf("functions = %v", cfg.FunctionNames())
	}
	if fn.Runtime != "python3.12" || fn.Handler != "handler.handler" || fn.Timeout != 15 || fn.Memory != 512 {
		t.Errorf("function = %+v", fn)
	}

	// The route came from the resource policy, the queue from the mapping.
	var got []string
	for _, tr := range cfg.Triggers {
		if tr.Type == "http" {
			got = append(got, tr.Method+" "+tr.Path)
		} else {
			got = append(got, fmt.Sprintf("sqs %s x%d", tr.Queue, tr.BatchSize))
		}
	}
	if want := "POST /orders,sqs order-events x10"; strings.Join(got, ",") != want {
		t.Errorf("triggers = %v, want %s", got, want)
	}

	// The queue's real attributes, plus the DLQ it points at.
	q := cfg.Resources.Queues["order-events"]
	if q == nil || q.VisibilityTimeout != 45 || q.DLQ != "order-events-dlq" || q.MaxReceiveCount != 5 {
		t.Errorf("queue = %+v (should mirror GetQueueAttributes)", q)
	}
	if cfg.Resources.Queues["order-events-dlq"] == nil {
		t.Error("a referenced DLQ must exist locally, or retries have nowhere to land")
	}

	// The strong guess (IAM + env agree) was taken, with its real key schema;
	// the code-only mention was not.
	tbl := cfg.Resources.Tables["orders"]
	if tbl == nil {
		t.Fatalf("tables = %v", cfg.Resources.Tables)
	}
	if tbl.PK.Name != "pk" || tbl.SK == nil || tbl.SK.Name != "createdAt" || tbl.SK.Type != "N" {
		t.Errorf("table = %+v (DescribeTable is the source of truth for keys)", tbl)
	}
	if cfg.Resources.Tables["audit-log"] != nil {
		t.Error("a code-only mention should not be imported unless the user checks it")
	}

	// The handler is on disk, where pulse.yaml says it is.
	if body := readFile(t, root, "functions/createOrder/handler.py"); !strings.Contains(body, "def handler") {
		t.Errorf("handler.py = %q", body)
	}

	// Secrets: names travel, values don't.
	env := readFile(t, root, ".env")
	if strings.Contains(env, "sk_live_51REAL") {
		t.Error(".env carried a live secret without --with-values")
	}
	if !strings.Contains(env, "STRIPE_KEY=CHANGE_ME") || !strings.Contains(env, "ORDERS_TABLE=CHANGE_ME") {
		t.Errorf(".env = %q", env)
	}
	if v := cfg.DotEnv["STRIPE_KEY"]; v != "CHANGE_ME" {
		t.Errorf("the written .env should load back through config: %q", v)
	}

	// The layer is the classic silent breakage; it must be loud.
	notes := readFile(t, root, "IMPORT-NOTES.md")
	if !strings.Contains(notes, "layer") {
		t.Errorf("IMPORT-NOTES.md should record the layer:\n%s", notes)
	}
	if !strings.Contains(screen, "layer") {
		t.Errorf("the layer should also be on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "111122223333") {
		t.Errorf("the account should be named before anything is read:\n%s", screen)
	}
}

func TestImportAWSDryRunWritesNothing(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	screen, dir, err := runImport(t, "\n", "--function", "createOrder", "--dry-run")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, screen)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("--dry-run wrote %v", entries)
	}
	if !strings.Contains(screen, "nothing was written") {
		t.Errorf("dry run should say so:\n%s", screen)
	}
	// It should show the actual file, not just a summary.
	for _, want := range []string{"project: createorder", "runtime: python3.12", "queue: order-events"} {
		if !strings.Contains(screen, want) {
			t.Errorf("dry run should print the pulse.yaml (missing %q):\n%s", want, screen)
		}
	}
}

func TestImportAWSWithValuesCopiesSecretsOnlyWhenAsked(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	screen, dir, err := runImport(t, "", "--function", "createOrder", "--yes", "--with-values")
	if err != nil {
		t.Fatalf("import failed: %v\n%s", err, screen)
	}
	env := readFile(t, filepath.Join(dir, "createorder"), ".env")
	if !strings.Contains(env, "STRIPE_KEY=sk_live_51REAL") {
		t.Errorf("--with-values should copy the real value, got:\n%s", env)
	}
	if !strings.Contains(screen, "--with-values") {
		t.Errorf("copying secrets to disk must be announced:\n%s", screen)
	}
}

// A locked-down account is the common case in a real org: the import must
// still produce a working project and say what it lost.
func TestImportAWSDegradesWhenIAMIsDenied(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	fake := startFakeAWS(t)
	fake.noIAM = true

	screen, dir, err := runImport(t, "", "--function", "createOrder", "--yes")
	if err != nil {
		t.Fatalf("an IAM denial must not fail the import: %v\n%s", err, screen)
	}
	if !strings.Contains(screen, "execution role policy") {
		t.Errorf("the lost read should be named on screen:\n%s", screen)
	}

	root := filepath.Join(dir, "createorder")
	if _, err := config.Load(filepath.Join(root, config.FileName)); err != nil {
		t.Fatalf("the project should still be valid: %v", err)
	}
	if !strings.Contains(readFile(t, root, "IMPORT-NOTES.md"), "execution role policy") {
		t.Error("the gap should outlive the terminal, in IMPORT-NOTES.md")
	}
	// The env-var signal alone is still deliberate enough to be strong, so
	// orders survives losing IAM.
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Tables["orders"] == nil {
		t.Error("ORDERS_TABLE=orders is evidence enough on its own")
	}
}

func TestImportAWSPicksAFunctionWhenNoneIsNamed(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	fake := startFakeAWS(t)

	// The picker lists both; 1 is the importable one (java17 sorts after).
	screen, dir, err := runImport(t, "1\n\ny\n")
	if err != nil {
		t.Fatalf("import failed: %v\n%s", err, screen)
	}
	if !fake.called("ListFunctions") {
		t.Error("with no --function, pulse should list the region")
	}
	if !strings.Contains(screen, "legacyReport") || !strings.Contains(screen, "java17") {
		t.Errorf("the picker should show what can't run and why:\n%s", screen)
	}
	if _, err := os.Stat(filepath.Join(dir, "createorder", config.FileName)); err != nil {
		t.Errorf("nothing was imported: %v", err)
	}
}

func TestImportAWSDeclinedAtTheConfirmationWritesNothing(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	screen, dir, err := runImport(t, "\nn\n", "--function", "createOrder")
	if err != nil {
		t.Fatalf("declining is not an error: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("answering no still wrote %v", entries)
	}
	if !strings.Contains(screen, "nothing was written") {
		t.Errorf("say so plainly:\n%s", screen)
	}
}

// A pipe or CI job must never be left waiting at a prompt: no TTY means take
// the defaults and finish.
func TestImportAWSNonInteractiveNeverPrompts(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "0")
	startFakeAWS(t)

	screen, dir, err := runImport(t, "", "--function", "createOrder")
	if err != nil {
		t.Fatalf("import failed: %v\n%s", err, screen)
	}
	for _, prompt := range []string{"toggle", "(Y/n)", "pick 1-"} {
		if strings.Contains(screen, prompt) {
			t.Errorf("prompted without a terminal (%q):\n%s", prompt, screen)
		}
	}
	cfg, err := config.Load(filepath.Join(dir, "createorder", config.FileName))
	if err != nil {
		t.Fatalf("the project should still be written: %v", err)
	}
	// Defaults mean the strong guess only.
	if cfg.Resources.Tables["orders"] == nil || cfg.Resources.Tables["audit-log"] != nil {
		t.Errorf("tables = %v, want only the strong guess", cfg.Resources.Tables)
	}
}

func TestImportAWSNameOverridesTheDirectory(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	_, dir, err := runImport(t, "", "--function", "createOrder", "--yes", "--name", "Shop-API")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, "shop-api", config.FileName))
	if err != nil {
		t.Fatalf("--name should decide the directory and the project name: %v", err)
	}
	if cfg.Project != "shop-api" {
		t.Errorf("project = %q, want shop-api (normalized)", cfg.Project)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// The permissions story, end to end: a denial on the one mandatory read must
// name the exact IAM action and point at the flag that prints the policy.
func TestImportAWSAccessDeniedNamesTheActionAndThePolicy(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	fake := startFakeAWS(t)
	fake.denyGetFunction = true

	screen, dir, err := runImport(t, "", "--function", "createOrder", "--yes")
	if err == nil {
		t.Fatalf("an AccessDenied on GetFunction must fail the import\n%s", screen)
	}
	if !strings.Contains(err.Error(), "lambda:GetFunction") {
		t.Errorf("error should name the refused action, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--policy") {
		t.Errorf("error should point at the policy printer, got: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed import must write nothing, found %v", entries)
	}
}

// --policy is what someone runs BECAUSE they have no access yet, so it must
// not need credentials, a region, or a profile.
func TestImportAWSPolicyWorksWithoutCredentials(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ENDPOINT_URL"} {
		t.Setenv(k, "")
	}

	screen, _, err := runImport(t, "", "--policy")
	if err != nil {
		t.Fatalf("--policy must work with no AWS setup at all: %v", err)
	}
	for _, want := range []string{
		"lambda:GetFunction", "dynamodb:DescribeTable", "sts:GetCallerIdentity",
		"PulseImportReadOnly", "read-only",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("policy output missing %q:\n%s", want, screen)
		}
	}
	// Every action carries its reason — that is what makes it a request an
	// admin can approve rather than a wall of strings.
	if !strings.Contains(screen, "confirm which account") {
		t.Errorf("the why column is missing:\n%s", screen)
	}
	if strings.Contains(screen, "\"Action\": \"*\"") {
		t.Error("the policy must not contain a wildcard action")
	}
}

// A typo is the likeliest mistake anyone makes here, and the generic taxonomy
// answers it with "https response error StatusCode: 404" plus advice to check
// connectivity — which is fine and not the problem.
func TestImportAWSTypoSuggestsTheRealFunction(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	_, dir, err := runImport(t, "", "--function", "createOrderr", "--yes")
	if err == nil {
		t.Fatal("want an error for a function that doesn't exist")
	}
	for _, want := range []string{`no Lambda function named "createOrderr"`, "eu-west-1", `did you mean "createOrder"?`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "https response error") || strings.Contains(err.Error(), "connectivity") {
		t.Errorf("error is relaying SDK internals or wrong advice: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("nothing should be written, found %v", entries)
	}
}

// A profile that doesn't exist must be caught before pulse asks anything —
// it used to prompt for a region for a profile that isn't there.
func TestImportAWSUnknownProfileFailsBeforeAnyQuestion(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte("[profile dev]\nregion = eu-west-1\n[profile prod]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	screen, _, err := runImport(t, "", "--function", "createOrder", "--profile", "prd", "--yes")
	if err == nil {
		t.Fatal("want an error for an unknown profile")
	}
	if !strings.Contains(err.Error(), `"prd" isn't configured`) || !strings.Contains(err.Error(), `did you mean "prod"?`) {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(screen, "which region?") {
		t.Errorf("asked for a region for a profile that doesn't exist:\n%s", screen)
	}
}

// --dry-run means "show me what you'd do", so it must not turn into a
// conversation: it takes the pre-checked defaults like --yes does.
func TestImportAWSDryRunAsksNothing(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	// Empty stdin: any prompt would fail with "cancelled" rather than hang.
	screen, dir, err := runImport(t, "", "--function", "createOrder", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run should not need input: %v\n%s", err, screen)
	}
	for _, prompt := range []string{"toggle", "(Y/n)"} {
		if strings.Contains(screen, prompt) {
			t.Errorf("--dry-run prompted (%q):\n%s", prompt, screen)
		}
	}
	// It still has to show the plan it would write, guesses included.
	if !strings.Contains(screen, "project: createorder") || !strings.Contains(screen, "orders") {
		t.Errorf("dry run should still show the full plan:\n%s", screen)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("--dry-run wrote %v", entries)
	}
}

// The right name in the wrong region is as common as a typo, and the two are
// indistinguishable from "not found" unless pulse looks.
func TestImportAWSFindsTheFunctionInAnotherRegion(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)
	// The function lives in eu-west-1 (homeRegion); ask in the wrong one.
	t.Setenv("AWS_REGION", "us-east-1")

	_, dir, err := runImport(t, "", "--function", "createOrder", "--yes")
	if err == nil {
		t.Fatal("want an error when the function isn't in the region asked for")
	}
	if !strings.Contains(err.Error(), "isn't in us-east-1") || !strings.Contains(err.Error(), "it's in eu-west-1") {
		t.Errorf("error should say where it actually is, got: %v", err)
	}
	// And the fix must be the command that works.
	if !strings.Contains(err.Error(), "--region eu-west-1") {
		t.Errorf("fix should be runnable, got: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("nothing should be written, found %v", entries)
	}
}

// A name that exists nowhere must stay a not-found — the probe must not
// invent a region.
func TestImportAWSProbeDoesNotInventARegion(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	_, _, err := runImport(t, "", "--function", "nowhere", "--yes")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), `no Lambda function named "nowhere"`) {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), "it's in") {
		t.Errorf("the probe claimed a region for a function that doesn't exist: %v", err)
	}
}

// Dependencies: an imported bundle that ships only a manifest gets installed,
// the way `pulse init` installs what it scaffolds.
func TestImportAWSInstallsDependencies(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	screen, dir, err := runImport(t, "", "--function", "createOrder", "--yes")
	if err != nil {
		t.Fatalf("import failed: %v\n%s", err, screen)
	}
	root := filepath.Join(dir, "createorder")
	// The fake's package carries requirements.txt, so pulse should have built
	// the venv at the PROJECT root (where the runner looks), not beside the code.
	if _, err := os.Stat(filepath.Join(root, ".venv")); err != nil {
		t.Errorf(".venv should exist at the project root: %v\n%s", err, screen)
	}
	if _, err := os.Stat(filepath.Join(root, "functions", "createOrder", ".venv")); err == nil {
		t.Error(".venv must not be created beside the code — the runner won't find it there")
	}
	// Having installed, the manual command is noise.
	if strings.Contains(screen, "python3 -m venv .venv &&") {
		t.Errorf("still printing the manual command after installing:\n%s", screen)
	}
}

func TestImportAWSNoInstallSkipsAndSaysHow(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	startFakeAWS(t)

	screen, dir, err := runImport(t, "", "--function", "createOrder", "--yes", "--no-install")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "createorder", ".venv")); err == nil {
		t.Error("--no-install still created a venv")
	}
	if !strings.Contains(screen, "python3 -m venv .venv &&") {
		t.Errorf("--no-install must print the command it skipped:\n%s", screen)
	}
}

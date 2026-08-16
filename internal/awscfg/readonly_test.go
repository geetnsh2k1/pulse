package awscfg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// guardFixture builds a config the way pulse does, pointed at a server that
// records anything reaching it. Nothing should reach it for a mutating call.
func guardFixture(t *testing.T) (hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		t.Errorf("a request reached AWS that should have been blocked: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return &n
}

// The promise a user is asked to trust when pointing this at their employer's
// account: pulse cannot alter anything. Not "doesn't" — cannot. Every mutating
// call below is refused before it is signed, so it never reaches AWS and never
// appears in CloudTrail.
func TestGuardBlocksEveryMutatingCall(t *testing.T) {
	hits := guardFixture(t)

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx := context.Background()

	lam := lambda.NewFromConfig(cfg)
	ddb := dynamodb.NewFromConfig(cfg)
	q := sqs.NewFromConfig(cfg)

	// The calls that would actually damage an account, one per service.
	attempts := map[string]func() error{
		"lambda:DeleteFunction": func() error {
			_, err := lam.DeleteFunction(ctx, &lambda.DeleteFunctionInput{FunctionName: str("victim")})
			return err
		},
		"lambda:UpdateFunctionCode": func() error {
			_, err := lam.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{FunctionName: str("victim")})
			return err
		},
		"lambda:Invoke": func() error {
			_, err := lam.Invoke(ctx, &lambda.InvokeInput{FunctionName: str("victim")})
			return err
		},
		"lambda:AddPermission": func() error {
			_, err := lam.AddPermission(ctx, &lambda.AddPermissionInput{FunctionName: str("victim")})
			return err
		},
		"dynamodb:PutItem": func() error {
			_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: str("victim"),
				Item: map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: "x"}}})
			return err
		},
		"dynamodb:DeleteTable": func() error {
			_, err := ddb.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: str("victim")})
			return err
		},
		"dynamodb:Scan": func() error { // reads DATA — not on the list either
			_, err := ddb.Scan(ctx, &dynamodb.ScanInput{TableName: str("victim")})
			return err
		},
		"sqs:SendMessage": func() error {
			_, err := q.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: str("http://x/q"), MessageBody: str("x")})
			return err
		},
		"sqs:PurgeQueue": func() error {
			_, err := q.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: str("http://x/q")})
			return err
		},
		"sqs:ReceiveMessage": func() error { // consuming a production queue would be destructive
			_, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: str("http://x/q")})
			return err
		},
	}

	for name, call := range attempts {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s was NOT blocked", name)
			}
			var ro *ReadOnlyError
			if !errors.As(err, &ro) {
				t.Fatalf("%s failed for the wrong reason (%v) — the guard must be what stops it", name, err)
			}
			// The message has to make clear whose fault it is.
			if !strings.Contains(ro.Error(), "bug in pulse") || !strings.Contains(ro.Error(), "nothing was sent to AWS") {
				t.Errorf("unhelpful guard message: %s", ro.Error())
			}
		})
	}
	if *hits != 0 {
		t.Errorf("%d blocked call(s) still reached the network", *hits)
	}
}

// …and the reads pulse actually needs still work, or the guard would be a
// very safe way of doing nothing.
func TestGuardAllowsTheReadsImportNeeds(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Functions":[],"QueueUrls":[]}`))
	}))
	defer srv.Close()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := lambda.NewFromConfig(cfg).ListFunctions(context.Background(),
		&lambda.ListFunctionsInput{}); err != nil {
		var ro *ReadOnlyError
		if errors.As(err, &ro) {
			t.Fatalf("the guard blocked a read pulse needs: %v", err)
		}
	}
}

// An operation pulse can call must be on the list; an operation on the list
// must be one pulse calls. The first direction is checked against the
// importer's interfaces in internal/importer/policy_test.go — this side just
// makes sure the list can't quietly grow.
func TestReadOnlyListIsExactlyWhatWeExpect(t *testing.T) {
	got := strings.Join(ReadOnlyOperationNames(), ",")
	want := strings.Join([]string{
		"DescribeTable", "GetApis", "GetCallerIdentity", "GetFunction", "GetIntegration",
		"GetLayerVersionByArn", "GetPolicy", "GetQueueAttributes", "GetQueueUrl", "GetRolePolicy",
		"GetRoutes", "ListEventSourceMappings", "ListFunctions", "ListQueues", "ListRolePolicies",
		"ListTables",
	}, ",")
	if got != want {
		t.Errorf("the read-only list changed.\n got: %s\nwant: %s\n\n"+
			"If this is deliberate, say so in the commit — it widens what pulse can do to an account.", got, want)
	}
	for _, op := range ReadOnlyOperationNames() {
		for _, verb := range []string{"Create", "Delete", "Put", "Update", "Set", "Send", "Add",
			"Remove", "Purge", "Invoke", "Publish", "Tag", "Write", "Attach", "Detach"} {
			if strings.HasPrefix(op, verb) {
				t.Errorf("%q is on the read-only list but its verb can change or consume state", op)
			}
		}
	}
}

func str(s string) *string { return &s }

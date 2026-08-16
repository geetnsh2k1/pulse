package importer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"
)

// ---- fakes: every AWS response is scripted, nothing reaches the network ----

type fakeLambda struct {
	fn       *lambda.GetFunctionOutput
	list     []lambda.ListFunctionsOutput // one entry per page
	mappings *lambda.ListEventSourceMappingsOutput
	policy   *lambda.GetPolicyOutput
	layers   map[string]string // layer arn -> download url
	errs     map[string]error
}

func (f *fakeLambda) GetLayerVersionByArn(_ context.Context, in *lambda.GetLayerVersionByArnInput, _ ...func(*lambda.Options)) (*lambda.GetLayerVersionByArnOutput, error) {
	if err := f.errs["GetLayerVersionByArn"]; err != nil {
		return nil, err
	}
	url, ok := f.layers[aws.ToString(in.Arn)]
	if !ok {
		return nil, errors.New("ResourceNotFoundException: no such layer")
	}
	return &lambda.GetLayerVersionByArnOutput{
		Content: &lambdatypes.LayerVersionContentOutput{Location: aws.String(url), CodeSize: 1024},
	}, nil
}

func (f *fakeLambda) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	if err := f.errs["GetFunction"]; err != nil {
		return nil, err
	}
	return f.fn, nil
}
func (f *fakeLambda) ListFunctions(_ context.Context, in *lambda.ListFunctionsInput, _ ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error) {
	if err := f.errs["ListFunctions"]; err != nil {
		return nil, err
	}
	page := 0
	if in.Marker != nil {
		page = atoiOr(aws.ToString(in.Marker), 0)
	}
	out := f.list[page]
	return &out, nil
}
func (f *fakeLambda) ListEventSourceMappings(context.Context, *lambda.ListEventSourceMappingsInput, ...func(*lambda.Options)) (*lambda.ListEventSourceMappingsOutput, error) {
	if err := f.errs["ListEventSourceMappings"]; err != nil {
		return nil, err
	}
	if f.mappings == nil {
		return &lambda.ListEventSourceMappingsOutput{}, nil
	}
	return f.mappings, nil
}
func (f *fakeLambda) GetPolicy(context.Context, *lambda.GetPolicyInput, ...func(*lambda.Options)) (*lambda.GetPolicyOutput, error) {
	if err := f.errs["GetPolicy"]; err != nil {
		return nil, err
	}
	if f.policy == nil {
		return nil, errors.New("ResourceNotFoundException: no policy")
	}
	return f.policy, nil
}

type fakeSQS struct {
	urls  []string
	attrs map[string]map[string]string
	errs  map[string]error
}

func (f *fakeSQS) ListQueues(context.Context, *sqs.ListQueuesInput, ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	if err := f.errs["ListQueues"]; err != nil {
		return nil, err
	}
	return &sqs.ListQueuesOutput{QueueUrls: f.urls}, nil
}
func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("https://sqs/1234/" + aws.ToString(in.QueueName))}, nil
}
func (f *fakeSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if err := f.errs["GetQueueAttributes"]; err != nil {
		return nil, err
	}
	name := arnTail(aws.ToString(in.QueueUrl))
	return &sqs.GetQueueAttributesOutput{Attributes: f.attrs[name]}, nil
}

type fakeDynamo struct {
	names  []string
	tables map[string]*ddbtypes.TableDescription
	errs   map[string]error
}

func (f *fakeDynamo) ListTables(context.Context, *dynamodb.ListTablesInput, ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error) {
	if err := f.errs["ListTables"]; err != nil {
		return nil, err
	}
	return &dynamodb.ListTablesOutput{TableNames: f.names}, nil
}
func (f *fakeDynamo) DescribeTable(_ context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if err := f.errs["DescribeTable"]; err != nil {
		return nil, err
	}
	return &dynamodb.DescribeTableOutput{Table: f.tables[aws.ToString(in.TableName)]}, nil
}

type fakeIAM struct {
	policies map[string]string // policy name -> document
	errs     map[string]error
}

func (f *fakeIAM) ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	if err := f.errs["ListRolePolicies"]; err != nil {
		return nil, err
	}
	var names []string
	for n := range f.policies {
		names = append(names, n)
	}
	return &iam.ListRolePoliciesOutput{PolicyNames: names}, nil
}
func (f *fakeIAM) GetRolePolicy(_ context.Context, in *iam.GetRolePolicyInput, _ ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	doc := f.policies[aws.ToString(in.PolicyName)]
	return &iam.GetRolePolicyOutput{PolicyDocument: aws.String(doc)}, nil
}

type fakeAPIGW struct {
	apis         []apigwtypes.Api
	routes       map[string][]apigwtypes.Route
	integrations map[string]string // integration id -> uri
	errs         map[string]error
}

func (f *fakeAPIGW) GetApis(context.Context, *apigatewayv2.GetApisInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetApisOutput, error) {
	if err := f.errs["GetApis"]; err != nil {
		return nil, err
	}
	return &apigatewayv2.GetApisOutput{Items: f.apis}, nil
}
func (f *fakeAPIGW) GetRoutes(_ context.Context, in *apigatewayv2.GetRoutesInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.GetRoutesOutput, error) {
	return &apigatewayv2.GetRoutesOutput{Items: f.routes[aws.ToString(in.ApiId)]}, nil
}
func (f *fakeAPIGW) GetIntegration(_ context.Context, in *apigatewayv2.GetIntegrationInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.GetIntegrationOutput, error) {
	uri, ok := f.integrations[aws.ToString(in.IntegrationId)]
	if !ok {
		return nil, errors.New("NotFoundException")
	}
	return &apigatewayv2.GetIntegrationOutput{IntegrationUri: aws.String(uri)}, nil
}

// ---- fixtures ----

func lambdaFixture() *fakeLambda {
	return &fakeLambda{
		fn: &lambda.GetFunctionOutput{
			Configuration: &lambdatypes.FunctionConfiguration{
				FunctionName: aws.String("createOrder"),
				Runtime:      lambdatypes.RuntimePython312,
				Handler:      aws.String("handler.handler"),
				Timeout:      aws.Int32(15),
				MemorySize:   aws.Int32(512),
				Role:         aws.String("arn:aws:iam::1234:role/createOrder-role"),
				PackageType:  lambdatypes.PackageTypeZip,
				CodeSize:     2 << 20,
				Environment: &lambdatypes.EnvironmentResponse{
					Variables: map[string]string{"TABLE_NAME": "orders", "SECRET": "shh"},
				},
			},
			Code: &lambdatypes.FunctionCodeLocation{Location: aws.String("https://presigned/code.zip")},
		},
		mappings: &lambda.ListEventSourceMappingsOutput{
			EventSourceMappings: []lambdatypes.EventSourceMappingConfiguration{{
				EventSourceArn: aws.String("arn:aws:sqs:eu-west-1:1234:order-events"),
				BatchSize:      aws.Int32(10),
				State:          aws.String("Enabled"),
			}},
		},
		policy: &lambda.GetPolicyOutput{Policy: aws.String(`{"Statement":[{
			"Effect":"Allow","Principal":{"Service":"apigateway.amazonaws.com"},
			"Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:execute-api:eu-west-1:1234:abc123/*/POST/orders"}}}]}`)},
		errs: map[string]error{},
	}
}

// discoverWith is discovererFixture with the Lambda side swapped, for tests
// that only care about how one scripted Lambda response is handled.
func discoverWith(t *testing.T, l *fakeLambda) *Discoverer {
	t.Helper()
	d := discovererFixture()
	d.Lambda = l
	return d
}

func discovererFixture() *Discoverer {
	return &Discoverer{
		Region: "eu-west-1",
		Lambda: lambdaFixture(),
		SQS: &fakeSQS{
			urls: []string{"https://sqs/1234/order-events", "https://sqs/1234/emails"},
			attrs: map[string]map[string]string{
				"order-events": {
					"QueueArn":          "arn:aws:sqs:eu-west-1:1234:order-events",
					"VisibilityTimeout": "45",
					"RedrivePolicy":     `{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:1234:order-dlq","maxReceiveCount":5}`,
				},
			},
			errs: map[string]error{},
		},
		Dynamo: &fakeDynamo{
			names: []string{"orders", "sessions"},
			tables: map[string]*ddbtypes.TableDescription{
				"orders": {
					TableName: aws.String("orders"),
					AttributeDefinitions: []ddbtypes.AttributeDefinition{
						{AttributeName: aws.String("customerId"), AttributeType: ddbtypes.ScalarAttributeTypeS},
						{AttributeName: aws.String("createdAt"), AttributeType: ddbtypes.ScalarAttributeTypeN},
					},
					KeySchema: []ddbtypes.KeySchemaElement{
						{AttributeName: aws.String("customerId"), KeyType: ddbtypes.KeyTypeHash},
						{AttributeName: aws.String("createdAt"), KeyType: ddbtypes.KeyTypeRange},
					},
					GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndexDescription{{IndexName: aws.String("byStatus")}},
					StreamSpecification:    &ddbtypes.StreamSpecification{StreamEnabled: aws.Bool(true)},
				},
			},
			errs: map[string]error{},
		},
		IAM: &fakeIAM{policies: map[string]string{
			"inline": `{"Statement":[{"Effect":"Allow","Action":"dynamodb:PutItem","Resource":"arn:aws:dynamodb:eu-west-1:1234:table/orders"}]}`,
		}, errs: map[string]error{}},
		APIGW: &fakeAPIGW{errs: map[string]error{}},
	}
}

// ---- tests ----

func TestDiscoverReadsEverything(t *testing.T) {
	d := discovererFixture()
	got, err := d.Discover(context.Background(), "createOrder")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if got.Region != "eu-west-1" {
		t.Errorf("region = %q", got.Region)
	}
	if got.Function.Name != "createOrder" || got.Function.Runtime != "python3.12" {
		t.Errorf("function = %+v", got.Function)
	}
	if got.Function.CodeURL == "" {
		t.Error("the presigned code URL is how the bundle gets downloaded later")
	}
	if len(got.EventSources) != 1 || got.EventSources[0].Kind != "sqs" || got.EventSources[0].BatchSize != 10 {
		t.Errorf("event sources = %+v", got.EventSources)
	}
	// The resource policy is the cheap, precise route source.
	if len(got.Routes) != 1 || got.Routes[0].Method != "POST" || got.Routes[0].Path != "/orders" {
		t.Errorf("routes = %+v", got.Routes)
	}
	// Names for the picker…
	if len(got.AllTables) != 2 || len(got.AllQueues) != 2 {
		t.Errorf("expected picker lists, got %d tables / %d queues", len(got.AllTables), len(got.AllQueues))
	}
	// …but the *triggering* queue is fully described, since its wiring matters.
	q, ok := findQueue(got.AllQueues, "order-events")
	if !ok || q.VisibilityTimeout != 45 || q.DLQName != "order-dlq" || q.MaxReceiveCount != 5 {
		t.Errorf("trigger queue not described: %+v", q)
	}
	if len(got.RolePolicy) != 1 || got.RolePolicy[0].Actions[0] != "dynamodb:PutItem" {
		t.Errorf("role policy = %+v", got.RolePolicy)
	}
	if len(d.Degraded) != 0 {
		t.Errorf("nothing should be degraded on the happy path: %+v", d.Degraded)
	}
}

// The whole point of the optional reads: a locked-down account still gets a
// usable import, with the loss reported rather than hidden.
func TestDiscoverDegradesInsteadOfFailing(t *testing.T) {
	d := discovererFixture()
	denied := errors.New("AccessDeniedException: not authorized")
	d.Lambda.(*fakeLambda).errs["ListEventSourceMappings"] = denied
	d.Lambda.(*fakeLambda).errs["GetPolicy"] = denied
	d.SQS.(*fakeSQS).errs["ListQueues"] = denied
	d.Dynamo.(*fakeDynamo).errs["ListTables"] = denied
	d.IAM.(*fakeIAM).errs["ListRolePolicies"] = denied
	d.APIGW.(*fakeAPIGW).errs["GetApis"] = denied

	got, err := d.Discover(context.Background(), "createOrder")
	if err != nil {
		t.Fatalf("denied optional reads must not fail the import: %v", err)
	}
	if got.Function.Name != "createOrder" {
		t.Error("the function itself must still come through")
	}
	joined := notesText(d.Degraded)
	for _, want := range []string{"event source", "http routes", "queue list", "table list", "role policy"} {
		if !strings.Contains(joined, want) {
			t.Errorf("degradation should mention %q, got: %s", want, joined)
		}
	}
}

// deniedAPIError and plainAPIError stand in for what the SDK hands back, so
// these tests need no network and no credentials.
type deniedAPIError struct{}

func (deniedAPIError) Error() string                 { return "api error AccessDeniedException" }
func (deniedAPIError) ErrorCode() string             { return "AccessDeniedException" }
func (deniedAPIError) ErrorMessage() string          { return "User is not authorized to perform this action" }
func (deniedAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

type plainAPIError struct{ code, msg string }

func (e *plainAPIError) Error() string                 { return e.code + ": " + e.msg }
func (e *plainAPIError) ErrorCode() string             { return e.code }
func (e *plainAPIError) ErrorMessage() string          { return e.msg }
func (e *plainAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// A degraded read is where most people actually meet AccessDenied, and these
// notes are copied into IMPORT-NOTES.md — so they have to name the missing
// permission, not relay SDK prose.
func TestDegradedNotesNameTheMissingPermission(t *testing.T) {
	d := discovererFixture()
	denied := &smithy.OperationError{
		ServiceID: "IAM", OperationName: "ListRolePolicies",
		Err: &deniedAPIError{},
	}
	d.IAM.(*fakeIAM).errs["ListRolePolicies"] = denied

	if _, err := d.Discover(context.Background(), "createOrder"); err != nil {
		t.Fatalf("a denied optional read must not fail the import: %v", err)
	}
	note := notesText(d.Degraded)
	if !strings.Contains(note, "iam:ListRolePolicies") {
		t.Errorf("note should name the permission, got: %s", note)
	}
	if !strings.Contains(note, "--policy") {
		t.Errorf("note should say how to get it, got: %s", note)
	}
	// The consequence still has to be there — that's why the note exists.
	if !strings.Contains(note, "env vars alone") {
		t.Errorf("note should say what it costs, got: %s", note)
	}
	if strings.Contains(note, "https response error") || strings.Contains(note, "RequestID") {
		t.Errorf("note is relaying SDK internals: %s", note)
	}
}

// A failure that isn't about permissions still has to read like a sentence.
func TestShortErrKeepsMessagesReadable(t *testing.T) {
	long := &smithy.OperationError{ServiceID: "DynamoDB", OperationName: "DescribeTable",
		Err: &plainAPIError{code: "ValidationException",
			msg: "The provided key element does not match the schema and this message keeps going well past what fits on one line"}}
	got := shortErr(long)
	if !strings.HasPrefix(got, "ValidationException: ") {
		t.Errorf("the API error code is the most useful part, got %q", got)
	}
	if len(got) > 115 {
		t.Errorf("message too long for a note (%d chars): %q", len(got), got)
	}
	// Cut at a word boundary — "api error A…" reads like a pulse bug.
	if strings.HasSuffix(got, "…") {
		trimmed := strings.TrimSuffix(got, "…")
		if strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, ",") {
			t.Errorf("clipped mid-punctuation: %q", got)
		}
		if last := trimmed[strings.LastIndex(trimmed, " ")+1:]; len(last) < 3 {
			t.Errorf("clipped mid-word (%q): %q", last, got)
		}
	}
	if plain := shortErr(errors.New("operation error DynamoDB: DescribeTable, something broke")); plain == "" {
		t.Error("a non-API error must still say something")
	}
}

// Losing the function itself is fatal — there is nothing to import.
func TestDiscoverFailsWhenTheFunctionIsUnreadable(t *testing.T) {
	d := discovererFixture()
	d.Lambda.(*fakeLambda).errs["GetFunction"] = errors.New("ResourceNotFoundException: no such function")
	if _, err := d.Discover(context.Background(), "ghost"); err == nil {
		t.Fatal("expected an error when the function can't be read")
	}
}

// KMS-encrypted variables read back as ciphertext; saying nothing would
// leave someone debugging a value that looks like garbage.
func TestDiscoverFlagsEncryptedEnvironment(t *testing.T) {
	d := discovererFixture()
	d.Lambda.(*fakeLambda).fn.Configuration.KMSKeyArn = aws.String("arn:aws:kms:eu-west-1:1234:key/abc")
	if _, err := d.Discover(context.Background(), "createOrder"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notesText(d.Degraded), "encrypted") {
		t.Errorf("KMS encryption must be reported, got: %s", notesText(d.Degraded))
	}
}

// When the resource policy is wildcarded, the API Gateway walk is the
// fallback — and it must match the integration to this function only.
func TestRoutesFallBackToAPIGatewayWalk(t *testing.T) {
	d := discovererFixture()
	d.Lambda.(*fakeLambda).policy = &lambda.GetPolicyOutput{Policy: aws.String(`{"Statement":[{
		"Effect":"Allow","Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:execute-api:eu-west-1:1234:abc123/*/*"}}}]}`)}
	d.APIGW = &fakeAPIGW{
		apis: []apigwtypes.Api{{ApiId: aws.String("abc123"), ProtocolType: apigwtypes.ProtocolTypeHttp}},
		routes: map[string][]apigwtypes.Route{"abc123": {
			{RouteKey: aws.String("GET /orders/{id}"), Target: aws.String("integrations/int1")},
			{RouteKey: aws.String("POST /other"), Target: aws.String("integrations/int2")},
			{RouteKey: aws.String("$default"), Target: aws.String("integrations/int1")},
		}},
		integrations: map[string]string{
			"int1": "arn:aws:lambda:eu-west-1:1234:function:createOrder",
			"int2": "arn:aws:lambda:eu-west-1:1234:function:somethingElse",
		},
		errs: map[string]error{},
	}

	got, err := d.Discover(context.Background(), "createOrder")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 1 {
		t.Fatalf("want only this function's route, got %+v", got.Routes)
	}
	if got.Routes[0].Method != "GET" || got.Routes[0].Path != "/orders/{id}" {
		t.Errorf("route = %+v", got.Routes[0])
	}
}

func TestRouteFromExecuteARN(t *testing.T) {
	cases := []struct {
		arn          string
		wantOK       bool
		method, path string
	}{
		{"arn:aws:execute-api:eu-west-1:1234:abc123/prod/POST/orders", true, "POST", "/orders"},
		{"arn:aws:execute-api:eu-west-1:1234:abc123/*/GET/orders/{id}", true, "GET", "/orders/{id}"},
		{"arn:aws:execute-api:eu-west-1:1234:abc123/*/*", false, "", ""}, // too broad to be a fact
		{"arn:aws:execute-api:eu-west-1:1234:abc123/*/POST/*", false, "", ""},
		{"arn:aws:sqs:eu-west-1:1234:queue", false, "", ""},
	}
	for _, c := range cases {
		got, ok := routeFromExecuteARN(c.arn)
		if ok != c.wantOK {
			t.Errorf("routeFromExecuteARN(%q) ok = %v, want %v", c.arn, ok, c.wantOK)
			continue
		}
		if ok && (got.Method != c.method || got.Path != c.path) {
			t.Errorf("routeFromExecuteARN(%q) = %s %s, want %s %s", c.arn, got.Method, got.Path, c.method, c.path)
		}
	}
}

func TestListFunctionsMarksWhatPulseCannotRun(t *testing.T) {
	d := discovererFixture()
	d.Lambda = &fakeLambda{
		list: []lambda.ListFunctionsOutput{{Functions: []lambdatypes.FunctionConfiguration{
			{FunctionName: aws.String("goFn"), Runtime: "provided.al2", Handler: aws.String("main"), PackageType: lambdatypes.PackageTypeZip},
			{FunctionName: aws.String("pyFn"), Runtime: lambdatypes.RuntimePython312, Handler: aws.String("h.h"), PackageType: lambdatypes.PackageTypeZip},
			{FunctionName: aws.String("imageFn"), Runtime: "", Handler: aws.String(""), PackageType: lambdatypes.PackageTypeImage},
		}}},
		errs: map[string]error{},
	}

	got, err := d.ListFunctions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 rows, got %d", len(got))
	}
	// Importable ones sort first so the picker leads with what works.
	if got[0].Name != "pyFn" || !got[0].Importable {
		t.Errorf("first row should be the runnable function, got %+v", got[0])
	}
	byName := map[string]FunctionSummary{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if s := byName["goFn"]; s.Importable || !strings.Contains(s.Why, "provided.al2") {
		t.Errorf("goFn should be marked unrunnable with a reason, got %+v", s)
	}
	if s := byName["imageFn"]; s.Importable || !strings.Contains(s.Why, "container-image") {
		t.Errorf("imageFn should be marked unrunnable with a reason, got %+v", s)
	}
}

// The picker offers names; the *definition* always comes from AWS, which is
// what makes a picked table exact rather than assumed (PLAN §12.10).
func TestDescribeTableReadsRealSchema(t *testing.T) {
	d := discovererFixture()
	got, err := d.DescribeTable(context.Background(), "orders")
	if err != nil {
		t.Fatal(err)
	}
	if got.PK.Name != "customerId" || got.PK.Type != "S" {
		t.Errorf("pk = %+v", got.PK)
	}
	if got.SK == nil || got.SK.Name != "createdAt" || got.SK.Type != "N" {
		t.Errorf("sk = %+v", got.SK)
	}
	if got.GSICount != 1 || !got.Streams {
		t.Errorf("indexes/streams not reported: %+v", got)
	}
}

func TestParsePolicyDocumentHandlesBothShapes(t *testing.T) {
	// Action/Resource are each either a string or a list.
	doc := `{"Statement":[
		{"Effect":"Allow","Action":"sqs:SendMessage","Resource":"arn:aws:sqs:::jobs"},
		{"Effect":"Allow","Action":["dynamodb:GetItem","dynamodb:PutItem"],"Resource":["arn:a","arn:b"]}
	]}`
	got, err := parsePolicyDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d", len(got))
	}
	if len(got[0].Actions) != 1 || len(got[1].Actions) != 2 || len(got[1].Resources) != 2 {
		t.Errorf("shapes not normalized: %+v", got)
	}
}

func TestParsePolicyDocumentURLEncoded(t *testing.T) {
	got, err := parsePolicyDocument(`%7B%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%22sqs%3A%2A%22%2C%22Resource%22%3A%22%2A%22%7D%5D%7D`)
	if err != nil {
		t.Fatalf("URL-encoded documents are normal from IAM: %v", err)
	}
	if len(got) != 1 || got[0].Actions[0] != "sqs:*" {
		t.Errorf("got %+v", got)
	}
}

func TestSourceKind(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sqs:eu-west-1:1:q":                     "sqs",
		"arn:aws:dynamodb:eu-west-1:1:table/t/stream/x": "dynamodb-stream",
		"arn:aws:kinesis:eu-west-1:1:stream/s":          "kinesis",
		"arn:aws:kafka:eu-west-1:1:cluster/c":           "kafka",
		"arn:aws:mq:eu-west-1:1:broker/b":               "mq",
		"":                                              "unknown",
	}
	for arn, want := range cases {
		if got := sourceKind(arn, lambdatypes.EventSourceMappingConfiguration{}); got != want {
			t.Errorf("sourceKind(%q) = %q, want %q", arn, got, want)
		}
	}
}

// Discovery feeding the mapper is the real contract: fixtures in, a
// reviewable plan out, with evidence attached.
func TestDiscoveryFeedsBuildPlan(t *testing.T) {
	d := discovererFixture()
	disc, err := d.Discover(context.Background(), "createOrder")
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(*disc, "shop")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if len(p.Functions) != 1 || len(p.Triggers) != 2 { // one http, one sqs
		t.Fatalf("plan = %+v", p)
	}
	// orders should be guessed strongly: the env var names it AND the role
	// grants dynamodb:PutItem on it.
	var orders *Guess
	for i := range p.Guesses {
		if p.Guesses[i].Name == "orders" {
			orders = &p.Guesses[i]
		}
	}
	if orders == nil || !orders.Strong || len(orders.Signals) < 2 {
		t.Fatalf("orders should be a strong guess with both signals, got %+v", p.Guesses)
	}
}

// A Lambda resource policy produces an identical `execute-api` ARN for a REST
// API (v1, payload format 1.0) and an HTTP API (v2, normally 2.0). The two
// formats put the method and path in different places, so assuming the wrong
// one hands a working handler an event it doesn't recognize: it fails locally
// while production is fine — precisely the "looks like pulse almost works"
// failure pulse exists to prevent.
func TestRoutesFromPolicyResolveTheRealPayloadFormat(t *testing.T) {
	policyFor := func(apiID string) *lambda.GetPolicyOutput {
		return &lambda.GetPolicyOutput{Policy: aws.String(`{"Statement":[{
			"Effect":"Allow","Principal":{"Service":"apigateway.amazonaws.com"},
			"Condition":{"ArnLike":{"AWS:SourceArn":
			"arn:aws:execute-api:eu-west-1:1234:` + apiID + `/prod/POST/orders"}}}]}`)}
	}

	t.Run("REST API v1 — not in the HTTP API list", func(t *testing.T) {
		d := discovererFixture()
		d.Lambda.(*fakeLambda).policy = policyFor("restapi123")
		// The account's HTTP APIs do not include it, so it is a REST API.
		d.APIGW.(*fakeAPIGW).apis = []apigwtypes.Api{
			{ApiId: aws.String("httpapi999"), ProtocolType: apigwtypes.ProtocolTypeHttp},
		}

		got, err := d.Discover(context.Background(), "createOrder")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Routes) != 1 {
			t.Fatalf("routes = %+v", got.Routes)
		}
		if got.Routes[0].PayloadFormat != "1.0" {
			t.Errorf("payload format = %q, want 1.0 — a REST API always uses 1.0",
				got.Routes[0].PayloadFormat)
		}
	})

	t.Run("HTTP API v2 — in the list", func(t *testing.T) {
		d := discovererFixture()
		d.Lambda.(*fakeLambda).policy = policyFor("httpapi999")
		d.APIGW.(*fakeAPIGW).apis = []apigwtypes.Api{
			{ApiId: aws.String("httpapi999"), ProtocolType: apigwtypes.ProtocolTypeHttp},
		}

		got, err := d.Discover(context.Background(), "createOrder")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Routes) != 1 || got.Routes[0].PayloadFormat != "2.0" {
			t.Errorf("routes = %+v, want one route with payload format 2.0", got.Routes)
		}
	})

	// If the list can't be read, pulse must not silently pick either one.
	t.Run("cannot check — says so instead of assuming", func(t *testing.T) {
		d := discovererFixture()
		d.Lambda.(*fakeLambda).policy = policyFor("restapi123")
		d.APIGW.(*fakeAPIGW).errs["GetApis"] = &smithy.OperationError{
			ServiceID: "API Gateway V2", OperationName: "GetApis", Err: &deniedAPIError{},
		}

		if _, err := d.Discover(context.Background(), "createOrder"); err != nil {
			t.Fatal(err)
		}
		note := notesText(d.Degraded)
		if !strings.Contains(note, "payload format") || !strings.Contains(note, "1.0") {
			t.Errorf("an unresolvable payload format must be reported with the fix, got: %s", note)
		}
	})
}

// Layers hold the dependencies, so being unable to read one is the difference
// between a project that runs and one that fails on its first import. It must
// never fail the whole import, and it must record why so the plan can say.
func TestDeniedLayerIsRecordedNotFatal(t *testing.T) {
	l := lambdaFixture()
	l.fn.Configuration.Layers = []lambdatypes.Layer{
		{Arn: aws.String("arn:aws:lambda:eu-west-1:1:layer:deps:9")},
	}
	l.errs["GetLayerVersionByArn"] = &smithy.GenericAPIError{
		Code:    "AccessDeniedException",
		Message: "User is not authorized to perform: lambda:GetLayerVersion",
	}

	d, err := discoverWith(t, l).Discover(context.Background(), "createOrder")
	if err != nil {
		t.Fatalf("a denied layer must not fail the import: %v", err)
	}
	got := d.Function
	if len(got.Layers) != 1 {
		t.Fatalf("the layer should still be listed, got %+v", got.Layers)
	}
	if got.Layers[0].CodeURL != "" {
		t.Error("a denied layer must not look downloadable")
	}
	if !strings.Contains(got.Layers[0].Unreadable, "lambda:GetLayerVersion") {
		t.Errorf("the reason must name the permission, got %q", got.Layers[0].Unreadable)
	}
}

// The happy path: a readable layer comes back with somewhere to download it.
func TestLayerDownloadURLIsResolved(t *testing.T) {
	l := lambdaFixture()
	arn := "arn:aws:lambda:eu-west-1:1:layer:deps:9"
	l.fn.Configuration.Layers = []lambdatypes.Layer{{Arn: aws.String(arn)}}
	l.layers = map[string]string{arn: "https://presigned/deps.zip"}

	d, err := discoverWith(t, l).Discover(context.Background(), "createOrder")
	if err != nil {
		t.Fatal(err)
	}
	got := d.Function
	if len(got.Layers) != 1 || got.Layers[0].CodeURL != "https://presigned/deps.zip" {
		t.Errorf("layers = %+v", got.Layers)
	}
	if got.Layers[0].Name != "deps" || got.Layers[0].Version != "9" {
		t.Errorf("name/version parsed from the ARN = %q/%q", got.Layers[0].Name, got.Layers[0].Version)
	}
	if got.Layers[0].Unreadable != "" {
		t.Errorf("a readable layer carries no reason, got %q", got.Layers[0].Unreadable)
	}
}

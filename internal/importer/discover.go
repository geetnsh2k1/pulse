package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
)

// The narrow slice of each AWS API discovery uses. Interfaces rather than
// concrete clients so every test runs against fakes: nothing in this
// package's test suite can reach the network.
//
// Every method here is a read. There is deliberately no write anywhere in
// the importer.
//
// Adding a method here also adds a permission the user needs: ReadActions in
// policy.go must grant it, and policy_test.go fails until it does.
type (
	LambdaAPI interface {
		GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error)
		ListFunctions(context.Context, *lambda.ListFunctionsInput, ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error)
		ListEventSourceMappings(context.Context, *lambda.ListEventSourceMappingsInput, ...func(*lambda.Options)) (*lambda.ListEventSourceMappingsOutput, error)
		GetPolicy(context.Context, *lambda.GetPolicyInput, ...func(*lambda.Options)) (*lambda.GetPolicyOutput, error)
		GetLayerVersionByArn(context.Context, *lambda.GetLayerVersionByArnInput, ...func(*lambda.Options)) (*lambda.GetLayerVersionByArnOutput, error)
	}
	SQSAPI interface {
		ListQueues(context.Context, *sqs.ListQueuesInput, ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error)
		GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
		GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	}
	DynamoAPI interface {
		ListTables(context.Context, *dynamodb.ListTablesInput, ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error)
		DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	}
	APIGatewayAPI interface {
		GetApis(context.Context, *apigatewayv2.GetApisInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetApisOutput, error)
		GetRoutes(context.Context, *apigatewayv2.GetRoutesInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetRoutesOutput, error)
		GetIntegration(context.Context, *apigatewayv2.GetIntegrationInput, ...func(*apigatewayv2.Options)) (*apigatewayv2.GetIntegrationOutput, error)
	}
	IAMAPI interface {
		ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
		GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	}
)

// Discoverer reads one account+region. Region is carried explicitly so the
// plan records where a project came from.
type Discoverer struct {
	Lambda LambdaAPI
	SQS    SQSAPI
	Dynamo DynamoAPI
	APIGW  APIGatewayAPI
	IAM    IAMAPI
	Region string

	// Degraded collects the reads that were refused. Optional signals
	// (IAM introspection, API Gateway enumeration) must never fail an
	// import — they only make the guesses weaker, and the user deserves to
	// know that happened rather than wondering why nothing was found.
	mu       sync.Mutex
	Degraded []Note
}

// NewDiscoverer wires the real clients from a resolved AWS config.
func NewDiscoverer(cfg aws.Config, region string) *Discoverer {
	return &Discoverer{
		Lambda: lambda.NewFromConfig(cfg),
		SQS:    sqs.NewFromConfig(cfg),
		Dynamo: dynamodb.NewFromConfig(cfg),
		APIGW:  apigatewayv2.NewFromConfig(cfg),
		IAM:    iam.NewFromConfig(cfg),
		Region: region,
	}
}

func (d *Discoverer) degrade(subject, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Degraded = append(d.Degraded, Note{Subject: subject, Detail: detail})
}

// DegradedNotes is a snapshot of what couldn't be read. The caller folds
// these into the plan so a permission gap ends up in IMPORT-NOTES.md beside
// the project, not only in a terminal that scrolls away.
func (d *Discoverer) DegradedNotes() []Note {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Note(nil), d.Degraded...)
}

// FunctionSummary is the picker's row: enough to choose without another call.
type FunctionSummary struct {
	Name        string
	Runtime     string
	Handler     string
	PackageType string
	CodeSize    int64
	Importable  bool   // false when pulse would refuse it
	Why         string // why not, when Importable is false
}

// ListFunctions pages through every function in the region and marks which
// ones pulse could actually run, so the picker can show the truth up front
// instead of failing after a selection.
func (d *Discoverer) ListFunctions(ctx context.Context) ([]FunctionSummary, error) {
	var out []FunctionSummary
	var marker *string
	for {
		page, err := d.Lambda.ListFunctions(ctx, &lambda.ListFunctionsInput{Marker: marker})
		if err != nil {
			return nil, err
		}
		for _, f := range page.Functions {
			fn := Function{
				Name:        aws.ToString(f.FunctionName),
				Runtime:     string(f.Runtime),
				Handler:     aws.ToString(f.Handler),
				PackageType: string(f.PackageType),
				CodeSize:    f.CodeSize,
			}
			s := FunctionSummary{
				Name: fn.Name, Runtime: fn.Runtime, Handler: fn.Handler,
				PackageType: fn.PackageType, CodeSize: fn.CodeSize, Importable: true,
			}
			if err := refuse(fn); err != nil {
				var r *Refusal
				if ok := asRefusal(err, &r); ok {
					s.Importable, s.Why = false, r.Reason
				}
			}
			out = append(out, s)
		}
		if page.NextMarker == nil || aws.ToString(page.NextMarker) == "" {
			break
		}
		marker = page.NextMarker
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Importable != out[j].Importable {
			return out[i].Importable // runnable ones first
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// FindRegion looks for the same function name in other regions and returns
// the first candidate that has it, or "".
//
// This exists because "no function named X" is technically true and unhelpful
// when the real answer is "it's in us-east-1". GetFunction is the right probe:
// one exact lookup per region rather than paging every function, and it needs
// no permission the import doesn't already have. Runs on the error path only,
// concurrently, and gives up quietly.
func FindRegion(ctx context.Context, cfg aws.Config, fnName string, candidates []string) string {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	found := make([]string, len(candidates))
	var wg sync.WaitGroup
	for i, region := range candidates {
		wg.Add(1)
		go func(i int, region string) {
			defer wg.Done()
			client := lambda.NewFromConfig(cfg, func(o *lambda.Options) { o.Region = region })
			if _, err := client.GetFunction(ctx, &lambda.GetFunctionInput{
				FunctionName: aws.String(fnName),
			}); err == nil {
				found[i] = region
			}
		}(i, region)
	}
	wg.Wait()

	// Candidate order, not completion order, so the answer is deterministic.
	for _, r := range found {
		if r != "" {
			return r
		}
	}
	return ""
}

// Discover reads everything needed to plan one function. The independent
// reads run concurrently — six serial round trips to AWS is a visible wait,
// and this is meant to feel instant.
//
// Only the function itself is mandatory: every other read may be refused by
// IAM without stopping the import.
func (d *Discoverer) Discover(ctx context.Context, name string) (*Discovery, error) {
	out := &Discovery{Region: d.Region}

	fn, err := d.getFunction(ctx, name)
	if err != nil {
		return nil, err
	}
	out.Function = fn

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		sources, err := d.eventSources(ctx, name)
		if err != nil {
			d.degrade("event source mappings", whyUnreadable(err)+" · queue triggers may be missing")
			return
		}
		out.EventSources = sources
	}()

	go func() {
		defer wg.Done()
		routes, err := d.routes(ctx, fn)
		if err != nil {
			d.degrade("http routes", whyUnreadable(err)+" · add routes by hand after importing")
			return
		}
		out.Routes = routes
	}()

	go func() {
		defer wg.Done()
		names, err := d.queueNames(ctx)
		if err != nil {
			d.degrade("queue list", whyUnreadable(err)+" · the queue picker will be empty")
			return
		}
		out.AllQueues = names
	}()

	go func() {
		defer wg.Done()
		names, err := d.tableNames(ctx)
		if err != nil {
			d.degrade("table list", whyUnreadable(err)+" · the table picker will be empty")
			return
		}
		out.AllTables = names
	}()

	wg.Wait()

	// Layers carry the dependencies a deployment package doesn't ship, so a
	// function with layers usually cannot even be imported without them. Read
	// their download locations here; the writer merges them.
	d.resolveLayers(ctx, &out.Function)

	// The execution role sharpens resource guesses, and is the read most
	// likely to be denied — plenty of orgs lock IAM down. Optional by design.
	if fn.RoleARN != "" && d.IAM != nil {
		if stmts, err := d.rolePolicy(ctx, fn.RoleARN); err != nil {
			d.degrade("execution role policy", whyUnreadable(err)+" · resource guesses rely on env vars alone")
		} else {
			out.RolePolicy = stmts
		}
	}

	// Queues that actually trigger this function are worth a second call
	// each: their real visibility timeout and DLQ wiring matter, unlike the
	// hundred other queues in the account, which stay name-only for the
	// picker. Two calls per trigger beats DescribeQueue on everything.
	for _, es := range out.EventSources {
		if es.Kind != "sqs" {
			continue
		}
		qName := arnTail(es.ARN)
		full, err := d.DescribeQueue(ctx, qName)
		if err != nil {
			d.degrade("queue "+qName, whyUnreadable(err)+" · defaults applied locally")
			continue
		}
		out.AllQueues = mergeQueue(out.AllQueues, full)
	}
	return out, nil
}

// layerNameVersion splits arn:aws:lambda:region:acct:layer:NAME:VERSION.
// LayerName is the human name inside a layer ARN, for callers that hold an
// ARN from pulse.yaml and nothing else. Falls back to the ARN's last segment
// so a malformed ARN still prints as something recognisable.
func LayerName(arn string) string {
	name, _ := layerNameVersion(arn)
	return name
}

func layerNameVersion(arn string) (name, version string) {
	parts := strings.Split(arn, ":")
	if len(parts) < 8 {
		return arnTail(arn), ""
	}
	return parts[6], parts[7]
}

// resolveLayers turns the ARNs on a function into downloadable layers. A
// denial here is not fatal — the import proceeds and says plainly that the
// dependencies living in those layers will be missing locally, and which
// permission would fix it.
func (d *Discoverer) resolveLayers(ctx context.Context, fn *Function) {
	fn.Layers = d.ResolveLayers(ctx, fn.Layers)
}

// ResolveLayers fills in each layer's download URL, or the reason it could not
// be read. It never fails the caller: a layer pulse cannot fetch is a degraded
// import, not a broken one, and the reason travels with the layer so whoever
// reports it can say something true about that specific layer.
func (d *Discoverer) ResolveLayers(ctx context.Context, layers []Layer) []Layer {
	out := append([]Layer(nil), layers...)
	for i := range out {
		res, err := d.Lambda.GetLayerVersionByArn(ctx, &lambda.GetLayerVersionByArnInput{
			Arn: aws.String(out[i].ARN),
		})
		if err != nil {
			out[i].Unreadable = whyUnreadable(err)
			continue
		}
		if res.Content != nil {
			out[i].CodeURL = aws.ToString(res.Content.Location)
			out[i].CodeSize = res.Content.CodeSize
		}
	}
	return out
}

func (d *Discoverer) getFunction(ctx context.Context, name string) (Function, error) {
	res, err := d.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String(name)})
	if err != nil {
		return Function{}, err
	}
	if res.Configuration == nil {
		return Function{}, fmt.Errorf("AWS returned no configuration for %q", name)
	}
	c := res.Configuration

	fn := Function{
		Name:        aws.ToString(c.FunctionName),
		Runtime:     string(c.Runtime),
		Handler:     aws.ToString(c.Handler),
		TimeoutSec:  int(aws.ToInt32(c.Timeout)),
		MemoryMB:    int(aws.ToInt32(c.MemorySize)),
		RoleARN:     aws.ToString(c.Role),
		PackageType: string(c.PackageType),
		CodeSize:    c.CodeSize,
		Env:         map[string]string{},
	}
	if c.Environment != nil {
		for k, v := range c.Environment.Variables {
			fn.Env[k] = v
		}
	}
	if res.Code != nil {
		fn.CodeURL = aws.ToString(res.Code.Location)
	}
	for _, l := range c.Layers {
		arn := aws.ToString(l.Arn)
		name, version := layerNameVersion(arn)
		fn.Layers = append(fn.Layers, Layer{ARN: arn, Name: name, Version: version})
	}
	if c.VpcConfig != nil && len(c.VpcConfig.SubnetIds) > 0 {
		fn.VPCAttached = true
	}
	if res.Concurrency != nil {
		fn.Concurrency = res.Concurrency.ReservedConcurrentExecutions
	}
	// A KMS-encrypted environment means the values we can read are
	// ciphertext, not what the function sees.
	if c.KMSKeyArn != nil && aws.ToString(c.KMSKeyArn) != "" {
		d.degrade("encrypted environment", "variables are encrypted with a customer KMS key — pulse imports the names, but the values it can read are not the plaintext")
	}
	return fn, nil
}

func (d *Discoverer) eventSources(ctx context.Context, name string) ([]EventSource, error) {
	var out []EventSource
	var marker *string
	for {
		page, err := d.Lambda.ListEventSourceMappings(ctx, &lambda.ListEventSourceMappingsInput{
			FunctionName: aws.String(name), Marker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range page.EventSourceMappings {
			arn := aws.ToString(m.EventSourceArn)
			out = append(out, EventSource{
				Kind:      sourceKind(arn, m),
				ARN:       arn,
				BatchSize: int(aws.ToInt32(m.BatchSize)),
				Enabled:   strings.EqualFold(aws.ToString(m.State), "Enabled"),
				HasFilter: m.FilterCriteria != nil && len(m.FilterCriteria.Filters) > 0,
			})
		}
		if page.NextMarker == nil || aws.ToString(page.NextMarker) == "" {
			break
		}
		marker = page.NextMarker
	}
	return out, nil
}

// sourceKind names the event source from its ARN. Self-managed Kafka has no
// ARN at all, hence the fallback.
func sourceKind(arn string, m lambdatypes.EventSourceMappingConfiguration) string {
	switch {
	case strings.Contains(arn, ":sqs:"):
		return "sqs"
	case strings.Contains(arn, ":dynamodb:"):
		return "dynamodb-stream"
	case strings.Contains(arn, ":kinesis:"):
		return "kinesis"
	case strings.Contains(arn, ":kafka:"):
		return "kafka"
	case strings.Contains(arn, ":mq:"):
		return "mq"
	case m.SelfManagedEventSource != nil:
		return "kafka"
	case arn == "":
		return "unknown"
	}
	return "unknown"
}

// routes finds the HTTP routes pointing at this function. The resource
// policy is the cheap, precise path: API Gateway records the exact
// apiId/stage/METHOD/path it was granted. Enumerating every API in the
// account is the fallback for wildcarded policies.
func (d *Discoverer) routes(ctx context.Context, fn Function) ([]HTTPRoute, error) {
	fromPolicy, err := d.routesFromPolicy(ctx, fn.Name)
	if err == nil && len(fromPolicy) > 0 {
		// The policy gave us methods and paths but cannot say which KIND of API
		// this is, and that decides the event shape the handler receives.
		d.resolvePayloadFormats(ctx, fromPolicy)
		return fromPolicy, nil
	}
	if d.APIGW == nil {
		return fromPolicy, err
	}
	fromAPIs, apiErr := d.routesFromAPIs(ctx, fn.Name)
	if apiErr != nil {
		if err != nil {
			return nil, err // both paths failed; report the first
		}
		return fromPolicy, nil
	}
	return mergeRoutes(fromPolicy, fromAPIs), nil
}

// lambdaPolicy is the shape of a Lambda resource policy, trimmed to the
// parts that reveal an API Gateway integration.
type lambdaPolicy struct {
	Statement []struct {
		Effect    string `json:"Effect"`
		Principal any    `json:"Principal"`
		Condition struct {
			ArnLike   map[string]string `json:"ArnLike"`
			StringEqu map[string]string `json:"StringEquals"`
		} `json:"Condition"`
	} `json:"Statement"`
}

func (d *Discoverer) routesFromPolicy(ctx context.Context, name string) ([]HTTPRoute, error) {
	res, err := d.Lambda.GetPolicy(ctx, &lambda.GetPolicyInput{FunctionName: aws.String(name)})
	if err != nil {
		// No policy at all is normal for a queue-only function — not an error.
		if strings.Contains(err.Error(), "ResourceNotFound") {
			return nil, nil
		}
		return nil, err
	}
	doc := aws.ToString(res.Policy)
	if doc == "" {
		return nil, nil
	}
	// AWS sometimes returns the document URL-encoded.
	if strings.HasPrefix(strings.TrimSpace(doc), "%7B") {
		if dec, derr := url.QueryUnescape(doc); derr == nil {
			doc = dec
		}
	}
	var p lambdaPolicy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, fmt.Errorf("resource policy wasn't parseable JSON: %w", err)
	}

	var out []HTTPRoute
	for _, st := range p.Statement {
		if !strings.EqualFold(st.Effect, "Allow") {
			continue
		}
		for _, src := range []map[string]string{st.Condition.ArnLike, st.Condition.StringEqu} {
			for k, v := range src {
				if !strings.EqualFold(k, "AWS:SourceArn") || !strings.Contains(v, ":execute-api:") {
					continue
				}
				if r, ok := routeFromExecuteARN(v); ok {
					out = append(out, r)
				}
			}
		}
	}
	return out, nil
}

// routeFromExecuteARN reads arn:aws:execute-api:region:acct:apiId/stage/METHOD/path.
// A wildcard method or path means the policy is too broad to describe a
// route, so it yields nothing rather than a guess dressed as a fact.
func routeFromExecuteARN(arn string) (HTTPRoute, bool) {
	i := strings.Index(arn, ":execute-api:")
	if i < 0 {
		return HTTPRoute{}, false
	}
	rest := arn[i+len(":execute-api:"):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return HTTPRoute{}, false
	}
	// apiId is the last colon-separated field before the first slash.
	head := rest[:slash]
	apiID := head[strings.LastIndex(head, ":")+1:]

	parts := strings.Split(rest[slash+1:], "/")
	if len(parts) < 2 {
		return HTTPRoute{}, false
	}
	method := parts[1]
	path := "/" + strings.Join(parts[2:], "/")
	if method == "*" || strings.Contains(path, "*") {
		return HTTPRoute{}, false
	}
	return HTTPRoute{Method: method, Path: path, APIID: apiID, PayloadFormat: "2.0"}, true
}

// routesFromAPIs walks HTTP APIs and matches integrations whose URI names
// this function. Used when the resource policy is wildcarded or absent.
func (d *Discoverer) routesFromAPIs(ctx context.Context, fnName string) ([]HTTPRoute, error) {
	apis, err := d.APIGW.GetApis(ctx, &apigatewayv2.GetApisInput{})
	if err != nil {
		return nil, err
	}
	var out []HTTPRoute
	for _, api := range apis.Items {
		apiID := aws.ToString(api.ApiId)
		routes, err := d.APIGW.GetRoutes(ctx, &apigatewayv2.GetRoutesInput{ApiId: api.ApiId})
		if err != nil {
			continue // one unreadable API shouldn't lose the others
		}
		for _, r := range routes.Items {
			method, path, ok := splitRouteKey(aws.ToString(r.RouteKey))
			if !ok {
				continue
			}
			if !d.integrationTargets(ctx, apiID, r, fnName) {
				continue
			}
			out = append(out, HTTPRoute{
				Method: method, Path: path, APIID: apiID,
				PayloadFormat: payloadFormat(api),
			})
		}
	}
	return out, nil
}

// resolvePayloadFormats settles the one thing a Lambda resource policy cannot
// tell us: whether the API in front of this function is an HTTP API (v2,
// payload format 2.0 unless configured otherwise) or a REST API (v1, always
// 1.0). Both produce an identical-looking `execute-api` ARN.
//
// It matters more than it sounds. The two formats put the method and path in
// different places, so assuming the wrong one hands a working handler an event
// it doesn't recognize — it fails locally while production is fine, which is
// the exact "looks like pulse almost works" failure pulse exists to avoid.
//
// An API ID that is not in the v2 list is a REST API by elimination. If the
// list can't be read at all, nothing is assumed: the routes keep their default
// and the gap is reported.
func (d *Discoverer) resolvePayloadFormats(ctx context.Context, routes []HTTPRoute) {
	if d.APIGW == nil {
		return
	}
	formats, err := d.httpAPIFormats(ctx)
	if err != nil {
		d.degrade("api payload format", whyUnreadable(err)+
			" · assuming 2.0 (HTTP API) — if this is a REST API, set payloadFormat: \"1.0\" in pulse.yaml")
		return
	}
	for i := range routes {
		if f, ok := formats[routes[i].APIID]; ok {
			routes[i].PayloadFormat = f
			continue
		}
		// Not an HTTP API, so it is a REST API: those always use 1.0.
		routes[i].PayloadFormat = "1.0"
	}
}

// httpAPIFormats maps every v2 HTTP API in the region to its payload format,
// following pagination — a half-read list would mislabel real APIs as REST.
func (d *Discoverer) httpAPIFormats(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	var next *string
	for {
		page, err := d.APIGW.GetApis(ctx, &apigatewayv2.GetApisInput{NextToken: next})
		if err != nil {
			return nil, err
		}
		for _, api := range page.Items {
			out[aws.ToString(api.ApiId)] = payloadFormat(api)
		}
		if page.NextToken == nil || aws.ToString(page.NextToken) == "" {
			return out, nil
		}
		next = page.NextToken
	}
}

// integrationTargets reports whether a route's integration points at this
// function. An unreadable integration is treated as "no" rather than
// inventing a route.
func (d *Discoverer) integrationTargets(ctx context.Context, apiID string, r apigwtypes.Route, fnName string) bool {
	target := aws.ToString(r.Target) // "integrations/abc123"
	id := target[strings.LastIndex(target, "/")+1:]
	if id == "" {
		return false
	}
	res, err := d.APIGW.GetIntegration(ctx, &apigatewayv2.GetIntegrationInput{
		ApiId: aws.String(apiID), IntegrationId: aws.String(id),
	})
	if err != nil {
		return false
	}
	uri := aws.ToString(res.IntegrationUri)
	// The URI is the function ARN (or an ARN-in-a-path form); match the name
	// on a boundary so "orders" doesn't match "ordersArchive".
	return mentionsName(uri, fnName)
}

func payloadFormat(api apigwtypes.Api) string {
	if strings.EqualFold(string(api.ProtocolType), "HTTP") {
		return "2.0"
	}
	return "1.0"
}

// splitRouteKey parses "POST /orders" and rejects $default, which pulse has
// no equivalent for.
func splitRouteKey(key string) (method, path string, ok bool) {
	if key == "" || strings.HasPrefix(key, "$") {
		return "", "", false
	}
	m, p, found := strings.Cut(key, " ")
	if !found {
		return "", "", false
	}
	return m, p, true
}

func (d *Discoverer) queueNames(ctx context.Context) ([]Queue, error) {
	var out []Queue
	var token *string
	for {
		page, err := d.SQS.ListQueues(ctx, &sqs.ListQueuesInput{NextToken: token})
		if err != nil {
			return nil, err
		}
		for _, u := range page.QueueUrls {
			name := arnTail(u)
			out = append(out, Queue{Name: name, FIFO: strings.HasSuffix(name, ".fifo")})
		}
		if page.NextToken == nil || aws.ToString(page.NextToken) == "" {
			break
		}
		token = page.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DescribeQueue reads one queue's real attributes. Exported because the
// picker describes only what the user selects (PLAN §12.10).
func (d *Discoverer) DescribeQueue(ctx context.Context, name string) (Queue, error) {
	urlRes, err := d.SQS.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return Queue{}, err
	}
	attrs, err := d.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       urlRes.QueueUrl,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
	})
	if err != nil {
		return Queue{}, err
	}
	q := Queue{
		Name: name,
		ARN:  attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)],
		FIFO: strings.HasSuffix(name, ".fifo"),
	}
	q.VisibilityTimeout = atoiOr(attrs.Attributes[string(sqstypes.QueueAttributeNameVisibilityTimeout)], 30)

	// The redrive policy names the DLQ by ARN and the retry budget.
	if raw := attrs.Attributes[string(sqstypes.QueueAttributeNameRedrivePolicy)]; raw != "" {
		var rp struct {
			DeadLetterTargetArn string `json:"deadLetterTargetArn"`
			MaxReceiveCount     any    `json:"maxReceiveCount"`
		}
		if json.Unmarshal([]byte(raw), &rp) == nil {
			q.DLQName = arnTail(rp.DeadLetterTargetArn)
			q.MaxReceiveCount = anyToInt(rp.MaxReceiveCount)
		}
	}
	return q, nil
}

func (d *Discoverer) tableNames(ctx context.Context) ([]Table, error) {
	var out []Table
	var start *string
	for {
		page, err := d.Dynamo.ListTables(ctx, &dynamodb.ListTablesInput{ExclusiveStartTableName: start})
		if err != nil {
			return nil, err
		}
		for _, n := range page.TableNames {
			out = append(out, Table{Name: n})
		}
		if page.LastEvaluatedTableName == nil || aws.ToString(page.LastEvaluatedTableName) == "" {
			break
		}
		start = page.LastEvaluatedTableName
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DescribeTable reads one table's real key schema — the reason a picked
// table lands in pulse.yaml with the same pk/sk it has in production.
func (d *Discoverer) DescribeTable(ctx context.Context, name string) (Table, error) {
	res, err := d.Dynamo.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
	if err != nil {
		return Table{}, err
	}
	if res.Table == nil {
		return Table{}, fmt.Errorf("AWS returned no description for table %q", name)
	}
	t := res.Table
	out := Table{
		Name:     aws.ToString(t.TableName),
		GSICount: len(t.GlobalSecondaryIndexes),
		LSICount: len(t.LocalSecondaryIndexes),
		Streams:  t.StreamSpecification != nil && aws.ToBool(t.StreamSpecification.StreamEnabled),
	}
	types := map[string]string{}
	for _, a := range t.AttributeDefinitions {
		types[aws.ToString(a.AttributeName)] = string(a.AttributeType)
	}
	for _, k := range t.KeySchema {
		key := Key{Name: aws.ToString(k.AttributeName), Type: orS(types[aws.ToString(k.AttributeName)])}
		if k.KeyType == ddbtypes.KeyTypeHash {
			out.PK = key
		} else {
			sk := key
			out.SK = &sk
		}
	}
	return out, nil
}

// rolePolicy reads the inline policies attached to the execution role.
// Managed policies are deliberately not followed: they are usually broad
// ("AWSLambdaBasicExecutionRole") and would add noise, not signal.
func (d *Discoverer) rolePolicy(ctx context.Context, roleARN string) ([]PolicyStatement, error) {
	role := arnTail(roleARN)
	list, err := d.IAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(role)})
	if err != nil {
		return nil, err
	}
	var out []PolicyStatement
	for _, name := range list.PolicyNames {
		doc, err := d.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
			RoleName: aws.String(role), PolicyName: aws.String(name),
		})
		if err != nil {
			continue
		}
		stmts, err := parsePolicyDocument(aws.ToString(doc.PolicyDocument))
		if err != nil {
			continue
		}
		out = append(out, stmts...)
	}
	return out, nil
}

// parsePolicyDocument handles IAM's shape-shifting JSON: Action and
// Resource are each either a string or a list.
func parsePolicyDocument(doc string) ([]PolicyStatement, error) {
	if doc == "" {
		return nil, nil
	}
	if dec, err := url.QueryUnescape(doc); err == nil && strings.Contains(dec, "Statement") {
		doc = dec
	}
	var parsed struct {
		Statement []struct {
			Effect   string `json:"Effect"`
			Action   any    `json:"Action"`
			Resource any    `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		return nil, err
	}
	out := make([]PolicyStatement, 0, len(parsed.Statement))
	for _, st := range parsed.Statement {
		out = append(out, PolicyStatement{
			Effect:    st.Effect,
			Actions:   toStrings(st.Action),
			Resources: toStrings(st.Resource),
		})
	}
	return out, nil
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---- small helpers ----

// mergeQueue replaces a name-only entry with the fully described one.
func mergeQueue(list []Queue, full Queue) []Queue {
	for i, q := range list {
		if q.Name == full.Name {
			list[i] = full
			return list
		}
	}
	return append(list, full)
}

func mergeRoutes(a, b []HTTPRoute) []HTTPRoute {
	seen := map[string]bool{}
	var out []HTTPRoute
	for _, r := range append(a, b...) {
		key := strings.ToUpper(r.Method) + " " + normalizePath(r.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// shortErr keeps degradation notes readable: the SDK's full message names
// operations and request IDs nobody reading a summary needs.
// whyUnreadable explains a read that didn't happen, as the first clause of a
// note whose second clause is the consequence. A refused permission is the
// common case in a real organization and the only one with an exact answer, so
// it never gets buried in SDK prose — these notes are copied into
// IMPORT-NOTES.md, where "api error A…" would be useless twice over.
func whyUnreadable(err error) string {
	if action := awscfg.DeniedPermission(err); action != "" {
		return "no permission for " + action + " (see `pulse import aws --policy`)"
	}
	return "couldn't be read: " + shortErr(err)
}

func shortErr(err error) string {
	// An API error already carries the two things worth saying; the rest of
	// the SDK's message is request IDs and protocol detail.
	var api smithy.APIError
	if errors.As(err, &api) {
		code := api.ErrorCode()
		if msg := strings.TrimSpace(api.ErrorMessage()); msg != "" && msg != code {
			return code + ": " + clip(msg, 90)
		}
		return code
	}
	s := err.Error()
	if i := strings.Index(s, ": "); i > 0 && i < 40 {
		s = s[i+2:]
	}
	return clip(strings.TrimSpace(s), 90)
}

// clip trims at a word boundary — a message cut mid-token ("api error A…")
// reads like a bug in pulse rather than a long message.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexAny(cut, " ,;:"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func anyToInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		return atoiOr(t, 0)
	}
	return 0
}

// asRefusal is errors.As without importing errors into this file's hot path.
func asRefusal(err error, target **Refusal) bool {
	if r, ok := err.(*Refusal); ok {
		*target = r
		return true
	}
	return false
}

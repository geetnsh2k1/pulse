package awscfg

import (
	"context"
	"fmt"
	"sort"
	"strings"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
)

// Read-only, enforced rather than promised.
//
// pulse's AWS features are read-only by construction: no mutating call exists
// anywhere in the code, and the offline end-to-end test fails the build if a
// mutating verb ever reaches the wire. That is a property of today's code, and
// it is the wrong thing to ask a user to take on trust when they are pointing
// this at their employer's account.
//
// So every client built through this package carries a guard that refuses any
// operation not on the list below, before the request is signed or sent. A
// future feature that needs to write to AWS (export, say) has to add itself
// here deliberately — it cannot happen by accident, by a refactor, or by a
// dependency deciding to call something on our behalf.

// ReadOnlyOperations is the complete set of AWS operations pulse may issue.
// Names are the SDK's own operation names, which is what the middleware sees.
//
// Keep this in step with the API interfaces in internal/importer/discover.go —
// importer's policy_test.go reflects over those interfaces and fails if one of
// them is missing here.
var ReadOnlyOperations = map[string]bool{
	// identity preflight
	"GetCallerIdentity": true,

	// lambda
	"GetFunction":             true,
	"ListFunctions":           true,
	"ListEventSourceMappings": true,
	"GetPolicy":               true,
	"GetLayerVersionByArn":    true,

	// sqs
	"ListQueues":         true,
	"GetQueueUrl":        true,
	"GetQueueAttributes": true,

	// dynamodb
	"ListTables":    true,
	"DescribeTable": true,

	// apigatewayv2
	"GetApis":        true,
	"GetRoutes":      true,
	"GetIntegration": true,

	// iam
	"ListRolePolicies": true,
	"GetRolePolicy":    true,
}

// ReadOnlyError is returned when the guard stops an operation. It is a bug in
// pulse if a user ever sees this — which is exactly why it says so.
type ReadOnlyError struct{ Operation, Service string }

func (e *ReadOnlyError) Error() string {
	return fmt.Sprintf("pulse blocked %s.%s: it is not on the read-only list.\n"+
		"    This is a bug in pulse, not a problem with your account — nothing was sent to AWS.\n"+
		"    fix: please report it at https://github.com/geetnsh2k1/pulse/issues",
		strings.ToLower(strings.ReplaceAll(e.Service, " ", "")), e.Operation)
}

// readOnlyGuard installs the check at the start of the middleware stack, so a
// refused operation never gets signed, never gets sent, and never appears in
// the account's CloudTrail.
func readOnlyGuard(stack *middleware.Stack) error {
	return stack.Initialize.Add(
		middleware.InitializeMiddlewareFunc("pulseReadOnlyGuard",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
				middleware.InitializeOutput, middleware.Metadata, error) {

				op := awsmiddleware.GetOperationName(ctx)
				if !ReadOnlyOperations[op] {
					return middleware.InitializeOutput{}, middleware.Metadata{},
						&ReadOnlyError{Operation: op, Service: awsmiddleware.GetServiceID(ctx)}
				}
				return next.HandleInitialize(ctx, in)
			}),
		middleware.Before)
}

// ReadOnlyOperationNames lists the allowed operations, sorted — for docs and
// for tests that compare this set against the code that uses it.
func ReadOnlyOperationNames() []string {
	out := make([]string, 0, len(ReadOnlyOperations))
	for op := range ReadOnlyOperations {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

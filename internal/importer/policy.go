package importer

import (
	"encoding/json"
	"fmt"
	"sort"
)

// The permissions side of "read-only, always". AccessDenied is the most
// common way an import fails in a real organization, and the useful answer to
// it is not "ask your admin" — it is the exact policy to ask them for.
//
// This list must match the interfaces at the top of discover.go exactly.
// That isn't left to discipline: policy_test.go reflects over those
// interfaces and fails if an API is callable but ungranted, or granted but
// never called.

// ReadAction is one IAM action pulse may call, with the reason it needs it.
// The reason matters: an admin reading a permission request wants to know
// what it buys, not just what it grants.
type ReadAction struct {
	Action   string
	Why      string
	Optional bool // import degrades gracefully without it
}

// ReadActions is every action the importer can issue, in the order a reader
// would want them: preflight, the function, its triggers, its resources, then
// the optional extras.
func ReadActions() []ReadAction {
	return []ReadAction{
		{Action: "sts:GetCallerIdentity", Why: "confirm which account you're pointed at before reading anything"},

		{Action: "lambda:ListFunctions", Why: "list functions for the picker"},
		{Action: "lambda:GetFunction", Why: "runtime, handler, memory, timeout, env var names, code location"},
		{Action: "lambda:ListEventSourceMappings", Why: "which queues trigger the function"},
		{Action: "lambda:GetPolicy", Why: "which API Gateway routes invoke it"},

		{Action: "sqs:ListQueues", Why: "offer real queue names instead of asking you to type them"},
		{Action: "sqs:GetQueueUrl", Why: "resolve a queue name to its URL"},
		{Action: "sqs:GetQueueAttributes", Why: "copy the real visibility timeout and DLQ wiring"},

		{Action: "dynamodb:ListTables", Why: "offer real table names"},
		{Action: "dynamodb:DescribeTable", Why: "copy the real partition and sort keys"},

		{Action: "apigateway:GET", Why: "find routes when the function's resource policy is wildcarded", Optional: true},

		{Action: "iam:ListRolePolicies", Why: "read the execution role to guess which resources the code uses", Optional: true},
		{Action: "iam:GetRolePolicy", Why: "same — the policy document itself", Optional: true},
	}
}

// MinimalPolicyJSON is the pasteable IAM policy document.
//
// Resource is "*" because it has to be: lambda:ListFunctions,
// dynamodb:ListTables and sqs:ListQueues have no resource-level permissions
// in IAM at all, so there is nothing to scope them to. Every action in here
// is a read — the policy cannot create, change or delete anything.
func MinimalPolicyJSON() string {
	actions := make([]string, 0, len(ReadActions()))
	for _, a := range ReadActions() {
		actions = append(actions, a.Action)
	}
	sort.Strings(actions)

	doc := policyDoc{
		Version: "2012-10-17",
		Statement: []policyStatementDoc{{
			Sid:      "PulseImportReadOnly",
			Effect:   "Allow",
			Action:   actions,
			Resource: "*",
		}},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// Marshaling a fixed literal cannot fail; if it somehow does, say so
		// rather than printing an empty policy someone might paste.
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(out)
}

type policyDoc struct {
	Version   string               `json:"Version"`
	Statement []policyStatementDoc `json:"Statement"`
}

type policyStatementDoc struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

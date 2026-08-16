package importer

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
)

// The printed policy has to match what the code can actually call. This test
// walks the API interfaces themselves, so adding a method to LambdaAPI (or any
// of the others) without granting it fails here rather than in a stranger's
// terminal with an AccessDenied they can't act on.
func TestPolicyCoversEveryAPITheImporterCanCall(t *testing.T) {
	interfaces := map[string]reflect.Type{
		"lambda":       reflect.TypeOf((*LambdaAPI)(nil)).Elem(),
		"sqs":          reflect.TypeOf((*SQSAPI)(nil)).Elem(),
		"dynamodb":     reflect.TypeOf((*DynamoAPI)(nil)).Elem(),
		"apigatewayv2": reflect.TypeOf((*APIGatewayAPI)(nil)).Elem(),
		"iam":          reflect.TypeOf((*IAMAPI)(nil)).Elem(),
	}

	granted := map[string]bool{}
	for _, a := range ReadActions() {
		granted[a.Action] = true
	}

	for svc, iface := range interfaces {
		for i := 0; i < iface.NumMethod(); i++ {
			op := iface.Method(i).Name
			action := awscfg.IAMAction(svc, op)
			if !granted[action] {
				t.Errorf("%s.%s needs %q, which the policy doesn't grant — add it to ReadActions()",
					svc, op, action)
			}
		}
	}
}

// The reverse direction: nothing granted that isn't used. A read-only policy
// that asks for more than it needs is the reason security teams say no.
func TestPolicyGrantsNothingUnused(t *testing.T) {
	used := map[string]bool{"sts:GetCallerIdentity": true} // the preflight, not an interface
	for svc, iface := range map[string]reflect.Type{
		"lambda":       reflect.TypeOf((*LambdaAPI)(nil)).Elem(),
		"sqs":          reflect.TypeOf((*SQSAPI)(nil)).Elem(),
		"dynamodb":     reflect.TypeOf((*DynamoAPI)(nil)).Elem(),
		"apigatewayv2": reflect.TypeOf((*APIGatewayAPI)(nil)).Elem(),
		"iam":          reflect.TypeOf((*IAMAPI)(nil)).Elem(),
	} {
		for i := 0; i < iface.NumMethod(); i++ {
			used[awscfg.IAMAction(svc, iface.Method(i).Name)] = true
		}
	}
	for _, a := range ReadActions() {
		if !used[a.Action] {
			t.Errorf("policy grants %q but no code path calls it", a.Action)
		}
	}
}

func TestMinimalPolicyIsValidAndReadOnly(t *testing.T) {
	body := MinimalPolicyJSON()

	var doc struct {
		Version   string `json:"Version"`
		Statement []struct {
			Sid      string   `json:"Sid"`
			Effect   string   `json:"Effect"`
			Action   []string `json:"Action"`
			Resource string   `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the printed policy isn't valid JSON: %v\n%s", err, body)
	}
	if doc.Version != "2012-10-17" || len(doc.Statement) != 1 {
		t.Fatalf("policy = %s", body)
	}
	st := doc.Statement[0]
	if st.Effect != "Allow" || st.Resource != "*" || st.Sid == "" {
		t.Errorf("statement = %+v", st)
	}

	// The read-only promise, enforced on the document itself: no verb here may
	// change anything, and no wildcard may smuggle one in.
	mutating := []string{"Put", "Delete", "Create", "Update", "Set", "Send", "Add",
		"Remove", "Attach", "Detach", "Publish", "Invoke", "Tag", "Untag", "Write"}
	for _, action := range st.Action {
		if strings.Contains(action, "*") {
			t.Errorf("action %q is a wildcard — a minimal policy must be explicit", action)
		}
		verb := action[strings.Index(action, ":")+1:]
		for _, m := range mutating {
			if strings.HasPrefix(verb, m) {
				t.Errorf("action %q can modify AWS — import is read-only", action)
			}
		}
	}
	if !strings.Contains(body, "sts:GetCallerIdentity") {
		t.Error("the identity preflight must be in the policy, or the first call fails")
	}
	// Sorted, so two runs produce identical text and a diff means a real change.
	sorted := append([]string(nil), st.Action...)
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] > sorted[i] {
			t.Errorf("actions aren't sorted: %q before %q", sorted[i-1], sorted[i])
		}
	}
}

func TestEveryActionExplainsWhyItIsNeeded(t *testing.T) {
	for _, a := range ReadActions() {
		if a.Why == "" {
			t.Errorf("%s has no reason — an admin reading the request deserves one", a.Action)
		}
		if strings.HasSuffix(a.Why, ".") {
			t.Errorf("%s: reasons are printed inline, drop the full stop (%q)", a.Action, a.Why)
		}
	}
}

// Three lists have to agree or the read-only promise has a hole: the API
// interfaces (what the code CAN call), the printed IAM policy (what we ask the
// user to grant), and awscfg's runtime guard (what the SDK will actually let
// through). This checks the third against the first.
func TestRuntimeGuardAllowsEveryAPITheImporterCanCall(t *testing.T) {
	for svc, iface := range map[string]reflect.Type{
		"lambda":       reflect.TypeOf((*LambdaAPI)(nil)).Elem(),
		"sqs":          reflect.TypeOf((*SQSAPI)(nil)).Elem(),
		"dynamodb":     reflect.TypeOf((*DynamoAPI)(nil)).Elem(),
		"apigatewayv2": reflect.TypeOf((*APIGatewayAPI)(nil)).Elem(),
		"iam":          reflect.TypeOf((*IAMAPI)(nil)).Elem(),
	} {
		for i := 0; i < iface.NumMethod(); i++ {
			op := iface.Method(i).Name
			if !awscfg.ReadOnlyOperations[op] {
				t.Errorf("%s.%s would be blocked at runtime by awscfg's guard — add it to ReadOnlyOperations",
					svc, op)
			}
		}
	}
}

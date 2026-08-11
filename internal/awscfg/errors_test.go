package awscfg

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

// apiError fakes what the SDK hands back for a rejected call.
type apiError struct{ code string }

func (e apiError) Error() string                 { return "api error " + e.code }
func (e apiError) ErrorCode() string             { return e.code }
func (e apiError) ErrorMessage() string          { return e.code }
func (e apiError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// Every classified failure must name the cause AND carry a fix the user can
// act on — that is the whole point of the taxonomy.
func TestExplainClassifiesAndAlwaysOffersAFix(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCause string
		fixHas    string
	}{
		{"sso expired", errors.New("operation error SSO: the SSO session has expired or is invalid"), "sso expired", "aws sso login --profile prod"},
		{"no credentials", errors.New("failed to refresh cached credentials, no EC2 IMDS role found"), "no credentials", "aws configure --profile prod"},
		{"expired token", apiError{"ExpiredToken"}, "expired token", "refresh"},
		{"invalid creds", apiError{"InvalidClientTokenId"}, "invalid credentials", "aws configure --profile prod"},
		{"denied", apiError{"AccessDenied"}, "access denied", "policy"},
		{"throttled", apiError{"ThrottlingException"}, "throttled", "again"},
		{"dns", &net.DNSError{Err: "no such host", Name: "sts.amazonaws.com"}, "dns", "network"},
		{"timeout", context.DeadlineExceeded, "timeout", "connection"},
		{"unknown", errors.New("something nobody predicted"), "unknown", "pulse aws whoami"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Explain(c.err, Options{Profile: "prod"})
			var pe *Error
			if !errors.As(got, &pe) {
				t.Fatalf("Explain returned %T, want *awscfg.Error", got)
			}
			if pe.Cause != c.wantCause {
				t.Errorf("cause = %q, want %q", pe.Cause, c.wantCause)
			}
			if pe.Fix == "" {
				t.Error("every classified error must carry a fix")
			}
			if !strings.Contains(pe.Fix, c.fixHas) {
				t.Errorf("fix %q should mention %q", pe.Fix, c.fixHas)
			}
			if !strings.Contains(pe.Error(), "fix:") {
				t.Errorf("Error() should surface the fix, got %q", pe.Error())
			}
			if !errors.Is(got, c.err) && pe.Err == nil {
				t.Error("the original error must stay wrapped for debugging")
			}
		})
	}
}

func TestExplainProfileNotFoundListsWhatExists(t *testing.T) {
	writeAWSFiles(t, "[profile alpha]\n[profile beta]\n", "")
	got := Explain(errors.New("failed to get shared config profile, typo"), Options{Profile: "typo"})
	var pe *Error
	if !errors.As(got, &pe) {
		t.Fatalf("want *awscfg.Error, got %T", got)
	}
	if pe.Cause != "profile not found" {
		t.Fatalf("cause = %q", pe.Cause)
	}
	if !strings.Contains(pe.Fix, "alpha") || !strings.Contains(pe.Fix, "beta") {
		t.Errorf("fix should list real profiles, got %q", pe.Fix)
	}
	if !strings.Contains(pe.Msg, "typo") {
		t.Errorf("message should name the bad profile, got %q", pe.Msg)
	}
}

// AccessDenied is the failure a real organization actually hits, and "you
// aren't allowed to do this" leaves the user guessing what to request. The
// SDK always knows which call was refused.
func TestExplainNamesTheDeniedAction(t *testing.T) {
	denied := &smithy.OperationError{
		ServiceID:     "Lambda",
		OperationName: "GetFunction",
		Err:           apiError{"AccessDeniedException"},
	}
	got := Explain(denied, Options{Profile: "prod"})
	var pe *Error
	if !errors.As(got, &pe) {
		t.Fatalf("want *awscfg.Error, got %T", got)
	}
	if pe.Cause != "access denied" {
		t.Fatalf("cause = %q", pe.Cause)
	}
	if !strings.Contains(pe.Msg, "lambda:GetFunction") {
		t.Errorf("message should name the exact IAM action, got %q", pe.Msg)
	}
	if !strings.Contains(pe.Fix, "--policy") {
		t.Errorf("fix should point at the policy printer, got %q", pe.Fix)
	}

	// API Gateway authorizes by HTTP verb, so the operation name would send
	// someone hunting for an IAM action that doesn't exist.
	apigw := &smithy.OperationError{
		ServiceID: "API Gateway V2", OperationName: "GetApis", Err: apiError{"AccessDeniedException"},
	}
	if err := Explain(apigw, Options{Profile: "prod"}); !strings.Contains(err.Error(), "apigateway:GET") {
		t.Errorf("API Gateway denial should name apigateway:GET, got %q", err)
	}

	// Without an operation to name, the message must still read cleanly.
	plain := Explain(apiError{"AccessDenied"}, Options{Profile: "prod"})
	if !strings.Contains(plain.Error(), "not allowed to do this") {
		t.Errorf("bare denial reads badly: %q", plain)
	}
}

// A TLS-inspecting proxy and an unreachable proxy both look like generic
// network noise in the SDK's message, and neither remedy resembles the other.
func TestExplainSeparatesProxyAndTLSFailures(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://corp-proxy:3128")
	proxy := Explain(errors.New(`Get "https://sts.amazonaws.com/": proxyconnect tcp: dial tcp: connection refused`),
		Options{Profile: "prod"})
	var pe *Error
	if !errors.As(proxy, &pe) || pe.Cause != "proxy" {
		t.Fatalf("proxy failure classified as %v", proxy)
	}
	if !strings.Contains(pe.Fix, "HTTPS_PROXY=http://corp-proxy:3128") {
		t.Errorf("fix should name the proxy actually set, got %q", pe.Fix)
	}

	for _, err := range []error{
		x509.UnknownAuthorityError{},
		errors.New(`tls: failed to verify certificate: x509: certificate signed by unknown authority`),
	} {
		got := Explain(err, Options{Profile: "prod"})
		if !errors.As(got, &pe) || pe.Cause != "tls" {
			t.Errorf("TLS interception classified as %v", got)
			continue
		}
		if !strings.Contains(pe.Fix, "AWS_CA_BUNDLE") {
			t.Errorf("fix should name AWS_CA_BUNDLE, got %q", pe.Fix)
		}
	}
}

// The throttling message promises pulse already retried — it must not lie.
func TestThrottlingMessageMatchesTheRealRetryCount(t *testing.T) {
	got := Explain(apiError{"ThrottlingException"}, Options{Profile: "prod"})
	if !strings.Contains(got.Error(), fmt.Sprintf("retried %d times", maxAttempts)) {
		t.Errorf("message should state the real attempt count (%d), got %q", maxAttempts, got)
	}
	if maxAttempts < 2 {
		t.Error("claiming retries requires more than one attempt")
	}
}

func TestExplainNilStaysNil(t *testing.T) {
	if err := Explain(nil, Options{}); err != nil {
		t.Errorf("Explain(nil) = %v, want nil", err)
	}
}

func TestProfileLabelFallsBackLikeTheSDK(t *testing.T) {
	t.Setenv("AWS_PROFILE", "")
	if got := profileLabel(Options{}); got != "default" {
		t.Errorf("with nothing set, label = %q, want default", got)
	}
	t.Setenv("AWS_PROFILE", "from-env")
	if got := profileLabel(Options{}); got != "from-env" {
		t.Errorf("AWS_PROFILE ignored: %q", got)
	}
	if got := profileLabel(Options{Profile: "explicit"}); got != "explicit" {
		t.Errorf("--profile must win, got %q", got)
	}
}

// Whoami must fail on local configuration problems without ever reaching
// the network — nobody should need credentials to get a clear error.
func TestWhoamiNeedsARegionBeforeCalling(t *testing.T) {
	writeAWSFiles(t, "[profile noregion]\n", "[noregion]\naws_access_key_id=AKIA\naws_secret_access_key=x\n")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	_, err := Whoami(context.Background(), Options{Profile: "noregion"})
	if err == nil {
		t.Fatal("expected a region error")
	}
	var pe *Error
	if !errors.As(err, &pe) || pe.Cause != "no region" {
		t.Fatalf("want cause \"no region\", got %v", err)
	}
	if !strings.Contains(pe.Fix, "--region") {
		t.Errorf("fix should mention --region, got %q", pe.Fix)
	}
	if msg := pe.Error(); !strings.Contains(msg, "fix:") {
		t.Errorf("Error() should include the fix, got %q", msg)
	}
}

// Messages must name the real credential source. Blaming profile "default"
// when the caller used AWS_ACCESS_KEY_ID sends them to the wrong file.
func TestExplainAttributesEnvironmentCredentials(t *testing.T) {
	writeAWSFiles(t, "", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")

	got := Explain(apiError{"InvalidClientTokenId"}, Options{})
	var pe *Error
	if !errors.As(got, &pe) {
		t.Fatalf("want *Error, got %T", got)
	}
	if strings.Contains(pe.Msg, "profile") {
		t.Errorf("must not blame a profile, got %q", pe.Msg)
	}
	if !strings.Contains(pe.Msg, "environment variables") {
		t.Errorf("should name the env source, got %q", pe.Msg)
	}
	if !strings.Contains(pe.Fix, "AWS_ACCESS_KEY_ID") {
		t.Errorf("fix should point at the env vars, got %q", pe.Fix)
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	if got := EnvCredentialSource(); got != "" {
		t.Errorf("expected no source, got %q", got)
	}
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/v2/creds")
	if got := EnvCredentialSource(); !strings.Contains(got, "container") {
		t.Errorf("expected container source, got %q", got)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "sa-east-1")
	if got := EnvRegion(); got != "sa-east-1" {
		t.Errorf("EnvRegion should fall back to AWS_DEFAULT_REGION, got %q", got)
	}
}

package awscfg

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// callTimeout keeps a wrong profile or an unreachable network from hanging
// the CLI: AWS answers in well under a second when things are healthy.
const callTimeout = 10 * time.Second

// Options selects which account pulse reads from. Empty fields fall back to
// the standard AWS environment (AWS_PROFILE, AWS_REGION) and then to the
// shared config files, exactly like the aws CLI.
type Options struct {
	Profile string
	Region  string
}

// Identity is the answer to "whose account am I about to read?" — printed
// before any import so nobody discovers afterwards that they were pointed
// at production.
type Identity struct {
	Account string
	ARN     string
	UserID  string
	Region  string
	// Profile is the shared-config profile in play, empty when credentials
	// came from somewhere else. Naming a profile that isn't involved is worse
	// than naming nothing: it sends people to edit the wrong file.
	Profile string
	// Source is where the credentials actually came from, in words — a profile
	// name, "environment variables (AWS_ACCESS_KEY_ID)", or an instance role.
	Source string
}

// maxAttempts is how many times a call is tried before pulse gives up. Every
// AWS call pulse makes is a read, so retrying is always safe — and throttling
// is the one failure a user can do nothing about except wait, which the SDK's
// exponential backoff already does better than a person would.
const maxAttempts = 5

// Load resolves credentials without calling AWS. Errors here are about
// local configuration (a profile that doesn't exist, an unreadable file).
func Load(ctx context.Context, o Options) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRetryMaxAttempts(maxAttempts),
	}
	if o.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(o.Profile))
	}
	if o.Region != "" {
		opts = append(opts, config.WithRegion(o.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, Explain(err, o)
	}
	return cfg, nil
}

// Whoami proves the credentials actually work and reports the identity.
// This is pulse's preflight: one cheap, read-only call before anything else.
func Whoami(ctx context.Context, o Options) (*Identity, error) {
	cfg, err := Load(ctx, o)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, &Error{
			Cause: "no region",
			Msg:   "no AWS region configured",
			Fix:   regionFix(o),
		}
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, Explain(err, o)
	}
	id := &Identity{
		Account: aws.ToString(out.Account),
		ARN:     aws.ToString(out.Arn),
		UserID:  aws.ToString(out.UserId),
		Region:  cfg.Region,
		Source:  sourceLabel(o),
	}
	if !usingEnvCredentials(o) {
		id.Profile = profileLabel(o)
	}
	return id, nil
}

// regionFix advises on setting a region the way this caller supplies
// credentials: AWS_REGION for environment auth, the profile otherwise.
func regionFix(o Options) string {
	if usingEnvCredentials(o) {
		return "pass --region (e.g. --region us-east-1) or export AWS_REGION"
	}
	return fmt.Sprintf("pass --region (e.g. --region us-east-1), or set one for the profile: aws configure set region us-east-1 --profile %s", profileLabel(o))
}

// Error is a classified AWS failure: what went wrong, and what to do about
// it. Every path that talks to AWS returns one of these.
type Error struct {
	Cause string // short machine-ish label, useful in tests
	Msg   string // what happened, in plain words
	Fix   string // the command or change that resolves it
	Err   error  // the original, for -v style debugging later
}

func (e *Error) Error() string {
	if e.Fix == "" {
		return e.Msg
	}
	return e.Msg + "\n    fix: " + e.Fix
}

func (e *Error) Unwrap() error { return e.Err }

// Explain turns an SDK error into an actionable one. The SDK's own messages
// are accurate but written for SDK authors ("operation error STS: …
// failed to refresh cached credentials"), which is no help to someone who
// just needs to run `aws sso login`.
func Explain(err error, o Options) error {
	if err == nil {
		return nil
	}
	p := profileLabel(o)
	s := err.Error()

	// A profile named on the command line that doesn't exist locally.
	var missing config.SharedConfigProfileNotExistError
	if errors.As(err, &missing) || strings.Contains(s, "failed to get shared config profile") {
		fix := "run `pulse aws profiles` to see what's configured"
		if names, e := ProfileNames(); e == nil && len(names) > 0 {
			fix = "available profiles: " + strings.Join(names, ", ")
		} else if e == nil {
			fix = fmt.Sprintf("no profiles found in %s — run: aws configure --profile %s", ConfigPath(), p)
		}
		return &Error{Cause: "profile not found", Err: err,
			Msg: fmt.Sprintf("AWS profile %q isn't configured on this machine", p),
			Fix: fix}
	}

	// Expired IAM Identity Center session — by far the most common failure
	// for teams on SSO, and the one with the shortest fix.
	if containsAny(s, "SSOProviderInvalidToken", "sso session has expired", "SSO session has expired", "expired or is invalid") {
		return &Error{Cause: "sso expired", Err: err,
			Msg: fmt.Sprintf("the AWS SSO session for profile %q has expired", p),
			Fix: fmt.Sprintf("aws sso login --profile %s", p)}
	}

	// No usable credentials at all.
	if containsAny(s, "failed to refresh cached credentials", "no EC2 IMDS role found",
		"failed to retrieve credentials", "NoCredentialProviders", "credential provider chain") {
		return &Error{Cause: "no credentials", Err: err,
			Msg: "no working AWS credentials for " + sourceLabel(o),
			Fix: credFix(o)}
	}

	// Credentials exist but AWS rejects them.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ExpiredToken", "ExpiredTokenException", "RequestExpired":
			return &Error{Cause: "expired token", Err: err,
				Msg: "the credentials for " + sourceLabel(o) + " have expired",
				Fix: "refresh them: " + credFix(o)}
		case "InvalidClientTokenId", "UnrecognizedClientException", "SignatureDoesNotMatch":
			return &Error{Cause: "invalid credentials", Err: err,
				Msg: "AWS rejected the credentials for " + sourceLabel(o),
				Fix: credFix(o)}
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
			// Name the exact action. "Not allowed to do this" makes someone
			// guess what to ask their admin for; "not allowed to call
			// lambda:GetFunction" is a request they can paste.
			what := "do this"
			if action := DeniedPermission(err); action != "" {
				what = "call " + action
			}
			return &Error{Cause: "access denied", Err: err,
				Msg: sourceLabel(o) + " is authenticated but not allowed to " + what,
				Fix: "ask for read-only access — `pulse import aws --policy` prints the exact policy to request"}
		case "ThrottlingException", "TooManyRequestsException", "Throttling",
			"RequestLimitExceeded", "SlowDown":
			return &Error{Cause: "throttled", Err: err,
				Msg: "AWS is throttling these requests",
				Fix: fmt.Sprintf("pulse already retried %d times with backoff — wait a moment and run it again", maxAttempts)}
		}
	}

	// Network-shaped failures: DNS, proxy, TLS and timeouts all look alike in
	// the SDK's message and have completely different remedies.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &Error{Cause: "dns", Err: err,
			Msg: "couldn't resolve the AWS endpoint (DNS failure)",
			Fix: "check your network or VPN; if you use a proxy, set HTTPS_PROXY"}
	}

	// A proxy that is configured but unreachable — common on a corporate
	// laptop off the VPN. Name the proxy so it's obvious which one failed.
	if strings.Contains(s, "proxyconnect") {
		fix := "check your network or VPN"
		if p := proxyEnv(); p != "" {
			fix = fmt.Sprintf("%s is set but not reachable — connect to the VPN, or unset it", p)
		}
		return &Error{Cause: "proxy", Err: err,
			Msg: "couldn't reach AWS through the configured proxy", Fix: fix}
	}

	// TLS interception: an inspecting proxy re-signs traffic with a corporate
	// CA the Go runtime doesn't trust. Nothing about the credentials is wrong,
	// which is why this one is so confusing without a name for it.
	var unknownCA x509.UnknownAuthorityError
	var badCert x509.CertificateInvalidError
	var badHost x509.HostnameError
	if errors.As(err, &unknownCA) || errors.As(err, &badCert) || errors.As(err, &badHost) ||
		containsAny(s, "certificate signed by unknown authority", "tls: failed to verify certificate",
			"x509: certificate") {
		return &Error{Cause: "tls", Err: err,
			Msg: "the TLS connection to AWS wasn't trusted — usually a proxy that inspects HTTPS",
			Fix: "point AWS_CA_BUNDLE at your organization's CA certificate (the aws CLI needs the same thing)"}
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "i/o timeout") {
		return &Error{Cause: "timeout", Err: err,
			Msg: fmt.Sprintf("AWS didn't respond within %s", callTimeout),
			Fix: "check your connection (or a corporate proxy blocking sts.amazonaws.com) and try again"}
	}

	return &Error{Cause: "unknown", Err: err,
		Msg: fmt.Sprintf("AWS call failed for %s: %v", sourceLabel(o), err),
		Fix: "run `pulse aws whoami --profile " + p + "` to test connectivity on its own"}
}

// DeniedPermission reports the IAM action an error was refused for, or "" if
// the error isn't a permission problem at all. The SDK wraps every failure in
// an OperationError that knows the service and operation, so the only
// translation needed is into IAM's own naming.
//
// Exported because callers that *tolerate* a denial need this too: the
// importer degrades when an optional read is refused, and "you don't have
// iam:ListRolePolicies" is the only version of that note worth reading.
func DeniedPermission(err error) string {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return ""
	}
	switch api.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
	default:
		return ""
	}
	var op *smithy.OperationError
	if !errors.As(err, &op) {
		return ""
	}
	return IAMAction(op.Service(), op.Operation())
}

// IsNotFound reports whether AWS answered "that thing doesn't exist" — the
// one failure that is usually a typo rather than a problem with the setup, so
// callers can say something specific instead of relaying a 404.
func IsNotFound(err error) bool {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.ErrorCode() {
	case "ResourceNotFoundException", "NoSuchEntity", "NotFoundException",
		"QueueDoesNotExist", "AWS.SimpleQueueService.NonExistentQueue":
		return true
	}
	return false
}

// IAMAction turns an SDK service + operation into the IAM action string a
// policy has to grant — "Lambda"/"GetFunction" → "lambda:GetFunction".
//
// API Gateway is the exception worth encoding: its v2 API is authorized with
// HTTP verbs (apigateway:GET), not operation names, so anyone told they need
// "apigateway:GetApis" would search the IAM reference and come up empty.
func IAMAction(service, operation string) string {
	svc := strings.ToLower(strings.ReplaceAll(service, " ", ""))
	switch svc {
	case "apigatewayv2", "apigateway":
		return "apigateway:GET"
	case "":
		return ""
	}
	if operation == "" {
		return ""
	}
	return svc + ":" + operation
}

// proxyEnv names the proxy variable actually in play, so the fix can point at
// the right one instead of listing all four.
func proxyEnv() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			return k + "=" + v
		}
	}
	return ""
}

// profileLabel names the profile in play, mirroring the SDK's own fallback
// order, so messages never say "" when the user relied on AWS_PROFILE.
func profileLabel(o Options) string {
	if o.Profile != "" {
		return o.Profile
	}
	if p := os.Getenv("AWS_PROFILE"); p != "" {
		return p
	}
	return "default"
}

// usingEnvCredentials reports whether credentials come from the environment
// rather than a profile, so a message never blames a profile that isn't
// even involved.
func usingEnvCredentials(o Options) bool {
	return o.Profile == "" && os.Getenv("AWS_PROFILE") == "" && EnvCredentialSource() != ""
}

// sourceLabel describes where credentials came from, for messages: a
// profile name, or the environment source.
func sourceLabel(o Options) string {
	if usingEnvCredentials(o) {
		return EnvCredentialSource()
	}
	return "profile " + strconv.Quote(profileLabel(o))
}

// credFix is the "how do I fix my credentials" advice, matched to the
// source actually in use.
func credFix(o Options) string {
	if usingEnvCredentials(o) {
		return "check AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY in your environment"
	}
	p := profileLabel(o)
	return fmt.Sprintf("aws configure --profile %s   (or `aws sso login --profile %s` if your org uses SSO)", p, p)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// EnvCredentialSource names the non-profile credential source in play, or
// "" when there is none. Profiles are only one branch of AWS's chain:
// environment variables and instance/task roles are equally valid, and a
// machine with no ~/.aws at all can still be perfectly authenticated.
func EnvCredentialSource() string {
	switch {
	case os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != "":
		return "environment variables (AWS_ACCESS_KEY_ID)"
	case os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" ||
		os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "":
		return "container credentials (ECS/EKS task role)"
	case os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "":
		return "web identity token (IRSA / OIDC)"
	}
	return ""
}

// EnvRegion returns a region set in the environment, which the SDK would
// pick up on its own — so pulse must not ask for one that already exists.
func EnvRegion() string {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r
	}
	return os.Getenv("AWS_DEFAULT_REGION")
}

// HasAWSCLI reports whether the aws CLI is installed, so guidance can point
// at `aws configure` only when that command actually exists.
func HasAWSCLI() bool {
	_, err := exec.LookPath("aws")
	return err == nil
}

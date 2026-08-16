package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
)

// awsFixture points the resolver at throwaway config/credentials files, so
// no test can read the developer's real AWS setup or reach the network.
func awsFixture(t *testing.T, cfg string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", p)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
}

// cmdWith returns a command whose stdin is the scripted answers.
func cmdWith(answers string) (*cobra.Command, *bytes.Buffer) {
	c := &cobra.Command{}
	out := &bytes.Buffer{}
	c.SetIn(strings.NewReader(answers))
	c.SetOut(out)
	return c, out
}

func TestResolveAsksForProfileAndRegion(t *testing.T) {
	awsFixture(t, "[profile alpha]\n[profile beta]\n")
	t.Setenv("PULSE_ASSUME_TTY", "1")

	cmd, out := cmdWith("2\n5\n") // profile beta, then ap-south-1
	got, err := resolveAWSTarget(cmd, "", "")
	if err != nil {
		t.Fatalf("resolveAWSTarget: %v", err)
	}
	if got.Profile != "beta" {
		t.Errorf("profile = %q, want beta", got.Profile)
	}
	if got.Region != "ap-south-1" {
		t.Errorf("region = %q, want ap-south-1", got.Region)
	}
	if !strings.Contains(out.String(), "which aws profile?") {
		t.Error("expected the profile question")
	}
	if !strings.Contains(out.String(), "which region?") {
		t.Error("expected the region question")
	}
}

// A region already declared for the profile must not be asked for.
func TestResolveUsesProfileRegionWithoutAsking(t *testing.T) {
	awsFixture(t, "[profile alpha]\nregion = eu-west-2\n[profile beta]\n")
	t.Setenv("PULSE_ASSUME_TTY", "1")

	cmd, out := cmdWith("1\n")
	got, err := resolveAWSTarget(cmd, "", "")
	if err != nil {
		t.Fatalf("resolveAWSTarget: %v", err)
	}
	if got.Region != "eu-west-2" {
		t.Errorf("region = %q, want eu-west-2 from the profile", got.Region)
	}
	if strings.Contains(out.String(), "which region?") {
		t.Error("should not ask for a region the profile already declares")
	}
}

// One profile is not a choice — don't make the user confirm the obvious.
func TestResolveSkipsPickerForSingleProfile(t *testing.T) {
	awsFixture(t, "[profile only]\nregion = us-east-1\n")
	t.Setenv("PULSE_ASSUME_TTY", "1")

	cmd, out := cmdWith("")
	got, err := resolveAWSTarget(cmd, "", "")
	if err != nil {
		t.Fatalf("resolveAWSTarget: %v", err)
	}
	if got.Profile != "only" || got.Region != "us-east-1" {
		t.Errorf("got %+v, want profile=only region=us-east-1", got)
	}
	if strings.Contains(out.String(), "which aws profile?") {
		t.Error("should not ask when there is only one profile")
	}
}

// A `default` profile is the SDK's own answer; take it silently.
func TestResolvePrefersDefaultProfile(t *testing.T) {
	awsFixture(t, "[default]\nregion = us-east-1\n[profile other]\n")
	t.Setenv("PULSE_ASSUME_TTY", "1")

	cmd, out := cmdWith("")
	got, err := resolveAWSTarget(cmd, "", "")
	if err != nil {
		t.Fatalf("resolveAWSTarget: %v", err)
	}
	if got.Profile != "default" {
		t.Errorf("profile = %q, want default", got.Profile)
	}
	if strings.Contains(out.String(), "which aws profile?") {
		t.Error("a default profile should not trigger a question")
	}
}

// Scripts and CI must get the classified error, never a hanging prompt.
func TestResolveNonInteractiveErrorsInstead(t *testing.T) {
	awsFixture(t, "[profile alpha]\n[profile beta]\n")
	t.Setenv("PULSE_ASSUME_TTY", "") // not a TTY

	cmd, _ := cmdWith("")
	_, err := resolveAWSTarget(cmd, "", "")
	var pe *awscfg.Error
	if !errors.As(err, &pe) || pe.Cause != "profile needed" {
		t.Fatalf("want cause \"profile needed\", got %v", err)
	}
	if !strings.Contains(pe.Fix, "alpha") || !strings.Contains(pe.Fix, "beta") {
		t.Errorf("fix should list the profiles, got %q", pe.Fix)
	}

	// Same for a missing region once the profile is explicit.
	_, err = resolveAWSTarget(cmd, "alpha", "")
	if !errors.As(err, &pe) || pe.Cause != "no region" {
		t.Fatalf("want cause \"no region\", got %v", err)
	}
}

// Explicit flags bypass every question.
func TestResolveFlagsWinOverPrompts(t *testing.T) {
	awsFixture(t, "[profile alpha]\nregion = eu-west-2\n[profile beta]\n")
	t.Setenv("PULSE_ASSUME_TTY", "1")

	cmd, out := cmdWith("")
	got, err := resolveAWSTarget(cmd, "beta", "us-west-1")
	if err != nil {
		t.Fatalf("resolveAWSTarget: %v", err)
	}
	if got.Profile != "beta" || got.Region != "us-west-1" {
		t.Errorf("got %+v, want the flag values", got)
	}
	if out.Len() != 0 {
		t.Errorf("flags should silence every prompt, got output: %q", out.String())
	}
}

// Profiles are only one branch of AWS's credential chain: a CI container
// with env credentials and no ~/.aws is a normal, working setup and must
// not be refused.
func TestResolveAcceptsEnvironmentCredentials(t *testing.T) {
	awsFixture(t, "") // no profiles at all
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_REGION", "eu-central-1")

	cmd, out := cmdWith("")
	got, err := resolveAWSTarget(cmd, "", "")
	if err != nil {
		t.Fatalf("env credentials must be accepted: %v", err)
	}
	if got.Profile != "" {
		t.Errorf("profile = %q, want empty (env credentials, no profile)", got.Profile)
	}
	if got.Region != "eu-central-1" {
		t.Errorf("region = %q, want eu-central-1 from AWS_REGION", got.Region)
	}
	if !strings.Contains(out.String(), "environment variables") {
		t.Errorf("should say where credentials came from, got %q", out.String())
	}
}

// AWS_REGION must be honored even when profiles exist — the SDK would use
// it, so pulse must not ask a question with an answer already available.
func TestResolveNeverAsksWhenAWSRegionIsSet(t *testing.T) {
	awsFixture(t, "[default]\n")
	t.Setenv("AWS_REGION", "ap-northeast-1")
	t.Setenv("PULSE_ASSUME_TTY", "1")

	cmd, out := cmdWith("")
	got, err := resolveAWSTarget(cmd, "", "")
	if err != nil {
		t.Fatalf("resolveAWSTarget: %v", err)
	}
	if got.Region != "ap-northeast-1" {
		t.Errorf("region = %q, want ap-northeast-1", got.Region)
	}
	if strings.Contains(out.String(), "which region?") {
		t.Error("must not ask for a region AWS_REGION already provides")
	}
}

// The genuine empty state: nothing anywhere. The message must not blame a
// profile, and must offer both routes.
func TestResolveTrulyNothingConfigured(t *testing.T) {
	awsFixture(t, "")
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_WEB_IDENTITY_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	cmd, _ := cmdWith("")
	_, err := resolveAWSTarget(cmd, "", "")
	var pe *awscfg.Error
	if !errors.As(err, &pe) || pe.Cause != "no credentials" {
		t.Fatalf("want cause \"no credentials\", got %v", err)
	}
	if strings.Contains(pe.Msg, "profile \"") {
		t.Errorf("must not blame a specific profile, got %q", pe.Msg)
	}
	for _, want := range []string{"no profiles", "environment variables", "instance role"} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("message should mention %q, got %q", want, pe.Msg)
		}
	}
	if !strings.Contains(pe.Fix, "read-only") {
		t.Errorf("fix should reassure about read-only access, got %q", pe.Fix)
	}
}

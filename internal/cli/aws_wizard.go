package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// commonRegions are the ones people actually pick, offered as a shortlist;
// anything else can be typed. Not a validation list — AWS adds regions
// faster than we ship, so a typed value is always accepted.
var commonRegions = []string{
	"us-east-1", "us-west-2", "eu-west-1", "eu-central-1",
	"ap-south-1", "ap-southeast-1", "ap-northeast-1",
}

// resolveAWSTarget turns "which account?" into a definite answer, asking
// when it can and erroring precisely when it can't. Bare commands must ask
// rather than fail — the same rule the rest of the CLI follows.
//
// Non-interactive callers (CI, pipes) fall straight through to the
// classified errors from awscfg, so scripts still get exact diagnostics.
func resolveAWSTarget(cmd *cobra.Command, profile, region string) (awscfg.Options, error) {
	o := awscfg.Options{Profile: profile, Region: region}
	profiles, err := awscfg.Profiles()
	if err != nil {
		return o, err
	}

	in := promptIn(cmd)
	out := cmd.OutOrStdout()

	// A named profile that doesn't exist is a typo, and the SDK only says so
	// several steps later — after pulse has already asked which region to use
	// for a profile that isn't there. Catch it before asking anything.
	if o.Profile != "" && len(profiles) > 0 && !hasProfile(profiles, o.Profile) {
		names := make([]string, len(profiles))
		for i, p := range profiles {
			names[i] = p.Name
		}
		return o, &awscfg.Error{
			Cause: "profile not found",
			Msg:   fmt.Sprintf("AWS profile %q isn't configured on this machine", o.Profile),
			Fix:   clause(suggestion(o.Profile, names), "available profiles: "+strings.Join(names, ", ")),
		}
	}

	// Profiles are only one branch of AWS's credential chain. Environment
	// variables, ECS/EKS task roles and IRSA are equally valid — and a CI
	// container legitimately has no ~/.aws at all. Never refuse before the
	// chain has had its say.
	if len(profiles) == 0 {
		if src := awscfg.EnvCredentialSource(); src != "" {
			fmt.Fprintf(out, "%s\n", ui.Dim("no aws profiles — using "+src))
		} else if os.Getenv("AWS_PROFILE") != "" {
			// AWS_PROFILE names a profile pulse can't see; let the SDK try and
			// report precisely if it fails too.
			fmt.Fprintf(out, "%s\n", ui.Dim("using AWS_PROFILE="+os.Getenv("AWS_PROFILE")))
		} else {
			return o, noCredentialsError()
		}
	} else if o.Profile == "" && awscfg.EnvCredentialSource() == "" {
		p, err := chooseProfile(in, out, profiles)
		if err != nil {
			return o, err
		}
		o.Profile = p
	}

	// Region: an explicit flag, then the environment (which the SDK would
	// use anyway), then the chosen profile, and only then a question.
	if o.Region == "" {
		o.Region = awscfg.EnvRegion()
	}
	if o.Region == "" {
		for _, p := range profiles {
			if p.Name == o.Profile && p.Region != "" {
				o.Region = p.Region
				break
			}
		}
	}
	if o.Region == "" {
		r, err := chooseRegion(in, out, o.Profile)
		if err != nil {
			return o, err
		}
		o.Region = r
	}
	return o, nil
}

// hasProfile reports whether the named profile was found on this machine.
func hasProfile(profiles []awscfg.Profile, name string) bool {
	for _, p := range profiles {
		if p.Name == name {
			return true
		}
	}
	return false
}

// noCredentialsError is the genuine empty state: no profiles, no
// environment credentials, no role. The guidance adapts to whether the aws
// CLI even exists — suggesting `aws configure` to someone without the CLI
// is a dead end.
func noCredentialsError() error {
	fix := "set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or create a profile with the AWS CLI"
	if awscfg.HasAWSCLI() {
		fix = "run `aws configure --profile dev` (or `aws sso login` if your org uses IAM Identity Center)"
	}
	return &awscfg.Error{
		Cause: "no credentials",
		Msg:   "no AWS credentials found — no profiles, no environment variables, no instance role",
		Fix:   fix + ". pulse only ever needs read-only access.",
	}
}

// chooseProfile skips the question when there is nothing to choose: one
// profile means one answer, and AWS_PROFILE is already the user's stated
// preference.
func chooseProfile(in *bufio.Reader, out io.Writer, profiles []awscfg.Profile) (string, error) {
	if len(profiles) == 1 {
		fmt.Fprintf(out, "%s\n", ui.Dim("using the only aws profile: "+profiles[0].Name))
		return profiles[0].Name, nil
	}
	for _, p := range profiles {
		if p.Name == "default" {
			return "default", nil // the SDK's own choice; don't second-guess it
		}
	}
	if !stdinIsInteractive() {
		names := make([]string, len(profiles))
		for i, p := range profiles {
			names[i] = p.Name
		}
		return "", &awscfg.Error{
			Cause: "profile needed",
			Msg:   "no AWS profile chosen and no `default` profile exists",
			Fix:   "pass --profile with one of: " + strings.Join(names, ", "),
		}
	}

	opts := make([]pickOption, len(profiles))
	for i, p := range profiles {
		var notes []string
		if p.Region != "" {
			notes = append(notes, p.Region)
		}
		if p.SSO {
			notes = append(notes, "sso")
		}
		if p.AssumeRor {
			notes = append(notes, "assumes role")
		}
		opts[i] = pickOption{label: p.Name, desc: strings.Join(notes, " · ")}
	}
	idx, err := askPick(in, out, "which aws profile?", opts, 1)
	if err != nil {
		return "", err
	}
	return profiles[idx].Name, nil
}

// chooseRegion offers the shortlist and accepts anything typed — pulse
// should never reject a region just because it shipped before AWS added it.
func chooseRegion(in *bufio.Reader, out io.Writer, profile string) (string, error) {
	// With environment credentials there is no profile to blame or to fix,
	// so the advice has to name AWS_REGION instead.
	where, fix := fmt.Sprintf("for profile %q", profile),
		fmt.Sprintf("pass --region (e.g. --region us-east-1), or set one: aws configure set region us-east-1 --profile %s", profile)
	if profile == "" {
		where, fix = "", "pass --region (e.g. --region us-east-1) or export AWS_REGION"
	}
	if !stdinIsInteractive() {
		return "", &awscfg.Error{
			Cause: "no region",
			Msg:   strings.TrimSpace("no AWS region configured " + where),
			Fix:   fix,
		}
	}
	opts := make([]pickOption, 0, len(commonRegions)+1)
	for _, r := range commonRegions {
		opts = append(opts, pickOption{label: r})
	}
	opts = append(opts, pickOption{label: "other…", desc: "type any region"})

	question := "which region?"
	if profile != "" {
		question = fmt.Sprintf("which region? (profile %s declares none)", profile)
	}
	idx, err := askPick(in, out, question, opts, 1)
	if err != nil {
		return "", err
	}
	if idx < len(commonRegions) {
		return commonRegions[idx], nil
	}
	return askText(in, out, "region", "", func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("a region is required, e.g. eu-west-2")
		}
		return nil
	})
}

// regionHint nudges toward making a prompted region permanent — printed
// only after a successful call, and never by writing to ~/.aws ourselves.
func regionHint(out io.Writer, o awscfg.Options, prompted bool) {
	if prompted {
		fmt.Fprintf(out, "%s\n", ui.Hint(fmt.Sprintf(
			"make it stick: `aws configure set region %s --profile %s`", o.Region, o.Profile)))
	}
}

// declaresRegion reports whether the profile itself sets a region, so the
// "make it stick" hint only appears when there is nothing set yet.
func declaresRegion(profile string) bool {
	profiles, err := awscfg.Profiles()
	if err != nil {
		return false
	}
	for _, p := range profiles {
		if p.Name == profile {
			return p.Region != ""
		}
	}
	return false
}

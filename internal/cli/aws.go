package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// awsProfile and awsRegion are shared by every AWS-touching command so the
// flags mean the same thing everywhere.
var (
	awsProfile string
	awsRegion  string
)

var awsCmd = &cobra.Command{
	Use:   "aws",
	Short: "Talk to a real AWS account (read-only)",
	Long: `Commands that read from a real AWS account.

pulse uses the same credentials the aws CLI does — the profiles in
~/.aws/config and ~/.aws/credentials, AWS_PROFILE, AWS_REGION and the rest
of the standard chain. Nothing here writes to AWS.`,
	RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var awsProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List the AWS profiles pulse can see",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		profiles, err := awscfg.Profiles()
		if err != nil {
			return err
		}
		if len(profiles) == 0 {
			fmt.Println(ui.Warn("no AWS profiles found"))
			fmt.Printf("  %s\n", ui.Dim("looked in "+awscfg.ConfigPath()))
			fmt.Printf("  %s\n", ui.Hint("create one: `aws configure --profile dev`"))
			return nil
		}

		fmt.Printf("%s %s\n\n", ui.AccentBold("⚡ aws profiles"),
			ui.Dim(fmt.Sprintf("— %d found in %s", len(profiles), awscfg.ConfigPath())))
		for _, p := range profiles {
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
			notes = append(notes, p.Source)
			fmt.Printf("  %-24s %s\n", ui.Bold(p.Name), ui.Dim(strings.Join(notes, " · ")))
		}
		fmt.Printf("\n%s\n", ui.Hint("check one works: `pulse aws whoami --profile <name>`"))
		return nil
	},
}

var awsWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show which AWS account and identity pulse would read",
	Long: `Confirms credentials work and prints the account you are pointed at.

One read-only sts:GetCallerIdentity call — the same preflight pulse runs
before importing anything, so you always know whose account is in play.`,
	Args: cobra.NoArgs,
	Example: `  pulse aws whoami
  pulse aws whoami --profile prod --region eu-west-1`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// Ask rather than fail: a bare `pulse aws whoami` walks the user to an
		// answer, while scripts still get the classified errors.
		opts, err := resolveAWSTarget(cmd, awsProfile, awsRegion)
		if err != nil {
			return err
		}
		promptedRegion := awsRegion == "" && opts.Region != ""

		id, err := awscfg.Whoami(cmd.Context(), opts)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n\n", ui.AccentBold("⚡ aws identity"))
		// Never label environment credentials as a profile: someone told to
		// fix "profile default" would go editing a file that isn't in play.
		if id.Profile != "" {
			fmt.Printf("  %s  %s\n", ui.Dim("profile"), ui.Bold(id.Profile))
		} else {
			fmt.Printf("  %s  %s\n", ui.Dim("from   "), ui.Bold(id.Source))
		}
		fmt.Printf("  %s  %s\n", ui.Dim("account"), ui.Bold(id.Account))
		fmt.Printf("  %s   %s\n", ui.Dim("region"), ui.Bold(id.Region))
		fmt.Printf("  %s      %s\n", ui.Dim("arn"), ui.Dim(id.ARN))
		fmt.Printf("\n%s\n", ui.OK("✓ credentials work — read-only access confirmed"))
		regionHint(cmd.OutOrStdout(), opts, promptedRegion && !declaresRegion(opts.Profile))
		return nil
	},
}

// addAWSFlags gives every AWS-touching command the same two flags, with
// Tab completion over the caller's actual profiles — pulse's completion
// always knows your setup, not a generic list.
func addAWSFlags(c *cobra.Command) {
	c.Flags().StringVar(&awsProfile, "profile", "", "AWS profile to use (default: AWS_PROFILE, then \"default\")")
	c.Flags().StringVar(&awsRegion, "region", "", "AWS region to read from (default: the profile's region)")
	_ = c.RegisterFlagCompletionFunc("profile",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			names, err := awscfg.ProfileNames()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		})
}

func init() {
	addAWSFlags(awsWhoamiCmd)
	awsProfilesCmd.ValidArgsFunction = cobra.NoFileCompletions
	awsWhoamiCmd.ValidArgsFunction = cobra.NoFileCompletions
	awsCmd.AddCommand(awsProfilesCmd, awsWhoamiCmd)
}

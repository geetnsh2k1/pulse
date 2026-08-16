package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
	"github.com/geetnsh2k1/pulse/internal/importer"
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
	addAWSFlags(awsLayersCmd)
	awsLayersCmd.ValidArgsFunction = cobra.NoFileCompletions
	awsProfilesCmd.ValidArgsFunction = cobra.NoFileCompletions
	awsWhoamiCmd.ValidArgsFunction = cobra.NoFileCompletions
	awsCmd.AddCommand(awsProfilesCmd, awsWhoamiCmd, awsLayersCmd)
}

var awsLayersCmd = &cobra.Command{
	Use:   "layers",
	Short: "Download the Lambda layers this project's functions need",
	Long: `Fetches the layers recorded in pulse.yaml and unpacks them locally.

Layer contents are gitignored — they're vendored bytes, not your source — so
a fresh clone of an imported project has the ARNs but not the packages, and
its functions fail on their first import. This downloads them again.

Read-only: lambda:GetLayerVersion plus the presigned download it hands back.
Nothing in your AWS account is created, changed or deleted.

The region comes from each layer's own ARN, so layers shared from another
region resolve correctly without a --region flag.`,
	Args:    cobra.NoArgs,
	Example: `  pulse aws layers\n  pulse aws layers --profile prod`,
	RunE:    runAWSLayers,
}

func runAWSLayers(cmd *cobra.Command, _ []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	// Re-fetching is idempotent and cheap to reason about, so this deliberately
	// does not skip functions whose layers are already present: "my layer is
	// stale" is a real state, and a command that refuses to act on it is worse
	// than one that spends a few seconds.
	var work []missingLayers
	for _, name := range sortedFunctionNames(cfg) {
		fn := cfg.Functions[name]
		if len(fn.Layers) > 0 {
			work = append(work, missingLayers{Function: name, CodeDir: fn.CodeDir, ARNs: fn.Layers})
		}
	}
	if len(work) == 0 {
		fmt.Printf("%s\n", ui.OK("✓ no function in this project declares any layers"))
		return nil
	}

	opts, err := resolveAWSTarget(cmd, awsProfile, awsRegion)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	id, err := awscfg.Whoami(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s\n", ui.AccentBold("⚡ pulse aws layers"), ui.Dim("— read-only"))
	fmt.Printf("  %s  %s\n\n", ui.Dim("account"), ui.Bold(id.Account))

	awsCfg, err := awscfg.Load(ctx, opts)
	if err != nil {
		return err
	}
	disco := importer.NewDiscoverer(awsCfg, id.Region)

	var fetched, failed int
	for _, w := range work {
		layers := make([]importer.Layer, 0, len(w.ARNs))
		for _, arn := range w.ARNs {
			layers = append(layers, importer.Layer{ARN: arn, Name: importer.LayerName(arn)})
		}

		sp := ui.StartSpinner(fmt.Sprintf("resolving %s for %s", layerWord(len(layers)), w.Function))
		layers = disco.ResolveLayers(ctx, layers)
		sp.Success()

		dest := filepath.Join(cfg.Root, w.CodeDir, importer.LayerDir)
		for _, l := range layers {
			if l.Unreadable != "" {
				fmt.Printf("  %s %s — %s\n", ui.Warn("✱"), ui.Bold(l.Name), ui.Dim(l.Unreadable))
				failed++
				continue
			}
			sp := ui.StartSpinner(fmt.Sprintf("downloading %s (%s)", l.Name, humanSize(l.CodeSize)))
			written, err := importer.FetchLayers(ctx, nil, []importer.Layer{l}, dest)
			if err != nil {
				sp.Fail("couldn't download " + l.Name)
				return err
			}
			sp.Success()
			fmt.Printf("  %s %s → %s %s\n", ui.OK("✓"), ui.Bold(l.Name),
				ui.Dim(filepath.Join(w.CodeDir, importer.LayerDir)),
				ui.Dim(fmt.Sprintf("(%d files)", len(written))))
			fetched++
		}
	}

	fmt.Println()
	if failed > 0 {
		fmt.Printf("%s\n", ui.Warn(fmt.Sprintf("✱ %d layer(s) unreadable — the functions using them will still fail", failed)))
		fmt.Printf("  %s\n", ui.Hint("see what access is needed: `pulse import aws --policy`"))
		return nil
	}
	fmt.Printf("%s\n", ui.OK(fmt.Sprintf("✓ %d layer(s) ready — `pulse start`", fetched)))
	return nil
}

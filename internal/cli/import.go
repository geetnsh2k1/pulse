package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/awscfg"
	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/importer"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

var (
	flagImportFunction string
	flagImportName     string
	flagImportDryRun   bool
	flagImportYes      bool
	flagImportValues   bool
	flagImportPolicy   bool
)

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Bring an existing cloud function into a local pulse project",
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var importAWSCmd = &cobra.Command{
	Use:   "aws [function]",
	Short: "Import a deployed Lambda function into a new local project",
	Long: `Turn a deployed Lambda into a pulse project you can run on your laptop.

pulse reads the function's configuration, its API Gateway routes and SQS
triggers, the queues and tables it appears to use, and its environment
variable names — then writes a new project with your real handler code.

Read-only, always: import calls only List*/Get*/Describe*. Nothing in your
AWS account is created, changed or deleted, so it cannot affect production.

Environment values are NOT copied by default (Lambda variables routinely
hold live API keys) — every value is written to .env as CHANGE_ME. Pass
--with-values when you actually want the real ones on your disk.

Anything pulse can't represent — layers, VPC config, S3/SNS/EventBridge
triggers, secondary indexes — is printed and written to IMPORT-NOTES.md
beside the project. Nothing is dropped silently.`,
	Args: cobra.MaximumNArgs(1),
	Example: `  pulse import aws                          pick a profile, region and function
  pulse import aws createOrder              import that function
  pulse import aws createOrder --dry-run    show the plan, write nothing
  pulse import aws createOrder --profile prod --region eu-west-1 --yes
  pulse import aws --policy                 print the read-only IAM policy it needs`,
	ValidArgsFunction: cobra.NoFileCompletions,
	RunE:              runImportAWS,
}

// addImportFlags is shared with the tests, so a flag can never exist on the
// real command but be missing from the one a test drives.
func addImportFlags(c *cobra.Command) {
	addAWSFlags(c)
	f := c.Flags()
	f.StringVar(&flagImportFunction, "function", "", "Lambda function to import (same as the positional argument)")
	f.StringVar(&flagImportName, "name", "", "project name and directory to create (default: the function's name)")
	f.BoolVar(&flagImportDryRun, "dry-run", false, "show the plan and the pulse.yaml it would write, then stop")
	f.BoolVar(&flagImportYes, "yes", false, "skip prompts: take the pre-checked defaults (for scripts and CI)")
	f.BoolVar(&flagImportValues, "with-values", false, "copy real environment values into .env (they may be secrets)")
	f.BoolVar(&flagImportPolicy, "policy", false, "print the minimal read-only IAM policy import needs, then exit")
}

func init() {
	addImportFlags(importAWSCmd)
	importCmd.AddCommand(importAWSCmd)
}

func runImportAWS(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	in := promptIn(cmd)
	out := cmd.OutOrStdout()

	// --policy is a document, not a call. It runs before anything touches
	// credentials on purpose: the moment you need it most is when AccessDenied
	// just stopped you, or when you're asking an admin for access you don't
	// have yet.
	if flagImportPolicy {
		printReadPolicy(out)
		return nil
	}

	fnName := flagImportFunction
	if len(args) == 1 {
		if fnName != "" && fnName != args[0] {
			return fmt.Errorf("two different functions given: %q and %q — pass just one", args[0], fnName)
		}
		fnName = args[0]
	}

	// An import creates a new project, so running it inside one would be
	// ambiguous at best and destructive at worst (PLAN §12.9).
	if err := notInsideAProject(); err != nil {
		return err
	}

	// Whose account? Asked before anything is read, answered on screen.
	opts, err := resolveAWSTarget(cmd, awsProfile, awsRegion)
	if err != nil {
		return err
	}
	id, err := awscfg.Whoami(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s %s\n", ui.AccentBold("⚡ pulse import aws"), ui.Dim("— read-only"))
	fmt.Fprintf(out, "  %s  %s\n", ui.Dim("account"), ui.Bold(id.Account))
	fmt.Fprintf(out, "  %s   %s\n", ui.Dim("region"), ui.Bold(id.Region))
	if id.Profile != "" {
		fmt.Fprintf(out, "  %s  %s\n", ui.Dim("profile"), ui.Bold(id.Profile))
	} else {
		fmt.Fprintf(out, "  %s     %s\n", ui.Dim("from"), ui.Bold(id.Source))
	}

	awsCfg, err := awscfg.Load(ctx, opts)
	if err != nil {
		return err
	}
	disco := importer.NewDiscoverer(awsCfg, id.Region)

	if fnName == "" {
		sp := ui.StartSpinner("listing functions in " + id.Region)
		list, err := disco.ListFunctions(ctx)
		if err != nil {
			sp.Fail("couldn't list functions")
			return awscfg.Explain(err, opts)
		}
		sp.Success()
		if fnName, err = pickImportFunction(in, out, list, id.Region); err != nil {
			return err
		}
	}

	sp := ui.StartSpinner("reading " + fnName)
	d, err := disco.Discover(ctx, fnName)
	if err != nil {
		sp.Fail("couldn't read the function")
		return explainMissingFunction(ctx, disco, opts, fnName, id.Region, err)
	}
	sp.Success()

	// Refuse before downloading: a container-image or oversized function is
	// a refusal no matter what, and shouldn't cost a 200 MB transfer first.
	if err := d.Importable(); err != nil {
		return err
	}
	reportGaps(out, disco.DegradedNotes())

	// Fetch the code now, while the user is still watching, for two reasons:
	// the scan below sharpens the resource guesses, and a presigned URL can't
	// expire during the questions that follow.
	var packages []*importer.CodePackage
	if d.Function.CodeURL != "" {
		sp = ui.StartSpinner(fmt.Sprintf("downloading code (%s)", humanSize(d.Function.CodeSize)))
		pkg, err := importer.FetchCode(ctx, &http.Client{Timeout: 5 * time.Minute}, importer.LocalName(fnName), d.Function.CodeURL)
		if err != nil {
			sp.Fail("download failed")
			return err
		}
		sp.Success()
		defer pkg.Close()
		packages = append(packages, pkg)
		d.CodeText = pkg.SourceText()
	} else {
		fmt.Fprintf(out, "  %s\n", ui.Warn("✱ ")+ui.Hint("AWS didn't return a code location — the project will be scaffolded without your handler"))
	}

	plan, err := importer.BuildPlan(*d, flagImportName)
	if err != nil {
		return err
	}

	// Facts are in. Now the inferred resources: proposed with evidence,
	// confirmed by the user, then described exactly so the local project
	// mirrors production field-for-field rather than by guesswork.
	picked, err := confirmGuesses(in, out, plan.Guesses, flagImportYes)
	if err != nil {
		return err
	}
	describePicked(ctx, disco, plan, picked)

	dest := importDest(plan.Project)
	previewPlan(out, plan, dest)

	if flagImportDryRun {
		fmt.Fprintf(out, "\n%s\n\n", ui.Bold("pulse.yaml it would write:"))
		for _, line := range strings.Split(strings.TrimRight(plan.ConfigYAML(), "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", ui.Dim(line))
		}
		fmt.Fprintf(out, "\n%s\n", ui.OK("✓ dry run — nothing was written"))
		fmt.Fprintf(out, "%s\n", ui.Hint("run it for real: `pulse import aws "+fnName+"`"))
		return nil
	}

	if !flagImportYes && stdinIsInteractive() {
		ok, err := askYesNo(in, out, "create "+dest+"?", true)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(out, "\n%s\n", ui.Dim("nothing was written"))
			return nil
		}
	}
	if flagImportValues {
		fmt.Fprintf(out, "\n%s\n", ui.Warn("✱ ")+ui.Hint("--with-values: real environment values are being written to .env — it is gitignored, keep it that way"))
	}

	// Gaps were printed when they happened; they belong in the project's notes
	// too, so a permission hole outlives the terminal session.
	plan.Warnings = append(plan.Warnings, disco.DegradedNotes()...)

	sp = ui.StartSpinner("writing " + dest)
	written, err := plan.Write(ctx, importer.WriteOptions{
		Dest:       dest,
		WithValues: flagImportValues,
		Profile:    orSource(id),
		Account:    id.Account,
		Code:       packages,
	})
	if err != nil {
		sp.Fail("import failed")
		return err
	}
	sp.Success()

	fmt.Fprintf(out, "\n%s imported %s %s\n", ui.OK("✓"), ui.Bold(plan.Project),
		ui.Dim(fmt.Sprintf("— %s · %d files", plan.Summary(), len(written.Files))))
	printImportNextSteps(out, plan, dest, written)
	return nil
}

// explainMissingFunction turns the likeliest mistake — a typo, or the right
// name in the wrong region — into a useful answer. The generic taxonomy can't:
// it would relay "https response error StatusCode: 404" and advise checking
// connectivity, which is fine and not the problem.
//
// One extra ListFunctions buys a "did you mean" and the real list. It's the
// error path, the user is already stuck, and if that read is denied too the
// message simply stays shorter.
func explainMissingFunction(ctx context.Context, disco *importer.Discoverer,
	opts awscfg.Options, fnName, region string, err error) error {

	if !awscfg.IsNotFound(err) {
		return awscfg.Explain(err, opts)
	}
	fix := "run `pulse import aws` with no name to pick from the list, or try another --region"
	if list, lerr := disco.ListFunctions(ctx); lerr == nil && len(list) > 0 {
		names := make([]string, 0, len(list))
		for _, f := range list {
			names = append(names, f.Name)
		}
		if guess := suggestion(fnName, names); guess != "" {
			fix = clause(guess, "or run `pulse import aws` to pick from the list")
		} else if len(names) <= 8 {
			fix = "functions in " + region + ": " + strings.Join(names, ", ")
		}
	}
	return &awscfg.Error{
		Cause: "no such function",
		Err:   err,
		Msg:   fmt.Sprintf("no Lambda function named %q in %s", fnName, region),
		Fix:   fix,
	}
}

// printReadPolicy prints the permissions import needs, in the two forms
// people actually need them: prose to justify the request, and a JSON document
// to paste into IAM. Every line of it is a read.
//
// Redirected to a file or a pipe it prints the document alone, so
// `pulse import aws --policy > policy.json` is a usable file rather than a
// screenshot of one.
func printReadPolicy(out io.Writer) {
	if !stdoutIsTerminal() {
		fmt.Fprintln(out, importer.MinimalPolicyJSON())
		return
	}

	fmt.Fprintf(out, "\n%s %s\n", ui.AccentBold("⚡ read-only policy for `pulse import aws`"),
		ui.Dim("— nothing here can change your account"))

	fmt.Fprintf(out, "\n  %s\n", ui.Bold("what it reads, and why"))
	for _, a := range importer.ReadActions() {
		note := a.Why
		if a.Optional {
			note += " (optional — import degrades without it)"
		}
		fmt.Fprintf(out, "    %-32s %s\n", ui.Accent(a.Action), ui.Dim(note))
	}

	fmt.Fprintf(out, "\n  %s\n\n", ui.Bold("attach this to the identity you import with"))
	for _, line := range strings.Split(importer.MinimalPolicyJSON(), "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}

	fmt.Fprintf(out, "\n  %s\n", ui.Dim("Resource is \"*\" because List* actions have no resource-level scoping in IAM."))
	fmt.Fprintf(out, "  %s\n", ui.Dim("Your function's code downloads over a presigned link — no extra S3 permission needed."))
	fmt.Fprintf(out, "\n%s\n", ui.Hint("check it worked: `pulse aws whoami` then `pulse import aws`"))
}

// notInsideAProject keeps `pulse import` from creating a project inside a
// project. v1 always creates a new directory; merging into an existing one is
// deliberately not offered, because the safe version of that is a diff, not a
// y/N prompt over someone's handler code.
func notInsideAProject() error {
	path, err := config.Find(workDir())
	if err != nil {
		return nil // no project here — the normal case
	}
	return fmt.Errorf("this is already a pulse project (%s)\n"+
		"    fix: run `pulse import aws` from a directory outside it — import always creates a new project", path)
}

// describePicked turns confirmed guesses into real definitions. Reading the
// actual key schema and queue attributes is what makes a picked resource
// identical to production instead of a plausible-looking approximation.
func describePicked(ctx context.Context, disco *importer.Discoverer, plan *importer.Plan, picked []importer.Guess) {
	if len(picked) == 0 {
		return
	}
	sp := ui.StartSpinner(fmt.Sprintf("describing %d resource(s)", len(picked)))
	var failed []string
	for _, g := range picked {
		switch g.Kind {
		case "table":
			t, err := disco.DescribeTable(ctx, g.Name)
			if err != nil {
				failed = append(failed, "table "+g.Name)
				continue
			}
			plan.AddTable(t, importer.Picked, g.Signals)
		case "queue":
			q, err := disco.DescribeQueue(ctx, g.Name)
			if err != nil {
				failed = append(failed, "queue "+g.Name)
				continue
			}
			plan.AddQueue(q, importer.Picked, g.Signals)
		}
	}
	sp.Success()
	// A description that failed is a gap, not a failure: say so and carry on.
	for _, f := range failed {
		plan.Warnings = append(plan.Warnings, importer.Note{
			Subject: f,
			Detail:  "couldn't be described — skipped; add it with `pulse add` once you know its keys",
		})
	}
}

// reportGaps prints what permissions or APIs refused to answer. An import
// with holes in it is fine; an import with silent holes is not.
func reportGaps(out io.Writer, notes []importer.Note) {
	for _, n := range notes {
		fmt.Fprintf(out, "  %s %s %s\n", ui.Warn("✱"), n.Subject, ui.Dim("— "+n.Detail))
	}
}

// importDest is where the project lands: a new directory beside the caller,
// named after the project.
func importDest(project string) string {
	dir := project
	if flagChdir != "" {
		dir = filepath.Join(flagChdir, dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if rel, err := filepath.Rel(mustWD(), abs); err == nil && !strings.HasPrefix(rel, "..") {
			return "." + string(filepath.Separator) + rel
		}
		return abs
	}
	return dir
}

// orSource is what IMPORT-NOTES.md records as the origin: the profile when
// there is one, otherwise the real credential source.
func orSource(id *awscfg.Identity) string {
	if id.Profile != "" {
		return id.Profile
	}
	return id.Source
}

func mustWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func printImportNextSteps(out io.Writer, plan *importer.Plan, dest string, written *importer.Written) {
	fmt.Fprintf(out, "\n%s\n", ui.AccentBold("next steps"))
	fmt.Fprintf(out, "  %s\n", ui.Accent("cd "+strings.TrimPrefix(dest, "."+string(filepath.Separator))))
	if !flagImportValues && countEnv(plan) > 0 {
		fmt.Fprintf(out, "  %s %s\n", ui.Accent("edit .env"),
			ui.Dim(fmt.Sprintf("— %d value(s) are CHANGE_ME", countEnv(plan))))
	}
	if cmd, why := dependencyStep(plan, written); cmd != "" {
		fmt.Fprintf(out, "  %s %s\n", ui.Accent(cmd), ui.Dim("— "+why))
	}
	fmt.Fprintf(out, "  %s\n", ui.Accent("pulse start"))
	fmt.Fprintf(out, "\n%s\n", ui.Hint("read IMPORT-NOTES.md first — it lists what AWS has that pulse doesn't"))
}

// dependencyStep reads what actually landed on disk rather than guessing from
// the runtime. A Lambda zip usually vendors its dependencies, in which case
// there is nothing to install and saying otherwise would be noise; a bundle
// that only carries a manifest needs one precise command.
func dependencyStep(_ *importer.Plan, written *importer.Written) (cmd, why string) {
	if written == nil {
		return "", ""
	}
	vendored := false
	manifests := map[string]string{}
	for _, f := range written.Files {
		slash := filepath.ToSlash(f)
		switch {
		case strings.Contains(slash, "/node_modules/"), strings.Contains(slash, "-packages/"):
			vendored = true
		case strings.HasSuffix(slash, "/package.json"):
			manifests["node"] = strings.TrimSuffix(slash, "/package.json")
		case strings.HasSuffix(slash, "/requirements.txt"):
			manifests["python"] = strings.TrimSuffix(slash, "/requirements.txt")
		}
	}
	if vendored {
		return "", "" // the package brought its own dependencies
	}
	if dir, ok := manifests["node"]; ok {
		return "npm install --prefix " + dir, "the package ships only a manifest"
	}
	if dir, ok := manifests["python"]; ok {
		// The runner prefers the project's own .venv, so create that one.
		return fmt.Sprintf("python3 -m venv .venv && .venv/bin/pip install -r %s/requirements.txt", dir),
			"pulse runs python functions from the project's .venv"
	}
	// No manifest and nothing vendored: the dependencies lived in a layer, and
	// the plan's caveats already say so.
	return "", ""
}

func countEnv(plan *importer.Plan) int {
	n := 0
	for _, f := range plan.Functions {
		for _, k := range f.EnvNames {
			if !config.ReservedEnvKeys[k] {
				n++
			}
		}
	}
	return n
}

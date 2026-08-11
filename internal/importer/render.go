package importer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// placeholder is what an imported environment value becomes by default.
// Lambda env vars routinely hold live API keys; copying them onto disk
// because someone ran an import would be a nasty surprise, so values are
// opt-in (--with-values) and this marker makes the gap obvious.
//
// It lives in config because the runtime recognizes it too: a table named
// CHANGE_ME produces "your .env isn't filled in", not "declare this table".
const placeholder = config.Placeholder

// ToConfig turns a plan into a real *config.Config. Building the same
// structure pulse.yaml parses into means the plan can be run through the
// project validator before a single file is written — an import that
// wouldn't load is caught here rather than by the user.
//
// Env values never travel: names go to pulse.yaml only when the value is
// non-secret by construction (there is no such case yet, so they all go to
// .env), and DotEnv carries what the caller chose to keep.
func (p *Plan) ToConfig() *config.Config {
	cfg := &config.Config{
		Project:   p.Project,
		Region:    p.Region,
		Functions: map[string]*config.Function{},
		Resources: config.Resources{
			Tables: map[string]*config.Table{},
			Queues: map[string]*config.Queue{},
		},
	}

	for _, f := range p.Functions {
		cfg.Functions[f.Name] = &config.Function{
			Name:    f.Name,
			Runtime: f.Runtime,
			Handler: f.Handler,
			CodeDir: f.CodeDir,
			Timeout: f.TimeoutSec,
			Memory:  f.MemoryMB,
		}
	}

	for _, t := range p.Triggers {
		tr := &config.Trigger{Type: t.Kind, Function: t.Function}
		switch t.Kind {
		case "http":
			tr.Method, tr.Path = t.Method, t.Path
			// Only record a non-default payload format; pulse defaults to 2.0
			// and an explicit "2.0" everywhere is noise in the file.
			if t.PayloadFormat == "1.0" {
				tr.PayloadFormat = "1.0"
			}
		case "sqs":
			tr.Queue, tr.BatchSize = t.Queue, t.BatchSize
		}
		cfg.Triggers = append(cfg.Triggers, tr)
	}

	for _, t := range p.Tables {
		table := &config.Table{
			Name: t.Name,
			PK:   config.KeyDef{Name: t.PK.Name, Type: orS(t.PK.Type)},
		}
		if t.SK != nil {
			table.SK = &config.KeyDef{Name: t.SK.Name, Type: orS(t.SK.Type)}
		}
		cfg.Resources.Tables[t.Name] = table
	}

	for _, q := range p.Queues {
		cfg.Resources.Queues[q.Name] = &config.Queue{
			Name:              q.Name,
			DLQ:               q.DLQ,
			MaxReceiveCount:   q.MaxReceiveCount,
			VisibilityTimeout: q.VisibilityTimeout,
		}
	}
	return cfg
}

// DotEnvLines renders the .env body for the imported function. withValues
// decides whether real values travel; either way the file documents what the
// function expects, so nothing is silently missing at runtime.
func (p *Plan) DotEnvLines(withValues bool) string {
	var b strings.Builder
	b.WriteString("# Imported from AWS by `pulse import aws`.\n")
	if withValues {
		b.WriteString("# Values came from the live function — treat this file as secret.\n")
	} else {
		b.WriteString("# Values were NOT imported: each is " + placeholder + " until you fill it in.\n")
		b.WriteString("# Re-run with --with-values to pull the real ones from AWS.\n")
	}
	b.WriteString("\n")

	for _, f := range p.Functions {
		if len(f.EnvNames) == 0 {
			continue
		}
		if len(p.Functions) > 1 {
			fmt.Fprintf(&b, "# %s\n", f.Name)
		}
		for _, k := range f.EnvNames {
			if config.ReservedEnvKeys[k] {
				continue // pulse sets these; AWS wouldn't allow them either
			}
			v := placeholder
			if withValues {
				v = f.EnvValues[k]
			}
			fmt.Fprintf(&b, "%s=%s\n", k, quoteIfNeeded(v))
		}
	}
	return b.String()
}

// DotEnvExampleLines is the committed twin: names only, never values, so a
// teammate cloning the repo knows exactly what to set.
func (p *Plan) DotEnvExampleLines() string {
	var b strings.Builder
	b.WriteString("# Variables this project expects. Copy to .env and fill in:\n")
	b.WriteString("#   cp .env.example .env\n\n")
	for _, f := range p.Functions {
		for _, k := range f.EnvNames {
			if config.ReservedEnvKeys[k] {
				continue
			}
			fmt.Fprintf(&b, "%s=\n", k)
		}
	}
	return b.String()
}

// Notes renders IMPORT-NOTES.md: the honest record of what pulse could not
// carry across, committed with the project so the gap outlives the terminal
// session that printed it.
func (p *Plan) Notes(profile, account string) string {
	var b strings.Builder
	b.WriteString("# Import notes\n\n")
	fmt.Fprintf(&b, "Imported by `pulse import aws` from account %s (%s), region %s.\n\n",
		orUnknown(account), orUnknown(profile), p.Region)

	b.WriteString("## What came across\n\n")
	for _, f := range p.Functions {
		fmt.Fprintf(&b, "- function `%s` — %s, handler `%s`\n", f.Name, f.Runtime, f.Handler)
	}
	for _, t := range p.Triggers {
		if t.Kind == "http" {
			fmt.Fprintf(&b, "- route `%s %s` → `%s`\n", t.Method, t.Path, t.Function)
		} else {
			fmt.Fprintf(&b, "- queue `%s` → `%s` (batch %d)\n", t.Queue, t.Function, t.BatchSize)
		}
	}
	for _, t := range p.Tables {
		fmt.Fprintf(&b, "- table `%s` (pk `%s`%s) — %s\n", t.Name, t.PK.Name, skSuffix(t.SK), provenanceWord(t.Provenance, t.Signals))
	}
	for _, q := range p.Queues {
		fmt.Fprintf(&b, "- queue `%s` — %s\n", q.Name, provenanceWord(q.Provenance, q.Signals))
	}

	if len(p.Warnings) > 0 {
		b.WriteString("\n## Caveats — imported, but not identical to AWS\n\n")
		for _, n := range p.Warnings {
			fmt.Fprintf(&b, "- **%s**: %s\n", n.Subject, n.Detail)
		}
	}
	if len(p.Unsupported) > 0 {
		b.WriteString("\n## Not imported — pulse can't represent these yet\n\n")
		for _, n := range p.Unsupported {
			fmt.Fprintf(&b, "- **%s**: %s\n", n.Subject, n.Detail)
		}
	}
	b.WriteString("\n---\n\n")
	b.WriteString("Your handlers are unchanged, plain AWS SDK code. pulse only ever read from\n")
	b.WriteString("AWS — nothing in your account was created, modified, or deleted.\n")
	return b.String()
}

// Summary is the one-screen count for the console after a successful import.
func (p *Plan) Summary() string {
	parts := []string{
		plural(len(p.Functions), "function", "functions"),
		plural(len(p.Triggers), "trigger", "triggers"),
	}
	if n := len(p.Tables); n > 0 {
		parts = append(parts, plural(n, "table", "tables"))
	}
	if n := len(p.Queues); n > 0 {
		parts = append(parts, plural(n, "queue", "queues"))
	}
	return strings.Join(parts, " · ")
}

func provenanceWord(pv Provenance, signals []string) string {
	switch pv {
	case Confirmed:
		return "confirmed by AWS configuration"
	case Picked:
		return "chosen by you from the account"
	default:
		if len(signals) == 0 {
			return "inferred"
		}
		sorted := append([]string(nil), signals...)
		sort.Strings(sorted)
		return "inferred from " + strings.Join(sorted, ", ")
	}
}

func skSuffix(sk *Key) string {
	if sk == nil {
		return ""
	}
	return ", sk `" + sk.Name + "`"
}

// quoteIfNeeded keeps values that contain spaces or a # readable when the
// .env file is parsed back.
func quoteIfNeeded(v string) string {
	if v == "" {
		return ""
	}
	if strings.ContainsAny(v, " \t#\"'") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
	}
	return v
}

func orS(t string) string {
	if t == "" {
		return "S"
	}
	return t
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

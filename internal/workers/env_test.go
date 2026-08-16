package workers

import (
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
)

// envMap turns workerEnv's KEY=VALUE slice into something assertable.
func envMap(t *testing.T, lines []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("malformed env entry: %q", l)
		}
		out[k] = v
	}
	return out
}

// The layering contract: .env is the shared base, a function's own env:
// overrides it, and pulse's AWS_* wiring always wins so the local cloud
// cannot be broken from a project file.
func TestWorkerEnvPrecedence(t *testing.T) {
	fn := &config.Function{
		Name: "worker", Runtime: "python3.12", Handler: "handler.handler",
		CodeDir: "fn", Timeout: 5, Memory: 128,
		Env: map[string]string{
			"SHARED_KEY": "from-pulse-yaml",
			"ONLY_YAML":  "yaml",
			// A project must not be able to hijack the local façade.
			"AWS_REGION": "eu-west-9",
		},
	}
	cfg := &config.Config{
		Project: "env-test", Region: "us-east-1", Root: t.TempDir(),
		Functions: map[string]*config.Function{"worker": fn},
		DotEnv: map[string]string{
			"SHARED_KEY":  "from-dotenv",
			"ONLY_DOTENV": "dotenv",
			"AWS_REGION":  "eu-west-8",
		},
	}

	p := newPool(fn, cfg, nil, t.TempDir())
	p.awsEndpoint = "http://127.0.0.1:9999"
	got := envMap(t, p.workerEnv("w0"))

	checks := []struct{ key, want, why string }{
		{"ONLY_DOTENV", "dotenv", ".env values must reach the function"},
		{"ONLY_YAML", "yaml", "function env: must reach the function"},
		{"SHARED_KEY", "from-pulse-yaml", "function env: must override .env"},
		{"AWS_REGION", "us-east-1", "pulse's AWS wiring must beat both layers"},
	}
	for _, c := range checks {
		if got[c.key] != c.want {
			t.Errorf("%s = %q, want %q — %s", c.key, got[c.key], c.want, c.why)
		}
	}
	if got["AWS_ENDPOINT_URL"] != "http://127.0.0.1:9999" {
		t.Errorf("AWS_ENDPOINT_URL = %q, want the local façade", got["AWS_ENDPOINT_URL"])
	}
	// Parity with AWS: a function sees only configured variables, never the
	// developer's shell.
	t.Setenv("PULSE_SHELL_LEAK_CANARY", "leaked")
	if _, leaked := envMap(t, p.workerEnv("w0"))["PULSE_SHELL_LEAK_CANARY"]; leaked {
		t.Error("the parent shell must not be inherited by workers")
	}
}

// A project with no .env must behave exactly as before.
func TestWorkerEnvWithoutDotEnv(t *testing.T) {
	fn := &config.Function{
		Name: "worker", Runtime: "python3.12", Handler: "handler.handler",
		CodeDir: "fn", Timeout: 5, Memory: 128,
		Env: map[string]string{"GREETING": "yo"},
	}
	cfg := &config.Config{
		Project: "env-test", Region: "us-east-1", Root: t.TempDir(),
		Functions: map[string]*config.Function{"worker": fn},
	}
	got := envMap(t, newPool(fn, cfg, nil, t.TempDir()).workerEnv("w0"))
	if got["GREETING"] != "yo" {
		t.Errorf("GREETING = %q, want yo", got["GREETING"])
	}
}

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/config"
)

const addTestYAML = `project: demo
functions:
  createOrder:
    runtime: python3.12
    handler: handler.handler
    codeDir: services/create-order
  getOrders:
    runtime: python3.12
    handler: handler.handler
    codeDir: services/get-orders
    env: { CUSTOMERS_TABLE: customers }
`

// scaffoldAddProject writes a minimal valid project and points the CLI's
// working dir at it. Restores all touched globals afterwards.
func scaffoldAddProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(addTestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"create-order", "get-orders"} {
		codeDir := filepath.Join(dir, "services", fn)
		if err := os.MkdirAll(codeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codeDir, "handler.py"), []byte("def handler(e, c):\n    return {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prevChdir, prevPK, prevSK, prevFns := flagChdir, flagAddPK, flagAddSK, flagAddTableFns
	flagChdir, flagAddPK, flagAddSK, flagAddTableFns = dir, "id", "", nil
	t.Cleanup(func() {
		flagChdir, flagAddPK, flagAddSK, flagAddTableFns = prevChdir, prevPK, prevSK, prevFns
	})
	return dir
}

func yamlOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddTableWiresFunctionEnv(t *testing.T) {
	dir := scaffoldAddProject(t)
	flagAddPK = "email"
	flagAddTableFns = []string{"createOrder"}

	addTableCmd.Flags().Set("pk", "email")
	if err := runAddTable(addTableCmd, []string{"customers"}); err != nil {
		t.Fatalf("runAddTable: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("result does not validate: %v", err)
	}
	if cfg.Functions["createOrder"].Env["CUSTOMERS_TABLE"] != "customers" {
		t.Errorf("env not wired: %+v", cfg.Functions["createOrder"].Env)
	}
	if _, ok := cfg.Resources.Tables["customers"]; !ok {
		t.Error("table not declared")
	}
}

func TestAddTableExistingTableWiresOnly(t *testing.T) {
	dir := scaffoldAddProject(t)

	// Declare the table first, without any wiring.
	flagAddPK = "email"
	addTableCmd.Flags().Set("pk", "email")
	if err := runAddTable(addTableCmd, []string{"customers"}); err != nil {
		t.Fatal(err)
	}

	// Second run: table exists, default --pk, wire another function.
	if err := addTableCmd.Flags().Set("pk", "id"); err != nil {
		t.Fatal(err)
	}
	addTableCmd.Flags().Lookup("pk").Changed = false
	flagAddPK = "id"
	flagAddTableFns = []string{"createOrder"}
	if err := runAddTable(addTableCmd, []string{"customers"}); err != nil {
		t.Fatalf("wire-only run: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Functions["createOrder"].Env["CUSTOMERS_TABLE"] != "customers" {
		t.Errorf("env not wired on existing table: %+v", cfg.Functions["createOrder"].Env)
	}
	if strings.Count(yamlOf(t, dir), "customers:\n") != 1 {
		t.Error("table declared twice")
	}
}

func TestAddTableIdempotentEnv(t *testing.T) {
	dir := scaffoldAddProject(t)

	// getOrders already has CUSTOMERS_TABLE=customers in the fixture.
	flagAddPK = "email"
	addTableCmd.Flags().Set("pk", "email")
	flagAddTableFns = []string{"getOrders"}
	if err := runAddTable(addTableCmd, []string{"customers"}); err != nil {
		t.Fatalf("runAddTable: %v", err)
	}
	if n := strings.Count(yamlOf(t, dir), "CUSTOMERS_TABLE"); n != 1 {
		t.Errorf("expected exactly 1 CUSTOMERS_TABLE entry, found %d", n)
	}
}

func TestAddTableUnknownFunction(t *testing.T) {
	scaffoldAddProject(t)
	flagAddTableFns = []string{"nope"}
	err := runAddTable(addTableCmd, []string{"customers"})
	if err == nil || !strings.Contains(err.Error(), "unknown function") {
		t.Fatalf("want unknown-function error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "createOrder") {
		t.Errorf("error should list existing functions: %v", err)
	}
}

func TestAddTableExistingKeyChangeRefused(t *testing.T) {
	scaffoldAddProject(t)
	flagAddPK = "email"
	addTableCmd.Flags().Set("pk", "email")
	if err := runAddTable(addTableCmd, []string{"customers"}); err != nil {
		t.Fatal(err)
	}
	// Same table again with an explicit --pk must refuse.
	err := runAddTable(addTableCmd, []string{"customers"})
	if err == nil || !strings.Contains(err.Error(), "key can't be changed") {
		t.Fatalf("want key-change refusal, got: %v", err)
	}
}

func TestEnvVarForTable(t *testing.T) {
	cases := map[string]string{
		"customers":    "CUSTOMERS_TABLE",
		"order-events": "ORDER_EVENTS_TABLE",
		"v2.things":    "V2_THINGS_TABLE",
	}
	for in, want := range cases {
		if got := envVarForTable(in); got != want {
			t.Errorf("envVarForTable(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoveFunctionDropsTriggers(t *testing.T) {
	dir := scaffoldAddProject(t)
	// Wire a route + queue trigger at createOrder, then remove the function.
	flagAddFn = "createOrder"
	if err := runAddRoute(addRouteCmd, []string{"POST", "/orders"}); err != nil {
		t.Fatal(err)
	}
	if err := runRemoveFunction(removeFunctionCmd, []string{"createOrder"}); err != nil {
		t.Fatalf("remove function: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("result invalid: %v", err)
	}
	if _, ok := cfg.Functions["createOrder"]; ok {
		t.Error("function still present")
	}
	for _, tr := range cfg.Triggers {
		if tr.Function == "createOrder" {
			t.Error("dangling trigger survived")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "services", "create-order", "handler.py")); err != nil {
		t.Error("code must be kept")
	}
}

func TestRemoveRouteExactMatch(t *testing.T) {
	dir := scaffoldAddProject(t)
	flagAddFn = "createOrder"
	if err := runAddRoute(addRouteCmd, []string{"POST", "/orders"}); err != nil {
		t.Fatal(err)
	}
	if err := runRemoveRoute(removeRouteCmd, []string{"post", "/orders"}); err != nil {
		t.Fatalf("remove route (case-insensitive method): %v", err)
	}
	if err := runRemoveRoute(removeRouteCmd, []string{"POST", "/orders"}); err == nil {
		t.Fatal("second removal should error with route list")
	}
	cfg, _ := config.Load(filepath.Join(dir, config.FileName))
	if len(cfg.Triggers) != 0 {
		t.Errorf("triggers left: %d", len(cfg.Triggers))
	}
}

func TestRemoveTableCleansEnv(t *testing.T) {
	dir := scaffoldAddProject(t)
	flagAddPK = "email"
	addTableCmd.Flags().Set("pk", "email")
	flagAddTableFns = []string{"createOrder"}
	if err := runAddTable(addTableCmd, []string{"customers"}); err != nil {
		t.Fatal(err)
	}
	if err := runRemoveTable(removeTableCmd, []string{"customers"}); err != nil {
		t.Fatalf("remove table: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("result invalid: %v", err)
	}
	if _, ok := cfg.Resources.Tables["customers"]; ok {
		t.Error("table still declared")
	}
	if v, ok := cfg.Functions["createOrder"].Env["CUSTOMERS_TABLE"]; ok {
		t.Errorf("env wiring survived: %v", v)
	}
	// getOrders' fixture env pointed at customers too — must be cleaned.
	if v, ok := cfg.Functions["getOrders"].Env["CUSTOMERS_TABLE"]; ok {
		t.Errorf("fixture env wiring survived: %v", v)
	}
}

func TestRemoveQueueRefusesWhenUsedAsDLQ(t *testing.T) {
	scaffoldAddProject(t)
	flagAddWorker, flagAddDLQ = "createOrder", true
	if err := runAddQueue(addQueueCmd, []string{"jobs"}); err != nil {
		t.Fatal(err)
	}
	flagAddWorker, flagAddDLQ = "", false
	err := runRemoveQueue(removeQueueCmd, []string{"jobs-dlq"})
	if err == nil || !strings.Contains(err.Error(), "dead-letter queue of") {
		t.Fatalf("want dlq-in-use refusal, got: %v", err)
	}
	if err := runRemoveQueue(removeQueueCmd, []string{"jobs"}); err != nil {
		t.Fatalf("removing the main queue: %v", err)
	}
}

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	ddbsvc "github.com/geetnsh2k1/pulse/internal/services/dynamodb"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// pulse tables — look inside your data without aws-cli. Bare: all tables
// with counts. With a name: the items themselves.

var (
	flagTablesLimit  int
	flagTablesDelete string
	flagTablesSK     string
)

var tablesCmd = &cobra.Command{
	Use:   "tables [name]",
	Short: "Browse your tables — and the items inside them",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runTables,
}

func init() {
	tablesCmd.Flags().IntVarP(&flagTablesLimit, "limit", "n", 20, "items to show")
	tablesCmd.Flags().StringVar(&flagTablesDelete, "delete", "", "delete the item with this partition-key value")
	tablesCmd.Flags().StringVar(&flagTablesSK, "sk", "", "sort-key value (needed with --delete on sk tables)")
	rootCmd.AddCommand(tablesCmd)
}

func runTables(_ *cobra.Command, args []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if len(cfg.Resources.Tables) == 0 {
		fmt.Println(ui.Hint("no tables declared — `pulse add table <name> --pk <key>` creates one"))
		return nil
	}

	if flagTablesDelete != "" {
		if len(args) != 1 {
			return fmt.Errorf("--delete needs the table name: `pulse tables <name> --delete <key>`")
		}
		return deleteTableItem(cfg, args[0])
	}
	if len(args) == 1 {
		return printTableItems(cfg, args[0])
	}

	fmt.Println(ui.AccentBold("tables"))
	names := sortedKeys(cfg.Resources.Tables)
	nameW := 0
	for _, n := range names {
		nameW = max(nameW, len(n))
	}
	for _, name := range names {
		items, _, err := fetchItems(cfg, name, 1000)
		count := ui.Dim("engine unreachable")
		if err == nil {
			count = ui.Dim(fmt.Sprintf("%d item(s)", len(items)))
			if len(items) == 1000 {
				count = ui.Dim("1000+ item(s)")
			}
		}
		fmt.Printf("  %s%s  %s\n", ui.Bold(name), pad(name, nameW), count)
	}
	fmt.Println(ui.Hint("\nsee inside one: `pulse tables <name>`"))
	return nil
}

func printTableItems(cfg *config.Config, name string) error {
	if _, ok := cfg.Resources.Tables[name]; !ok {
		return fmt.Errorf("unknown table %q — this project has: %s",
			name, strings.Join(sortedKeys(cfg.Resources.Tables), ", "))
	}
	items, more, err := fetchItems(cfg, name, flagTablesLimit)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println(ui.Hint(fmt.Sprintf("table %s is empty — write something and look again", name)))
		return nil
	}

	tb := cfg.Resources.Tables[name]
	fmt.Printf("%s %s\n", ui.AccentBold(name), ui.Dim(fmt.Sprintf("— %d item(s) shown", len(items))))
	for _, raw := range items {
		item := plainItem(raw)
		key := fmt.Sprint(item[tb.PK.Name])
		if tb.SK != nil {
			key += " · " + fmt.Sprint(item[tb.SK.Name])
		}
		fmt.Printf("  %s  %s\n", ui.Bold(key), ui.Dim(compactFields(item, tb)))
	}
	if more {
		fmt.Println(ui.Hint(fmt.Sprintf("\nmore exist — `pulse tables %s -n %d` shows more", name, flagTablesLimit*2)))
	}
	return nil
}

// compactFields renders everything except the key, sorted, truncated.
func compactFields(item map[string]any, tb *config.Table) string {
	keys := make([]string, 0, len(item))
	for k := range item {
		if k == tb.PK.Name || (tb.SK != nil && k == tb.SK.Name) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, _ := json.Marshal(item[k])
		parts = append(parts, k+"="+string(v))
	}
	line := strings.Join(parts, " · ")
	if r := []rune(line); len(r) > 110 {
		line = string(r[:107]) + "…"
	}
	return line
}

// fetchItems scans via the running engine, or directly against the store.
func fetchItems(cfg *config.Config, table string, limit int) (items []map[string]any, more bool, err error) {
	if info, ok := engine.Current(cfg.Root); ok {
		u := fmt.Sprintf("%s/api/tables/items?name=%s&limit=%d", info.Addr, url.QueryEscape(table), limit)
		resp, err := http.Get(u)
		if err != nil {
			return nil, false, fmt.Errorf("calling the engine: %w", err)
		}
		defer resp.Body.Close()
		var out struct {
			Items []map[string]any `json:"items"`
			More  bool             `json:"more"`
			Error string           `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, false, err
		}
		if out.Error != "" {
			return nil, false, fmt.Errorf("%s", out.Error)
		}
		return out.Items, out.More, nil
	}

	st, err := store.Open(cfg.Root)
	if err != nil {
		return nil, false, err
	}
	defer st.Close()
	svc := ddbsvc.New(cfg, st)
	if err := svc.Init(cfg); err != nil {
		return nil, false, err
	}
	page, apiErr := svc.Scan(table, "", nil, nil, limit, nil)
	if apiErr != nil {
		return nil, false, fmt.Errorf("%s", apiErr.Message)
	}
	return page.Items, page.LastKey != nil, nil
}

// plainAV unwraps DynamoDB's wire format for display: {"S":"x"} → "x",
// {"BOOL":true} → true, {"N":"42"} → 42, recursively through lists/maps.
func plainAV(v any) any {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return v
	}
	for kind, inner := range m {
		switch kind {
		case "S", "BOOL", "N", "SS", "NS", "B", "BS":
			return inner
		case "NULL":
			return nil
		case "L":
			list, _ := inner.([]any)
			out := make([]any, len(list))
			for i, e := range list {
				out[i] = plainAV(e)
			}
			return out
		case "M":
			mm, _ := inner.(map[string]any)
			out := make(map[string]any, len(mm))
			for k, e := range mm {
				out[k] = plainAV(e)
			}
			return out
		}
	}
	return v
}

func plainItem(item map[string]any) map[string]any {
	out := make(map[string]any, len(item))
	for k, v := range item {
		out[k] = plainAV(v)
	}
	return out
}

// deleteTableItem removes one item by key — DynamoDB semantics, so an
// absent key is a quiet success.
func deleteTableItem(cfg *config.Config, name string) error {
	tb, ok := cfg.Resources.Tables[name]
	if !ok {
		return fmt.Errorf("unknown table %q — this project has: %s",
			name, strings.Join(sortedKeys(cfg.Resources.Tables), ", "))
	}
	if tb.SK != nil && flagTablesSK == "" {
		return fmt.Errorf("table %s has a sort key (%s) — pass it too: --sk <value>", name, tb.SK.Name)
	}
	if tb.SK == nil && flagTablesSK != "" {
		return fmt.Errorf("table %s has no sort key — drop the --sk flag", name)
	}

	key := map[string]any{tb.PK.Name: map[string]any{tb.PK.Type: flagTablesDelete}}
	if tb.SK != nil {
		key[tb.SK.Name] = map[string]any{tb.SK.Type: flagTablesSK}
	}

	if info, ok := engine.Current(cfg.Root); ok {
		body, _ := json.Marshal(map[string]any{"name": name, "key": key})
		resp, err := http.Post(info.Addr+"/api/tables/items/delete", "application/json", strings.NewReader(string(body)))
		if err != nil {
			return fmt.Errorf("calling the engine: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			return fmt.Errorf("%s", e.Error)
		}
	} else {
		st, err := store.Open(cfg.Root)
		if err != nil {
			return err
		}
		defer st.Close()
		svc := ddbsvc.New(cfg, st)
		if err := svc.Init(cfg); err != nil {
			return err
		}
		if _, apiErr := svc.Delete(name, key, "", nil, nil, false); apiErr != nil {
			return fmt.Errorf("%s", apiErr.Message)
		}
	}

	fmt.Printf("%s deleted %s from %s %s\n", ui.OK("✓"), ui.Bold(flagTablesDelete), ui.Bold(name),
		ui.Dim("(an absent key is a quiet success — DynamoDB semantics)"))
	return nil
}

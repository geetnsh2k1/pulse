// Package dynamodb is pulse's local DynamoDB: SQLite-backed tables speaking
// the real wire protocol, with a documented subset of the expression
// language (see expr.go). Fidelity where it matters, loud errors where the
// subset ends.
package dynamodb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/geetnsh2k1/pulse/internal/awsfacade"
	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/store"
)

const maxItemBytes = 400 * 1024 // AWS's per-item cap

type tableDef struct {
	Name   string
	PKName string
	PKType string
	SKName string
	SKType string
}

// TableStats is the inspection view for /api/tables and `pulse list`.
type TableStats struct {
	Name  string `json:"name"`
	Key   string `json:"key"`
	Items int    `json:"items"`
}

type Service struct {
	st *store.Store

	mu     sync.Mutex
	tables map[string]*tableDef
}

func New(cfg *config.Config, st *store.Store) *Service {
	return &Service{st: st, tables: map[string]*tableDef{}}
}

// Init loads persisted table definitions and upserts the ones declared in
// pulse.yaml (declared config wins over stale persisted definitions).
func (s *Service) Init(cfg *config.Config) error {
	rows, err := s.st.DB().Query(`SELECT name, pk_name, pk_type, COALESCE(sk_name,''), COALESCE(sk_type,'') FROM ddb_tables`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t tableDef
		if err := rows.Scan(&t.Name, &t.PKName, &t.PKType, &t.SKName, &t.SKType); err != nil {
			rows.Close()
			return err
		}
		s.tables[t.Name] = &t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for name, tb := range cfg.Resources.Tables {
		def := &tableDef{Name: name, PKName: tb.PK.Name, PKType: tb.PK.Type}
		if tb.SK != nil {
			def.SKName, def.SKType = tb.SK.Name, tb.SK.Type
		}
		if err := s.persistTable(def); err != nil {
			return err
		}
		s.tables[name] = def
	}
	return nil
}

func (s *Service) persistTable(t *tableDef) error {
	_, err := s.st.DB().Exec(
		`INSERT INTO ddb_tables (name, pk_name, pk_type, sk_name, sk_type) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET pk_name=excluded.pk_name, pk_type=excluded.pk_type,
		   sk_name=excluded.sk_name, sk_type=excluded.sk_type`,
		t.Name, t.PKName, t.PKType, nullable(t.SKName), nullable(t.SKType))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Service) table(name string) *tableDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tables[name]
}

// ---- errors ----

// tableNotFoundErr teaches: the message carries the exact pulse.yaml snippet
// that would fix it.
func tableNotFoundErr(name string, knownTables []string) *awsfacade.APIError {
	known := strings.Join(knownTables, ", ")
	if known == "" {
		known = "none yet"
	}
	msg := fmt.Sprintf(
		"table %q is not declared. Add it to pulse.yaml:\n\n  resources:\n    tables:\n      %s:\n        pk: id\n\n(known tables: %s)",
		name, name, known)

	// The name is the import placeholder, so the advice above would be wrong:
	// nobody wants a table called CHANGE_ME. The real problem is one line in
	// .env, and this is the first request an imported project ever serves.
	if name == config.Placeholder {
		msg = fmt.Sprintf(
			"the table name is still %q — an environment variable in .env hasn't been filled in yet.\n\n"+
				"  Open .env and replace every %s with the real value, then try again.\n"+
				"  (this project's tables: %s)",
			config.Placeholder, config.Placeholder, known)
	}

	return &awsfacade.APIError{
		Type:      "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
		QueryCode: "ResourceNotFoundException",
		Message:   msg,
	}
}

func (s *Service) errTableNotFound(name string) *awsfacade.APIError {
	return tableNotFoundErr(name, s.TableNames())
}

// errTableNotFoundLocked is for callers already holding s.mu.
func (s *Service) errTableNotFoundLocked(name string) *awsfacade.APIError {
	names := make([]string, 0, len(s.tables))
	for n := range s.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return tableNotFoundErr(name, names)
}

func errValidation(format string, args ...any) *awsfacade.APIError {
	return &awsfacade.APIError{
		Type:      "com.amazon.coral.validate#ValidationException",
		QueryCode: "ValidationException",
		Message:   fmt.Sprintf(format, args...),
	}
}

func errCondFailed() *awsfacade.APIError {
	return &awsfacade.APIError{
		Type:      "com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException",
		QueryCode: "ConditionalCheckFailedException",
		Message:   "The conditional request failed",
	}
}

func errInternal(err error) *awsfacade.APIError {
	return &awsfacade.APIError{
		Type:      "com.amazonaws.dynamodb.v20120810#InternalServerError",
		QueryCode: "InternalServerError",
		Message:   err.Error(),
		Status:    500,
	}
}

// ---- key handling ----

// encodeKeyAV canonicalizes one key AttributeValue for storage/lookup.
func encodeKeyAV(av any, wantType, attr string) (string, *awsfacade.APIError) {
	kind, val, ok := avKind(av)
	if !ok {
		return "", errValidation("key attribute %q is not a valid attribute value", attr)
	}
	if kind != wantType {
		return "", errValidation("key attribute %q must be of type %s (got %s)", attr, wantType, kind)
	}
	switch kind {
	case "S", "B":
		return kind + ":" + val.(string), nil
	case "N":
		f, ok := numOf(val)
		if !ok {
			return "", errValidation("key attribute %q is not a valid number", attr)
		}
		return "N:" + formatNum(f), nil
	}
	return "", errValidation("key type %s is not usable as a key", kind)
}

// keyOf extracts and encodes (pk, sk) from an item or Key map. strict
// requires the map to contain exactly the key attributes with no extras
// (GetItem/Delete/Update semantics).
func (s *Service) keyOf(t *tableDef, m map[string]any, strict bool) (pk, sk string, apiErr *awsfacade.APIError) {
	pkAV, ok := m[t.PKName]
	if !ok {
		return "", "", errValidation("missing the partition key %q", t.PKName)
	}
	pk, apiErr = encodeKeyAV(pkAV, t.PKType, t.PKName)
	if apiErr != nil {
		return "", "", apiErr
	}
	if t.SKName != "" {
		skAV, ok := m[t.SKName]
		if !ok {
			return "", "", errValidation("missing the sort key %q", t.SKName)
		}
		sk, apiErr = encodeKeyAV(skAV, t.SKType, t.SKName)
		if apiErr != nil {
			return "", "", apiErr
		}
	}
	if strict {
		want := 1
		if t.SKName != "" {
			want = 2
		}
		if len(m) != want {
			return "", "", errValidation("the Key must contain exactly the table's key attributes")
		}
	}
	return pk, sk, nil
}

// ---- raw storage ----

func (s *Service) readItem(t *tableDef, pk, sk string) (map[string]any, *awsfacade.APIError) {
	var raw string
	err := s.st.DB().QueryRow(
		`SELECT item FROM ddb_items WHERE tbl = ? AND pk = ? AND sk = ?`, t.Name, pk, sk).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal(err)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return nil, errInternal(err)
	}
	return item, nil
}

func (s *Service) writeItem(t *tableDef, pk, sk string, item map[string]any) *awsfacade.APIError {
	raw, err := json.Marshal(item)
	if err != nil {
		return errInternal(err)
	}
	if len(raw) > maxItemBytes {
		return errValidation("item size %d exceeds the 400KB limit", len(raw))
	}
	if _, err := s.st.DB().Exec(
		`INSERT INTO ddb_items (tbl, pk, sk, item) VALUES (?, ?, ?, ?)
		 ON CONFLICT(tbl, pk, sk) DO UPDATE SET item = excluded.item`,
		t.Name, pk, sk, string(raw)); err != nil {
		return errInternal(err)
	}
	return nil
}

func (s *Service) removeItem(t *tableDef, pk, sk string) *awsfacade.APIError {
	if _, err := s.st.DB().Exec(
		`DELETE FROM ddb_items WHERE tbl = ? AND pk = ? AND sk = ?`, t.Name, pk, sk); err != nil {
		return errInternal(err)
	}
	return nil
}

// evalCondition parses+runs a ConditionExpression against the existing item
// (empty map when the item doesn't exist, like AWS).
func evalCondition(expr string, names map[string]string, values map[string]any, existing map[string]any) *awsfacade.APIError {
	if expr == "" {
		return nil
	}
	cond, err := ParseCondition(expr, names)
	if err != nil {
		return errValidation("ConditionExpression: %v", err)
	}
	if existing == nil {
		existing = map[string]any{}
	}
	ok, err := cond.Eval(existing, values)
	if err != nil {
		return errValidation("ConditionExpression: %v", err)
	}
	if !ok {
		return errCondFailed()
	}
	return nil
}

// ---- operations ----

func (s *Service) Put(table string, item map[string]any, condExpr string, names map[string]string, values map[string]any, returnOld bool) (map[string]any, *awsfacade.APIError) {
	t := s.table(table)
	if t == nil {
		return nil, s.errTableNotFound(table)
	}
	pk, sk, apiErr := s.keyOf(t, item, false)
	if apiErr != nil {
		return nil, apiErr
	}
	old, apiErr := s.readItem(t, pk, sk)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := evalCondition(condExpr, names, values, old); apiErr != nil {
		return nil, apiErr
	}
	if apiErr := s.writeItem(t, pk, sk, item); apiErr != nil {
		return nil, apiErr
	}
	if returnOld {
		return old, nil
	}
	return nil, nil
}

func (s *Service) Get(table string, key map[string]any) (map[string]any, *awsfacade.APIError) {
	t := s.table(table)
	if t == nil {
		return nil, s.errTableNotFound(table)
	}
	pk, sk, apiErr := s.keyOf(t, key, true)
	if apiErr != nil {
		return nil, apiErr
	}
	return s.readItem(t, pk, sk)
}

func (s *Service) Update(table string, key map[string]any, updateExpr, condExpr string, names map[string]string, values map[string]any, returnValues string) (map[string]any, *awsfacade.APIError) {
	t := s.table(table)
	if t == nil {
		return nil, s.errTableNotFound(table)
	}
	pk, sk, apiErr := s.keyOf(t, key, true)
	if apiErr != nil {
		return nil, apiErr
	}
	existing, apiErr := s.readItem(t, pk, sk)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := evalCondition(condExpr, names, values, existing); apiErr != nil {
		return nil, apiErr
	}

	base := existing
	if base == nil {
		// UpdateItem on a missing item creates it from the key, like AWS.
		base = map[string]any{}
		for k, v := range key {
			base[k] = v
		}
	}

	updated := base
	if updateExpr != "" {
		u, err := ParseUpdate(updateExpr, names)
		if err != nil {
			return nil, errValidation("UpdateExpression: %v", err)
		}
		updated, err = u.Apply(base, values)
		if err != nil {
			return nil, errValidation("UpdateExpression: %v", err)
		}
		for _, attr := range []string{t.PKName, t.SKName} {
			if attr == "" {
				continue
			}
			if !avEqual(updated[attr], base[attr]) {
				return nil, errValidation("cannot update the key attribute %q", attr)
			}
		}
	}
	if apiErr := s.writeItem(t, pk, sk, updated); apiErr != nil {
		return nil, apiErr
	}

	switch returnValues {
	case "ALL_NEW":
		return updated, nil
	case "ALL_OLD":
		return existing, nil
	case "", "NONE":
		return nil, nil
	}
	return nil, errValidation("ReturnValues %q is not supported by pulse (use NONE, ALL_NEW, or ALL_OLD)", returnValues)
}

func (s *Service) Delete(table string, key map[string]any, condExpr string, names map[string]string, values map[string]any, returnOld bool) (map[string]any, *awsfacade.APIError) {
	t := s.table(table)
	if t == nil {
		return nil, s.errTableNotFound(table)
	}
	pk, sk, apiErr := s.keyOf(t, key, true)
	if apiErr != nil {
		return nil, apiErr
	}
	existing, apiErr := s.readItem(t, pk, sk)
	if apiErr != nil {
		return nil, apiErr
	}
	if apiErr := evalCondition(condExpr, names, values, existing); apiErr != nil {
		return nil, apiErr
	}
	if apiErr := s.removeItem(t, pk, sk); apiErr != nil {
		return nil, apiErr
	}
	if returnOld {
		return existing, nil
	}
	return nil, nil
}

// pageResult carries Query/Scan output.
type pageResult struct {
	Items        []map[string]any
	ScannedCount int
	LastKey      map[string]any
}

type storedRow struct {
	pkEnc, skEnc string
	item         map[string]any
}

func (s *Service) Query(table string, kc *KeyCond, filterExpr string, names map[string]string, values map[string]any, limit int, forward bool, startKey map[string]any) (*pageResult, *awsfacade.APIError) {
	t := s.table(table)
	if t == nil {
		return nil, s.errTableNotFound(table)
	}

	// Split the key condition into partition part and optional sort part.
	var pkPart, skPart *KeyCondPart
	for i := range kc.Parts {
		part := &kc.Parts[i]
		switch part.Name {
		case t.PKName:
			pkPart = part
		case t.SKName:
			skPart = part
		default:
			return nil, errValidation("key condition references %q, which is not a key attribute of %s", part.Name, table)
		}
	}
	if pkPart == nil || pkPart.Op != "=" {
		return nil, errValidation("the key condition must test the partition key %q with =", t.PKName)
	}
	pkVal, err := resolveValue(values, pkPart.Ref1)
	if err != nil {
		return nil, errValidation("%v", err)
	}
	pkEnc, apiErr := encodeKeyAV(pkVal, t.PKType, t.PKName)
	if apiErr != nil {
		return nil, apiErr
	}

	rows, err2 := s.st.DB().Query(
		`SELECT sk, item FROM ddb_items WHERE tbl = ? AND pk = ?`, table, pkEnc)
	if err2 != nil {
		return nil, errInternal(err2)
	}
	var all []storedRow
	for rows.Next() {
		var r storedRow
		var raw string
		if err := rows.Scan(&r.skEnc, &raw); err != nil {
			rows.Close()
			return nil, errInternal(err)
		}
		if err := json.Unmarshal([]byte(raw), &r.item); err != nil {
			rows.Close()
			return nil, errInternal(err)
		}
		r.pkEnc = pkEnc
		all = append(all, r)
	}
	rows.Close()

	// Sort by typed sort key; encoded N doesn't sort numerically as text.
	sort.SliceStable(all, func(i, j int) bool {
		if t.SKName == "" {
			return all[i].skEnc < all[j].skEnc
		}
		c, err := avCompare(all[i].item[t.SKName], all[j].item[t.SKName])
		if err != nil {
			return all[i].skEnc < all[j].skEnc
		}
		return c < 0
	})
	if !forward {
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
	}

	// Apply the sort-key condition to define the candidate range.
	candidates := all[:0:0]
	for _, r := range all {
		match, apiErr := skMatches(t, skPart, r.item, values)
		if apiErr != nil {
			return nil, apiErr
		}
		if match {
			candidates = append(candidates, r)
		}
	}

	return s.page(t, candidates, filterExpr, names, values, limit, startKey)
}

func skMatches(t *tableDef, skPart *KeyCondPart, item map[string]any, values map[string]any) (bool, *awsfacade.APIError) {
	if skPart == nil {
		return true, nil
	}
	have := item[t.SKName]
	v1, err := resolveValue(values, skPart.Ref1)
	if err != nil {
		return false, errValidation("%v", err)
	}
	switch skPart.Op {
	case "begins_with":
		return avBeginsWith(have, v1), nil
	case "between":
		v2, err := resolveValue(values, skPart.Ref2)
		if err != nil {
			return false, errValidation("%v", err)
		}
		c1, e1 := avCompare(have, v1)
		c2, e2 := avCompare(have, v2)
		return e1 == nil && e2 == nil && c1 >= 0 && c2 <= 0, nil
	default:
		c, err := avCompare(have, v1)
		if err != nil {
			return false, nil
		}
		switch skPart.Op {
		case "=":
			return c == 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		case ">=":
			return c >= 0, nil
		}
	}
	return false, errValidation("unsupported sort key operator %q", skPart.Op)
}

func (s *Service) Scan(table string, filterExpr string, names map[string]string, values map[string]any, limit int, startKey map[string]any) (*pageResult, *awsfacade.APIError) {
	t := s.table(table)
	if t == nil {
		return nil, s.errTableNotFound(table)
	}
	rows, err := s.st.DB().Query(
		`SELECT pk, sk, item FROM ddb_items WHERE tbl = ? ORDER BY pk, sk`, table)
	if err != nil {
		return nil, errInternal(err)
	}
	var all []storedRow
	for rows.Next() {
		var r storedRow
		var raw string
		if err := rows.Scan(&r.pkEnc, &r.skEnc, &raw); err != nil {
			rows.Close()
			return nil, errInternal(err)
		}
		if err := json.Unmarshal([]byte(raw), &r.item); err != nil {
			rows.Close()
			return nil, errInternal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	return s.page(t, all, filterExpr, names, values, limit, startKey)
}

// page implements AWS paging semantics: Limit bounds items *read* (before
// filtering); LastEvaluatedKey points at the last read item when more remain.
func (s *Service) page(t *tableDef, candidates []storedRow, filterExpr string, names map[string]string, values map[string]any, limit int, startKey map[string]any) (*pageResult, *awsfacade.APIError) {
	start := 0
	if len(startKey) > 0 {
		spk, ssk, apiErr := s.keyOf(t, startKey, true)
		if apiErr != nil {
			return nil, errValidation("ExclusiveStartKey: %s", apiErr.Message)
		}
		for i, r := range candidates {
			if r.pkEnc == spk && r.skEnc == ssk {
				start = i + 1
				break
			}
		}
	}

	var filter *Condition
	if filterExpr != "" {
		var err error
		filter, err = ParseCondition(filterExpr, names)
		if err != nil {
			return nil, errValidation("FilterExpression: %v", err)
		}
	}

	res := &pageResult{Items: []map[string]any{}}
	i := start
	for ; i < len(candidates); i++ {
		if limit > 0 && res.ScannedCount == limit {
			break
		}
		r := candidates[i]
		res.ScannedCount++
		keep := true
		if filter != nil {
			var err error
			keep, err = filter.Eval(r.item, values)
			if err != nil {
				return nil, errValidation("FilterExpression: %v", err)
			}
		}
		if keep {
			res.Items = append(res.Items, r.item)
		}
	}
	if i < len(candidates) {
		last := candidates[i-1]
		res.LastKey = map[string]any{t.PKName: last.item[t.PKName]}
		if t.SKName != "" {
			res.LastKey[t.SKName] = last.item[t.SKName]
		}
	}
	return res, nil
}

// ---- table admin ----

func (s *Service) EnsureDeclared(def *tableDef) *awsfacade.APIError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tables[def.Name]; exists {
		return &awsfacade.APIError{
			Type:      "com.amazonaws.dynamodb.v20120810#ResourceInUseException",
			QueryCode: "ResourceInUseException",
			Message:   fmt.Sprintf("Table already exists: %s", def.Name),
		}
	}
	if err := s.persistTable(def); err != nil {
		return errInternal(err)
	}
	s.tables[def.Name] = def
	return nil
}

func (s *Service) Drop(name string) *awsfacade.APIError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tables[name]; !exists {
		return s.errTableNotFoundLocked(name)
	}
	if _, err := s.st.DB().Exec(`DELETE FROM ddb_items WHERE tbl = ?`, name); err != nil {
		return errInternal(err)
	}
	if _, err := s.st.DB().Exec(`DELETE FROM ddb_tables WHERE name = ?`, name); err != nil {
		return errInternal(err)
	}
	delete(s.tables, name)
	return nil
}

func (s *Service) TableNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tables))
	for n := range s.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Service) itemCount(name string) int {
	var n int
	_ = s.st.DB().QueryRow(`SELECT COUNT(*) FROM ddb_items WHERE tbl = ?`, name).Scan(&n)
	return n
}

// AllStats reports every table with its key shape and item count.
func (s *Service) AllStats() []TableStats {
	out := []TableStats{}
	for _, name := range s.TableNames() {
		t := s.table(name)
		key := fmt.Sprintf("pk %s %s", t.PKName, t.PKType)
		if t.SKName != "" {
			key += fmt.Sprintf(", sk %s %s", t.SKName, t.SKType)
		}
		out = append(out, TableStats{Name: name, Key: key, Items: s.itemCount(name)})
	}
	return out
}

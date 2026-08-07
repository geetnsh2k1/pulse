package dynamodb

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/store"
)

func testService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Resources: config.Resources{
			Tables: map[string]*config.Table{
				"orders": {Name: "orders", PK: config.KeyDef{Name: "id", Type: "S"}},
				"events": {Name: "events", PK: config.KeyDef{Name: "userId", Type: "S"},
					SK: &config.KeyDef{Name: "seq", Type: "N"}},
			},
		},
	}
	s := New(cfg, st)
	if err := s.Init(cfg); err != nil {
		t.Fatal(err)
	}
	return s
}

// do runs a protocol action and decodes the response envelope.
func do(t *testing.T, s *Service, action, body string) map[string]any {
	t.Helper()
	resp, apiErr := s.Do(action, []byte(body))
	if apiErr != nil {
		t.Fatalf("%s: %s", action, apiErr.Message)
	}
	raw, _ := json.Marshal(resp)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func doErr(t *testing.T, s *Service, action, body string) *string {
	t.Helper()
	_, apiErr := s.Do(action, []byte(body))
	if apiErr == nil {
		return nil
	}
	msg := apiErr.Type + ": " + apiErr.Message
	return &msg
}

func TestPutGetRoundtripAllTypes(t *testing.T) {
	s := testService(t)
	item := `{
		"id": {"S": "o-1"},
		"qty": {"N": "2"},
		"flag": {"BOOL": true},
		"nothing": {"NULL": true},
		"tags": {"SS": ["a","b"]},
		"nums": {"NS": ["1","2"]},
		"list": {"L": [{"S":"x"},{"N":"1"}]},
		"meta": {"M": {"nested": {"S": "ok"}}}
	}`
	do(t, s, "PutItem", `{"TableName":"orders","Item":`+item+`}`)

	got := do(t, s, "GetItem", `{"TableName":"orders","Key":{"id":{"S":"o-1"}}}`)
	gotItem := got["Item"].(map[string]any)
	if len(gotItem) != 8 {
		t.Errorf("attributes roundtripped = %d, want 8", len(gotItem))
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(item), &want)
	for k, v := range want {
		if !avEqual(gotItem[k], v) {
			t.Errorf("attribute %s = %v, want %v", k, gotItem[k], v)
		}
	}

	// Missing item → no Item key.
	got = do(t, s, "GetItem", `{"TableName":"orders","Key":{"id":{"S":"missing"}}}`)
	if _, has := got["Item"]; has {
		t.Error("missing item should omit the Item key")
	}
}

func TestKeyValidationAndUnknownTable(t *testing.T) {
	s := testService(t)
	if msg := doErr(t, s, "PutItem", `{"TableName":"nope","Item":{"id":{"S":"x"}}}`); msg == nil || !strings.Contains(*msg, "ResourceNotFound") {
		t.Errorf("unknown table error = %v", msg)
	}
	if msg := doErr(t, s, "PutItem", `{"TableName":"orders","Item":{"other":{"S":"x"}}}`); msg == nil || !strings.Contains(*msg, "partition key") {
		t.Errorf("missing pk error = %v", msg)
	}
	if msg := doErr(t, s, "PutItem", `{"TableName":"orders","Item":{"id":{"N":"1"}}}`); msg == nil || !strings.Contains(*msg, "must be of type S") {
		t.Errorf("wrong key type error = %v", msg)
	}
	if msg := doErr(t, s, "GetItem", `{"TableName":"orders","Key":{"id":{"S":"x"},"extra":{"S":"y"}}}`); msg == nil || !strings.Contains(*msg, "exactly") {
		t.Errorf("extra key attr error = %v", msg)
	}
}

func TestConditionalPut(t *testing.T) {
	s := testService(t)
	put := `{"TableName":"orders","Item":{"id":{"S":"c-1"},"v":{"N":"1"}},"ConditionExpression":"attribute_not_exists(id)"}`
	do(t, s, "PutItem", put)

	msg := doErr(t, s, "PutItem", put)
	if msg == nil || !strings.Contains(*msg, "ConditionalCheckFailed") {
		t.Errorf("second conditional put = %v, want ConditionalCheckFailed", msg)
	}
}

func TestUpdateItemFlow(t *testing.T) {
	s := testService(t)
	do(t, s, "PutItem", `{"TableName":"orders","Item":{"id":{"S":"u-1"},"status":{"S":"pending"},"qty":{"N":"5"}}}`)

	got := do(t, s, "UpdateItem", `{
		"TableName":"orders",
		"Key":{"id":{"S":"u-1"}},
		"UpdateExpression":"SET #s = :done, qty = qty + :one REMOVE tmp",
		"ExpressionAttributeNames":{"#s":"status"},
		"ExpressionAttributeValues":{":done":{"S":"processed"},":one":{"N":"1"}},
		"ReturnValues":"ALL_NEW"
	}`)
	attrs := got["Attributes"].(map[string]any)
	if !avEqual(attrs["status"], av("S", "processed")) || !avEqual(attrs["qty"], av("N", "6")) {
		t.Errorf("ALL_NEW attrs = %v", attrs)
	}

	// Update on a missing key creates the item (AWS behavior).
	do(t, s, "UpdateItem", `{
		"TableName":"orders","Key":{"id":{"S":"fresh"}},
		"UpdateExpression":"SET n = :one",
		"ExpressionAttributeValues":{":one":{"N":"1"}}
	}`)
	got = do(t, s, "GetItem", `{"TableName":"orders","Key":{"id":{"S":"fresh"}}}`)
	if _, has := got["Item"]; !has {
		t.Error("UpdateItem should upsert a missing item")
	}

	// Key attributes are immutable.
	if msg := doErr(t, s, "UpdateItem", `{
		"TableName":"orders","Key":{"id":{"S":"u-1"}},
		"UpdateExpression":"SET id = :x",
		"ExpressionAttributeValues":{":x":{"S":"other"}}
	}`); msg == nil || !strings.Contains(*msg, "key attribute") {
		t.Errorf("key mutation error = %v", msg)
	}
}

func TestDeleteWithCondition(t *testing.T) {
	s := testService(t)
	do(t, s, "PutItem", `{"TableName":"orders","Item":{"id":{"S":"d-1"},"status":{"S":"pending"}}}`)

	msg := doErr(t, s, "DeleteItem", `{
		"TableName":"orders","Key":{"id":{"S":"d-1"}},
		"ConditionExpression":"#s = :done",
		"ExpressionAttributeNames":{"#s":"status"},
		"ExpressionAttributeValues":{":done":{"S":"done"}}
	}`)
	if msg == nil || !strings.Contains(*msg, "ConditionalCheckFailed") {
		t.Errorf("guarded delete = %v", msg)
	}

	got := do(t, s, "DeleteItem", `{"TableName":"orders","Key":{"id":{"S":"d-1"}},"ReturnValues":"ALL_OLD"}`)
	if _, has := got["Attributes"]; !has {
		t.Error("ALL_OLD should return the deleted item")
	}
}

func seedEvents(t *testing.T, s *Service) {
	t.Helper()
	for _, row := range []struct {
		user string
		seq  string
		kind string
	}{
		{"amy", "1", "login"}, {"amy", "2", "click"}, {"amy", "10", "logout"},
		{"raj", "1", "login"},
	} {
		do(t, s, "PutItem", `{"TableName":"events","Item":{
			"userId":{"S":"`+row.user+`"},"seq":{"N":"`+row.seq+`"},"kind":{"S":"`+row.kind+`"}}}`)
	}
}

func TestQuerySortKeyRangesAndOrder(t *testing.T) {
	s := testService(t)
	seedEvents(t, s)

	got := do(t, s, "Query", `{
		"TableName":"events",
		"KeyConditionExpression":"userId = :u",
		"ExpressionAttributeValues":{":u":{"S":"amy"}}
	}`)
	items := got["Items"].([]any)
	if len(items) != 3 {
		t.Fatalf("amy items = %d, want 3", len(items))
	}
	// Numeric sk ordering: 1, 2, 10 (not "1", "10", "2").
	last := items[2].(map[string]any)
	if !avEqual(last["seq"], av("N", "10")) {
		t.Errorf("numeric sort broken; last = %v", last["seq"])
	}

	// Descending.
	got = do(t, s, "Query", `{
		"TableName":"events",
		"KeyConditionExpression":"userId = :u",
		"ExpressionAttributeValues":{":u":{"S":"amy"}},
		"ScanIndexForward": false
	}`)
	first := got["Items"].([]any)[0].(map[string]any)
	if !avEqual(first["seq"], av("N", "10")) {
		t.Errorf("descending order broken; first = %v", first["seq"])
	}

	// Range on the sort key.
	got = do(t, s, "Query", `{
		"TableName":"events",
		"KeyConditionExpression":"userId = :u AND seq BETWEEN :a AND :b",
		"ExpressionAttributeValues":{":u":{"S":"amy"},":a":{"N":"2"},":b":{"N":"10"}}
	}`)
	if int(got["Count"].(float64)) != 2 {
		t.Errorf("between count = %v", got["Count"])
	}

	// Filter applies after the key condition.
	got = do(t, s, "Query", `{
		"TableName":"events",
		"KeyConditionExpression":"userId = :u",
		"FilterExpression":"kind = :k",
		"ExpressionAttributeValues":{":u":{"S":"amy"},":k":{"S":"login"}}
	}`)
	if int(got["Count"].(float64)) != 1 || int(got["ScannedCount"].(float64)) != 3 {
		t.Errorf("filter count/scanned = %v/%v", got["Count"], got["ScannedCount"])
	}
}

func TestQueryPagination(t *testing.T) {
	s := testService(t)
	seedEvents(t, s)

	got := do(t, s, "Query", `{
		"TableName":"events",
		"KeyConditionExpression":"userId = :u",
		"ExpressionAttributeValues":{":u":{"S":"amy"}},
		"Limit": 2
	}`)
	if len(got["Items"].([]any)) != 2 {
		t.Fatalf("page 1 = %v items", len(got["Items"].([]any)))
	}
	lek, has := got["LastEvaluatedKey"]
	if !has {
		t.Fatal("expected LastEvaluatedKey on a truncated page")
	}
	lekJSON, _ := json.Marshal(lek)

	got = do(t, s, "Query", `{
		"TableName":"events",
		"KeyConditionExpression":"userId = :u",
		"ExpressionAttributeValues":{":u":{"S":"amy"}},
		"ExclusiveStartKey": `+string(lekJSON)+`
	}`)
	items := got["Items"].([]any)
	if len(items) != 1 || !avEqual(items[0].(map[string]any)["seq"], av("N", "10")) {
		t.Errorf("page 2 = %v", items)
	}
	if _, has := got["LastEvaluatedKey"]; has {
		t.Error("final page must not carry LastEvaluatedKey")
	}
}

func TestScanWithFilterAndProjection(t *testing.T) {
	s := testService(t)
	seedEvents(t, s)

	got := do(t, s, "Scan", `{
		"TableName":"events",
		"FilterExpression":"kind = :k",
		"ExpressionAttributeValues":{":k":{"S":"login"}},
		"ProjectionExpression":"userId, kind"
	}`)
	items := got["Items"].([]any)
	if len(items) != 2 {
		t.Fatalf("scan filtered = %d, want 2", len(items))
	}
	for _, it := range items {
		m := it.(map[string]any)
		if len(m) != 2 {
			t.Errorf("projection leaked attributes: %v", m)
		}
	}
}

func TestBatchWriteAndGet(t *testing.T) {
	s := testService(t)
	do(t, s, "BatchWriteItem", `{"RequestItems":{"orders":[
		{"PutRequest":{"Item":{"id":{"S":"b-1"},"v":{"N":"1"}}}},
		{"PutRequest":{"Item":{"id":{"S":"b-2"},"v":{"N":"2"}}}}
	]}}`)

	got := do(t, s, "BatchGetItem", `{"RequestItems":{"orders":{"Keys":[
		{"id":{"S":"b-1"}},{"id":{"S":"b-2"}},{"id":{"S":"b-3"}}
	]}}}`)
	responses := got["Responses"].(map[string]any)["orders"].([]any)
	if len(responses) != 2 {
		t.Errorf("batch get = %d items, want 2 (missing keys skipped)", len(responses))
	}
}

func TestTableAdminAndGuardrails(t *testing.T) {
	s := testService(t)

	do(t, s, "CreateTable", `{
		"TableName":"runtime-made",
		"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"}],
		"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"}]
	}`)
	got := do(t, s, "ListTables", `{}`)
	if len(got["TableNames"].([]any)) != 3 {
		t.Errorf("tables = %v", got["TableNames"])
	}
	if msg := doErr(t, s, "CreateTable", `{
		"TableName":"orders",
		"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],
		"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}]
	}`); msg == nil || !strings.Contains(*msg, "ResourceInUse") {
		t.Errorf("duplicate create = %v", msg)
	}

	if msg := doErr(t, s, "Query", `{
		"TableName":"events","IndexName":"byKind",
		"KeyConditionExpression":"kind = :k",
		"ExpressionAttributeValues":{":k":{"S":"login"}}
	}`); msg == nil || !strings.Contains(*msg, "secondary indexes") {
		t.Errorf("index guardrail = %v", msg)
	}
	if msg := doErr(t, s, "TransactWriteItems", `{}`); msg == nil || !strings.Contains(*msg, "transactions") {
		t.Errorf("transactions guardrail = %v", msg)
	}

	stats := s.AllStats()
	if len(stats) != 3 || stats[1].Key == "" {
		t.Errorf("stats = %+v", stats)
	}
}

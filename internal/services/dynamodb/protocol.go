package dynamodb

import (
	"encoding/json"
	"fmt"

	"github.com/geetnsh2k1/pulse/internal/awsfacade"
)

// Do dispatches one AWS-JSON-protocol action (the façade routes
// X-Amz-Target: DynamoDB_20120810.<Action> here).
func (s *Service) Do(action string, body []byte) (any, *awsfacade.APIError) {
	switch action {
	case "PutItem":
		return s.doPut(body)
	case "GetItem":
		return s.doGet(body)
	case "UpdateItem":
		return s.doUpdate(body)
	case "DeleteItem":
		return s.doDelete(body)
	case "Query":
		return s.doQuery(body)
	case "Scan":
		return s.doScan(body)
	case "BatchWriteItem":
		return s.doBatchWrite(body)
	case "BatchGetItem":
		return s.doBatchGet(body)
	case "CreateTable":
		return s.doCreateTable(body)
	case "DescribeTable":
		return s.doDescribeTable(body)
	case "DeleteTable":
		return s.doDeleteTable(body)
	case "ListTables":
		return s.doListTables()
	case "TransactWriteItems", "TransactGetItems":
		return nil, errValidation("pulse does not implement DynamoDB transactions yet — use individual writes with ConditionExpressions")
	}
	return nil, errValidation("pulse does not implement DynamoDB %s yet — open an issue if you need it", action)
}

func decodeReq[T any](body []byte) (*T, *awsfacade.APIError) {
	var req T
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errValidation("request body is not valid JSON: %v", err)
		}
	}
	return &req, nil
}

// applyProjection keeps only the named top-level attributes.
func applyProjection(item map[string]any, projection string, names map[string]string) (map[string]any, *awsfacade.APIError) {
	if projection == "" || item == nil {
		return item, nil
	}
	toks, err := lex(projection)
	if err != nil {
		return nil, errValidation("ProjectionExpression: %v", err)
	}
	p := &parser{toks: toks, names: names}
	keep := map[string]bool{}
	for {
		path, err := p.path()
		if err != nil {
			return nil, errValidation("ProjectionExpression: %v", err)
		}
		keep[path] = true
		if p.peek().kind == tComma {
			p.next()
			continue
		}
		break
	}
	if !p.atEOF() {
		return nil, errValidation("ProjectionExpression: unexpected %q", p.peek().text)
	}
	out := map[string]any{}
	for k, v := range item {
		if keep[k] {
			out[k] = v
		}
	}
	return out, nil
}

type writeReq struct {
	TableName                 string
	Item                      map[string]any
	Key                       map[string]any
	UpdateExpression          string
	ConditionExpression       string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]any
	ReturnValues              string
}

func (s *Service) doPut(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[writeReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if len(req.Item) == 0 {
		return nil, errValidation("PutItem requires an Item")
	}
	returnOld := req.ReturnValues == "ALL_OLD"
	old, apiErr := s.Put(req.TableName, req.Item, req.ConditionExpression,
		req.ExpressionAttributeNames, req.ExpressionAttributeValues, returnOld)
	if apiErr != nil {
		return nil, apiErr
	}
	resp := map[string]any{}
	if returnOld && old != nil {
		resp["Attributes"] = old
	}
	return resp, nil
}

type getReq struct {
	TableName                string
	Key                      map[string]any
	ProjectionExpression     string
	ExpressionAttributeNames map[string]string
	ConsistentRead           bool // local reads are always consistent
}

func (s *Service) doGet(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[getReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	item, apiErr := s.Get(req.TableName, req.Key)
	if apiErr != nil {
		return nil, apiErr
	}
	if item == nil {
		return map[string]any{}, nil // AWS omits Item when absent
	}
	item, apiErr = applyProjection(item, req.ProjectionExpression, req.ExpressionAttributeNames)
	if apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{"Item": item}, nil
}

func (s *Service) doUpdate(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[writeReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	result, apiErr := s.Update(req.TableName, req.Key, req.UpdateExpression, req.ConditionExpression,
		req.ExpressionAttributeNames, req.ExpressionAttributeValues, req.ReturnValues)
	if apiErr != nil {
		return nil, apiErr
	}
	resp := map[string]any{}
	if result != nil {
		resp["Attributes"] = result
	}
	return resp, nil
}

func (s *Service) doDelete(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[writeReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	returnOld := req.ReturnValues == "ALL_OLD"
	old, apiErr := s.Delete(req.TableName, req.Key, req.ConditionExpression,
		req.ExpressionAttributeNames, req.ExpressionAttributeValues, returnOld)
	if apiErr != nil {
		return nil, apiErr
	}
	resp := map[string]any{}
	if returnOld && old != nil {
		resp["Attributes"] = old
	}
	return resp, nil
}

type queryReq struct {
	TableName                 string
	IndexName                 string
	KeyConditionExpression    string
	FilterExpression          string
	ProjectionExpression      string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]any
	Limit                     int
	ScanIndexForward          *bool
	ExclusiveStartKey         map[string]any
}

func (s *Service) doQuery(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[queryReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if req.IndexName != "" {
		return nil, errValidation("secondary indexes (IndexName) are not in pulse's local subset yet — query the base table")
	}
	if req.KeyConditionExpression == "" {
		return nil, errValidation("Query requires a KeyConditionExpression")
	}
	kc, err := ParseKeyCondition(req.KeyConditionExpression, req.ExpressionAttributeNames)
	if err != nil {
		return nil, errValidation("KeyConditionExpression: %v", err)
	}
	forward := req.ScanIndexForward == nil || *req.ScanIndexForward
	page, apiErr := s.Query(req.TableName, kc, req.FilterExpression,
		req.ExpressionAttributeNames, req.ExpressionAttributeValues,
		req.Limit, forward, req.ExclusiveStartKey)
	if apiErr != nil {
		return nil, apiErr
	}
	return pageResponse(page, req.ProjectionExpression, req.ExpressionAttributeNames)
}

type scanReq struct {
	TableName                 string
	IndexName                 string
	FilterExpression          string
	ProjectionExpression      string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]any
	Limit                     int
	ExclusiveStartKey         map[string]any
}

func (s *Service) doScan(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[scanReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if req.IndexName != "" {
		return nil, errValidation("secondary indexes (IndexName) are not in pulse's local subset yet — scan the base table")
	}
	page, apiErr := s.Scan(req.TableName, req.FilterExpression,
		req.ExpressionAttributeNames, req.ExpressionAttributeValues,
		req.Limit, req.ExclusiveStartKey)
	if apiErr != nil {
		return nil, apiErr
	}
	return pageResponse(page, req.ProjectionExpression, req.ExpressionAttributeNames)
}

func pageResponse(page *pageResult, projection string, names map[string]string) (any, *awsfacade.APIError) {
	items := page.Items
	if projection != "" {
		items = make([]map[string]any, 0, len(page.Items))
		for _, it := range page.Items {
			p, apiErr := applyProjection(it, projection, names)
			if apiErr != nil {
				return nil, apiErr
			}
			items = append(items, p)
		}
	}
	resp := map[string]any{
		"Items":        items,
		"Count":        len(items),
		"ScannedCount": page.ScannedCount,
	}
	if page.LastKey != nil {
		resp["LastEvaluatedKey"] = page.LastKey
	}
	return resp, nil
}

type batchWriteReq struct {
	RequestItems map[string][]struct {
		PutRequest *struct {
			Item map[string]any
		}
		DeleteRequest *struct {
			Key map[string]any
		}
	}
}

func (s *Service) doBatchWrite(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[batchWriteReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	for table, ops := range req.RequestItems {
		for _, op := range ops {
			switch {
			case op.PutRequest != nil:
				if _, apiErr := s.Put(table, op.PutRequest.Item, "", nil, nil, false); apiErr != nil {
					return nil, apiErr
				}
			case op.DeleteRequest != nil:
				if _, apiErr := s.Delete(table, op.DeleteRequest.Key, "", nil, nil, false); apiErr != nil {
					return nil, apiErr
				}
			}
		}
	}
	return map[string]any{"UnprocessedItems": map[string]any{}}, nil
}

type batchGetReq struct {
	RequestItems map[string]struct {
		Keys                     []map[string]any
		ProjectionExpression     string
		ExpressionAttributeNames map[string]string
	}
}

func (s *Service) doBatchGet(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[batchGetReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	responses := map[string]any{}
	for table, spec := range req.RequestItems {
		items := []map[string]any{}
		for _, key := range spec.Keys {
			item, apiErr := s.Get(table, key)
			if apiErr != nil {
				return nil, apiErr
			}
			if item == nil {
				continue
			}
			item, apiErr = applyProjection(item, spec.ProjectionExpression, spec.ExpressionAttributeNames)
			if apiErr != nil {
				return nil, apiErr
			}
			items = append(items, item)
		}
		responses[table] = items
	}
	return map[string]any{"Responses": responses, "UnprocessedKeys": map[string]any{}}, nil
}

type createTableReq struct {
	TableName string
	KeySchema []struct {
		AttributeName string
		KeyType       string // HASH | RANGE
	}
	AttributeDefinitions []struct {
		AttributeName string
		AttributeType string
	}
	GlobalSecondaryIndexes []any
	LocalSecondaryIndexes  []any
}

func (s *Service) doCreateTable(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[createTableReq](body)
	if apiErr != nil {
		return nil, apiErr
	}
	if req.TableName == "" || len(req.KeySchema) == 0 {
		return nil, errValidation("CreateTable requires TableName and KeySchema")
	}
	if len(req.GlobalSecondaryIndexes) > 0 || len(req.LocalSecondaryIndexes) > 0 {
		return nil, errValidation("secondary indexes are not in pulse's local subset yet")
	}
	attrType := map[string]string{}
	for _, ad := range req.AttributeDefinitions {
		attrType[ad.AttributeName] = ad.AttributeType
	}
	def := &tableDef{Name: req.TableName}
	for _, ks := range req.KeySchema {
		typ, ok := attrType[ks.AttributeName]
		if !ok {
			return nil, errValidation("key attribute %q has no AttributeDefinition", ks.AttributeName)
		}
		switch ks.KeyType {
		case "HASH":
			def.PKName, def.PKType = ks.AttributeName, typ
		case "RANGE":
			def.SKName, def.SKType = ks.AttributeName, typ
		}
	}
	if def.PKName == "" {
		return nil, errValidation("KeySchema needs a HASH key")
	}
	if apiErr := s.EnsureDeclared(def); apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{"TableDescription": s.describe(def)}, nil
}

func (s *Service) doDescribeTable(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[struct{ TableName string }](body)
	if apiErr != nil {
		return nil, apiErr
	}
	t := s.table(req.TableName)
	if t == nil {
		return nil, s.errTableNotFound(req.TableName)
	}
	return map[string]any{"Table": s.describe(t)}, nil
}

func (s *Service) doDeleteTable(body []byte) (any, *awsfacade.APIError) {
	req, apiErr := decodeReq[struct{ TableName string }](body)
	if apiErr != nil {
		return nil, apiErr
	}
	t := s.table(req.TableName)
	if t == nil {
		return nil, s.errTableNotFound(req.TableName)
	}
	desc := s.describe(t)
	if apiErr := s.Drop(req.TableName); apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{"TableDescription": desc}, nil
}

func (s *Service) doListTables() (any, *awsfacade.APIError) {
	return map[string]any{"TableNames": s.TableNames()}, nil
}

func (s *Service) describe(t *tableDef) map[string]any {
	keySchema := []map[string]any{{"AttributeName": t.PKName, "KeyType": "HASH"}}
	attrDefs := []map[string]any{{"AttributeName": t.PKName, "AttributeType": t.PKType}}
	if t.SKName != "" {
		keySchema = append(keySchema, map[string]any{"AttributeName": t.SKName, "KeyType": "RANGE"})
		attrDefs = append(attrDefs, map[string]any{"AttributeName": t.SKName, "AttributeType": t.SKType})
	}
	return map[string]any{
		"TableName":            t.Name,
		"TableStatus":          "ACTIVE",
		"KeySchema":            keySchema,
		"AttributeDefinitions": attrDefs,
		"ItemCount":            s.itemCount(t.Name),
		"TableArn":             fmt.Sprintf("arn:aws:dynamodb:local:000000000000:table/%s", t.Name),
	}
}

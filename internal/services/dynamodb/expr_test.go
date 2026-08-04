package dynamodb

import (
	"strings"
	"testing"
)

func av(kind string, v any) map[string]any { return map[string]any{kind: v} }

func testItem() map[string]any {
	return map[string]any{
		"id":     av("S", "u-1"),
		"status": av("S", "pending"),
		"qty":    av("N", "5"),
		"email":  av("S", "geetansh@tartanhq.com"),
		"tags":   av("SS", []any{"a", "b"}),
	}
}

func TestConditionEval(t *testing.T) {
	values := map[string]any{
		":pending": av("S", "pending"),
		":done":    av("S", "done"),
		":three":   av("N", "3"),
		":ten":     av("N", "10"),
		":ge":      av("S", "geetansh"),
		":tagA":    av("S", "a"),
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"status = :pending", true},
		{"status = :done", false},
		{"status <> :done", true},
		{"qty > :three", true},
		{"qty > :ten", false},
		{"qty BETWEEN :three AND :ten", true},
		{"begins_with(email, :ge)", true},
		{"attribute_exists(email)", true},
		{"attribute_exists(missing)", false},
		{"attribute_not_exists(missing)", true},
		{"contains(tags, :tagA)", true},
		{"contains(email, :ge)", true},
		{"status = :pending AND qty > :three", true},
		{"status = :done OR qty > :three", true},
		{"NOT status = :done", true},
		{"(status = :done OR status = :pending) AND qty BETWEEN :three AND :ten", true},
		// Missing attribute comparisons are false, not errors.
		{"missing > :three", false},
		// #name aliasing.
		{"#s = :pending", true},
	}
	names := map[string]string{"#s": "status"}
	for _, tc := range cases {
		cond, err := ParseCondition(tc.expr, names)
		if err != nil {
			t.Errorf("%q: parse error: %v", tc.expr, err)
			continue
		}
		got, err := cond.Eval(testItem(), values)
		if err != nil {
			t.Errorf("%q: eval error: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestConditionRejectsUnsupported(t *testing.T) {
	cases := []struct {
		expr    string
		wantMsg string
	}{
		{"status IN (:a, :b)", "IN"},
		{"size(tags) > :three", "size()"},
		{"meta.owner = :a", "top-level"},
		{"status = 'literal'", "unexpected character"},
		{"#missing = :a", "ExpressionAttributeNames"},
	}
	for _, tc := range cases {
		_, err := ParseCondition(tc.expr, nil)
		if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("%q: error = %v, want mention of %q", tc.expr, err, tc.wantMsg)
		}
	}
}

func TestKeyConditionParsing(t *testing.T) {
	kc, err := ParseKeyCondition("id = :id AND begins_with(sk, :prefix)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(kc.Parts) != 2 || kc.Parts[0].Op != "=" || kc.Parts[1].Op != "begins_with" {
		t.Errorf("parts = %+v", kc.Parts)
	}
	if _, err := ParseKeyCondition("id <> :id", nil); err == nil {
		t.Error("<> should be rejected in key conditions")
	}
}

func TestUpdateApply(t *testing.T) {
	names := map[string]string{"#s": "status"}
	values := map[string]any{
		":done": av("S", "processed"),
		":one":  av("N", "1"),
		":zero": av("N", "0"),
		":tags": av("SS", []any{"b", "c"}),
	}

	u, err := ParseUpdate("SET #s = :done, qty = qty + :one REMOVE email ADD tags :tags", names)
	if err != nil {
		t.Fatal(err)
	}
	out, err := u.Apply(testItem(), values)
	if err != nil {
		t.Fatal(err)
	}
	if !avEqual(out["status"], av("S", "processed")) {
		t.Errorf("status = %v", out["status"])
	}
	if !avEqual(out["qty"], av("N", "6")) {
		t.Errorf("qty = %v", out["qty"])
	}
	if _, exists := out["email"]; exists {
		t.Error("email should have been removed")
	}
	_, tags, _ := avKind(out["tags"])
	if len(tags.([]any)) != 3 {
		t.Errorf("tags after ADD = %v", tags)
	}

	// if_not_exists: existing attr keeps its value, missing attr gets fallback.
	u2, err := ParseUpdate("SET qty = if_not_exists(qty, :zero), retries = if_not_exists(retries, :zero)", nil)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := u2.Apply(testItem(), values)
	if err != nil {
		t.Fatal(err)
	}
	if !avEqual(out2["qty"], av("N", "5")) || !avEqual(out2["retries"], av("N", "0")) {
		t.Errorf("if_not_exists: qty=%v retries=%v", out2["qty"], out2["retries"])
	}

	// ADD on a fresh number attribute starts from zero.
	u3, _ := ParseUpdate("ADD counter :one", nil)
	out3, err := u3.Apply(map[string]any{}, values)
	if err != nil || !avEqual(out3["counter"], av("N", "1")) {
		t.Errorf("ADD fresh counter = %v (%v)", out3["counter"], err)
	}
}

func TestUpdateRejectsUnsupported(t *testing.T) {
	cases := []struct {
		expr    string
		wantMsg string
	}{
		{"SET items = list_append(items, :v)", "list_append"},
		{"DELETE tags :v", "DELETE"},
		{"SET meta.count = :v", "top-level"},
	}
	for _, tc := range cases {
		_, err := ParseUpdate(tc.expr, nil)
		if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("%q: error = %v, want mention of %q", tc.expr, err, tc.wantMsg)
		}
	}
}

func TestAVHelpers(t *testing.T) {
	if !avEqual(av("N", "1.0"), av("N", "1")) {
		t.Error("N equality should be numeric")
	}
	if avEqual(av("S", "1"), av("N", "1")) {
		t.Error("different types are never equal")
	}
	if !avEqual(av("SS", []any{"a", "b"}), av("SS", []any{"b", "a"})) {
		t.Error("string sets compare order-insensitively")
	}
	c, err := avCompare(av("N", "2"), av("N", "10"))
	if err != nil || c >= 0 {
		t.Error("N ordering must be numeric, not lexicographic")
	}
	if !avEqual(
		av("M", map[string]any{"a": av("N", "1")}),
		av("M", map[string]any{"a": av("N", "1.0")})) {
		t.Error("maps compare element-wise")
	}
}

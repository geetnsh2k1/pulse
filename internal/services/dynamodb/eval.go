package dynamodb

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Items travel as decoded JSON: map[attribute]AttributeValue, where an
// AttributeValue is map[string]any like {"S":"x"}, {"N":"3"}, {"BOOL":true},
// {"L":[...]}, {"M":{...}}, {"SS":[...]}, …

func resolveValue(values map[string]any, ref string) (any, error) {
	v, ok := values[ref]
	if !ok {
		return nil, fmt.Errorf("ExpressionAttributeValues is missing %s", ref)
	}
	return v, nil
}

// avKind extracts the type tag and payload of an AttributeValue.
func avKind(av any) (string, any, bool) {
	m, ok := av.(map[string]any)
	if !ok || len(m) == 0 {
		return "", nil, false
	}
	for _, k := range [...]string{"S", "N", "B", "BOOL", "NULL", "L", "M", "SS", "NS", "BS"} {
		if v, ok := m[k]; ok {
			return k, v, true
		}
	}
	return "", nil, false
}

func numOf(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func formatNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// avEqual is DynamoDB equality: same type, same value (N compared
// numerically, sets order-insensitively, L/M element-wise).
func avEqual(a, b any) bool {
	ka, va, oka := avKind(a)
	kb, vb, okb := avKind(b)
	if !oka || !okb || ka != kb {
		return false
	}
	switch ka {
	case "N":
		fa, oa := numOf(va)
		fb, ob := numOf(vb)
		return oa && ob && fa == fb
	case "SS", "NS", "BS":
		return setEqual(va, vb, ka == "NS")
	case "L":
		la, _ := va.([]any)
		lb, _ := vb.([]any)
		if len(la) != len(lb) {
			return false
		}
		for i := range la {
			if !avEqual(la[i], lb[i]) {
				return false
			}
		}
		return true
	case "M":
		ma, _ := va.(map[string]any)
		mb, _ := vb.(map[string]any)
		if len(ma) != len(mb) {
			return false
		}
		for k, v := range ma {
			w, ok := mb[k]
			if !ok || !avEqual(v, w) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(va, vb)
	}
}

func setEqual(a, b any, numeric bool) bool {
	la, _ := a.([]any)
	lb, _ := b.([]any)
	if len(la) != len(lb) {
		return false
	}
	norm := func(l []any) map[string]bool {
		out := map[string]bool{}
		for _, e := range l {
			s, _ := e.(string)
			if numeric {
				if f, ok := numOf(e); ok {
					s = formatNum(f)
				}
			}
			out[s] = true
		}
		return out
	}
	return reflect.DeepEqual(norm(la), norm(lb))
}

// avCompare orders two AttributeValues of the same scalar type
// (S lexicographic, N numeric, B by decoded bytes).
func avCompare(a, b any) (int, error) {
	ka, va, oka := avKind(a)
	kb, vb, okb := avKind(b)
	if !oka || !okb || ka != kb {
		return 0, fmt.Errorf("cannot order values of different types")
	}
	switch ka {
	case "S":
		return strings.Compare(va.(string), vb.(string)), nil
	case "N":
		fa, oa := numOf(va)
		fb, ob := numOf(vb)
		if !oa || !ob {
			return 0, fmt.Errorf("invalid number")
		}
		switch {
		case fa < fb:
			return -1, nil
		case fa > fb:
			return 1, nil
		}
		return 0, nil
	case "B":
		ba, ea := base64.StdEncoding.DecodeString(va.(string))
		bb, eb := base64.StdEncoding.DecodeString(vb.(string))
		if ea != nil || eb != nil {
			return 0, fmt.Errorf("invalid binary value")
		}
		return bytes.Compare(ba, bb), nil
	}
	return 0, fmt.Errorf("type %s is not orderable", ka)
}

func avBeginsWith(av, prefix any) bool {
	k1, v1, ok1 := avKind(av)
	k2, v2, ok2 := avKind(prefix)
	if !ok1 || !ok2 || k1 != k2 {
		return false
	}
	switch k1 {
	case "S":
		return strings.HasPrefix(v1.(string), v2.(string))
	case "B":
		b1, e1 := base64.StdEncoding.DecodeString(v1.(string))
		b2, e2 := base64.StdEncoding.DecodeString(v2.(string))
		return e1 == nil && e2 == nil && bytes.HasPrefix(b1, b2)
	}
	return false
}

func avContains(container, needle any) bool {
	kc, vc, okc := avKind(container)
	if !okc {
		return false
	}
	switch kc {
	case "S":
		kn, vn, okn := avKind(needle)
		if !okn || kn != "S" {
			return false
		}
		return strings.Contains(vc.(string), vn.(string))
	case "SS", "NS", "BS":
		list, _ := vc.([]any)
		_, vn, okn := avKind(needle)
		if !okn {
			return false
		}
		for _, e := range list {
			if kc == "NS" {
				fa, oa := numOf(e)
				fb, ob := numOf(vn)
				if oa && ob && fa == fb {
					return true
				}
				continue
			}
			if e == vn {
				return true
			}
		}
		return false
	case "L":
		list, _ := vc.([]any)
		for _, e := range list {
			if avEqual(e, needle) {
				return true
			}
		}
		return false
	}
	return false
}

// ---- applying updates ----

func (t setTerm) resolve(item map[string]any, values map[string]any) (any, error) {
	switch t.kind {
	case "value":
		return resolveValue(values, t.ref)
	case "path":
		v, ok := item[t.path]
		if !ok {
			return nil, fmt.Errorf("attribute %q does not exist on the item", t.path)
		}
		return v, nil
	case "if_not_exists":
		if v, ok := item[t.path]; ok {
			return v, nil
		}
		if t.fbKind == "value" {
			return resolveValue(values, t.fbRef)
		}
		v, ok := item[t.fbPath]
		if !ok {
			return nil, fmt.Errorf("if_not_exists fallback attribute %q does not exist", t.fbPath)
		}
		return v, nil
	}
	return nil, fmt.Errorf("internal: unknown set term")
}

// Apply produces the updated copy of item.
func (u *Update) Apply(item map[string]any, values map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range item {
		out[k] = v
	}

	for _, s := range u.sets {
		av, err := s.a.resolve(out, values)
		if err != nil {
			return nil, err
		}
		if s.op != 0 {
			bv, err := s.b.resolve(out, values)
			if err != nil {
				return nil, err
			}
			ka, va, _ := avKind(av)
			kb, vb, _ := avKind(bv)
			if ka != "N" || kb != "N" {
				return nil, fmt.Errorf("SET arithmetic (+/-) works on numbers only")
			}
			fa, _ := numOf(va)
			fb, _ := numOf(vb)
			if s.op == '-' {
				fb = -fb
			}
			av = map[string]any{"N": formatNum(fa + fb)}
		}
		out[s.path] = av
	}

	for _, path := range u.removes {
		delete(out, path)
	}

	for _, a := range u.adds {
		val, err := resolveValue(values, a.ref)
		if err != nil {
			return nil, err
		}
		kv, vv, ok := avKind(val)
		if !ok {
			return nil, fmt.Errorf("ADD value for %q is not a valid attribute value", a.path)
		}
		existing, exists := out[a.path]
		switch kv {
		case "N":
			fv, _ := numOf(vv)
			base := 0.0
			if exists {
				ke, ve, _ := avKind(existing)
				if ke != "N" {
					return nil, fmt.Errorf("ADD on %q: existing attribute is not a number", a.path)
				}
				base, _ = numOf(ve)
			}
			out[a.path] = map[string]any{"N": formatNum(base + fv)}
		case "SS", "NS", "BS":
			addList, _ := vv.([]any)
			var merged []any
			if exists {
				ke, ve, _ := avKind(existing)
				if ke != kv {
					return nil, fmt.Errorf("ADD on %q: set type mismatch (%s vs %s)", a.path, ke, kv)
				}
				merged, _ = ve.([]any)
			}
			for _, e := range addList {
				dup := false
				for _, m := range merged {
					if m == e {
						dup = true
						break
					}
				}
				if !dup {
					merged = append(merged, e)
				}
			}
			out[a.path] = map[string]any{kv: merged}
		default:
			return nil, fmt.Errorf("ADD supports numbers and sets only (got %s)", kv)
		}
	}
	return out, nil
}

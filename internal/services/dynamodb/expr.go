// Expression-language subset for pulse's local DynamoDB.
//
// Supported — conditions/filters: =, <>, <, <=, >, >=, BETWEEN,
// begins_with(p, :v), attribute_exists(p), attribute_not_exists(p),
// contains(p, :v), AND, OR, NOT, parentheses, #name and :value aliases.
// Updates: SET p = :v (with + / - arithmetic and if_not_exists), REMOVE,
// ADD (numbers and sets). Top-level attribute paths only.
//
// Anything outside the subset fails loudly with a message saying so —
// never silently wrong.
package dynamodb

import (
	"fmt"
	"strings"
	"unicode"
)

// ---- lexer ----

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tName  // #alias
	tValue // :alias
	tOp    // = <> < <= > >=
	tLParen
	tRParen
	tComma
	tPlus
	tMinus
	tDot
	tLBracket
)

type token struct {
	kind tokKind
	text string
}

func lex(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			out = append(out, token{tLParen, "("})
			i++
		case c == ')':
			out = append(out, token{tRParen, ")"})
			i++
		case c == ',':
			out = append(out, token{tComma, ","})
			i++
		case c == '+':
			out = append(out, token{tPlus, "+"})
			i++
		case c == '-':
			out = append(out, token{tMinus, "-"})
			i++
		case c == '.':
			out = append(out, token{tDot, "."})
			i++
		case c == '[':
			out = append(out, token{tLBracket, "["})
			i++
		case c == '=':
			out = append(out, token{tOp, "="})
			i++
		case c == '<':
			switch {
			case strings.HasPrefix(s[i:], "<>"):
				out = append(out, token{tOp, "<>"})
				i += 2
			case strings.HasPrefix(s[i:], "<="):
				out = append(out, token{tOp, "<="})
				i += 2
			default:
				out = append(out, token{tOp, "<"})
				i++
			}
		case c == '>':
			if strings.HasPrefix(s[i:], ">=") {
				out = append(out, token{tOp, ">="})
				i += 2
			} else {
				out = append(out, token{tOp, ">"})
				i++
			}
		case c == '#' || c == ':':
			j := i + 1
			for j < len(s) && (isIdentChar(rune(s[j]))) {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("dangling %q in expression", string(c))
			}
			kind := tName
			if c == ':' {
				kind = tValue
			}
			out = append(out, token{kind, s[i:j]})
			i = j
		case isIdentStart(rune(c)):
			j := i
			for j < len(s) && isIdentChar(rune(s[j])) {
				j++
			}
			out = append(out, token{tIdent, s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q in expression", string(c))
		}
	}
	return append(out, token{tEOF, ""}), nil
}

func isIdentStart(r rune) bool { return unicode.IsLetter(r) || r == '_' }
func isIdentChar(r rune) bool  { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// ---- parser plumbing ----

type parser struct {
	toks  []token
	pos   int
	names map[string]string // #alias → real attribute name
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atEOF() bool { return p.peek().kind == tEOF }
func (p *parser) isKw(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

// path parses a single top-level attribute reference; nested paths are
// outside the subset and produce the explanatory error.
func (p *parser) path() (string, error) {
	t := p.next()
	var name string
	switch t.kind {
	case tIdent:
		name = t.text
	case tName:
		real, ok := p.names[t.text]
		if !ok {
			return "", fmt.Errorf("ExpressionAttributeNames is missing %s", t.text)
		}
		name = real
	default:
		return "", fmt.Errorf("expected an attribute name, got %q", t.text)
	}
	if p.peek().kind == tDot || p.peek().kind == tLBracket {
		return "", fmt.Errorf("pulse supports top-level attribute paths only — nested paths like %q are not in the local subset yet", name+"."+"…")
	}
	return name, nil
}

func (p *parser) valueRef() (string, error) {
	t := p.next()
	if t.kind != tValue {
		return "", fmt.Errorf("expected a :value placeholder, got %q (literals are not valid in DynamoDB expressions)", t.text)
	}
	return t.text, nil
}

// ---- condition AST ----

type condNode interface {
	eval(item map[string]any, values map[string]any) (bool, error)
}

type nBool struct {
	op   string // "AND" | "OR"
	l, r condNode
}

func (n nBool) eval(item, values map[string]any) (bool, error) {
	lv, err := n.l.eval(item, values)
	if err != nil {
		return false, err
	}
	if n.op == "AND" && !lv {
		return false, nil
	}
	if n.op == "OR" && lv {
		return true, nil
	}
	return n.r.eval(item, values)
}

type nNot struct{ c condNode }

func (n nNot) eval(item, values map[string]any) (bool, error) {
	v, err := n.c.eval(item, values)
	return !v, err
}

type nCompare struct {
	path, op, ref string
}

func (n nCompare) eval(item, values map[string]any) (bool, error) {
	want, err := resolveValue(values, n.ref)
	if err != nil {
		return false, err
	}
	have, ok := item[n.path]
	if !ok {
		return false, nil // missing attribute: comparisons are simply false
	}
	if n.op == "=" || n.op == "<>" {
		eq := avEqual(have, want)
		if n.op == "=" {
			return eq, nil
		}
		return !eq, nil
	}
	cmp, err := avCompare(have, want)
	if err != nil {
		return false, nil // unorderable / mixed types: false, like DynamoDB
	}
	switch n.op {
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	}
	return false, fmt.Errorf("unknown comparator %q", n.op)
}

type nBetween struct{ path, lo, hi string }

func (n nBetween) eval(item, values map[string]any) (bool, error) {
	have, ok := item[n.path]
	if !ok {
		return false, nil
	}
	lo, err := resolveValue(values, n.lo)
	if err != nil {
		return false, err
	}
	hi, err := resolveValue(values, n.hi)
	if err != nil {
		return false, err
	}
	c1, err1 := avCompare(have, lo)
	c2, err2 := avCompare(have, hi)
	if err1 != nil || err2 != nil {
		return false, nil
	}
	return c1 >= 0 && c2 <= 0, nil
}

type nFunc struct {
	name, path, ref string
}

func (n nFunc) eval(item, values map[string]any) (bool, error) {
	have, exists := item[n.path]
	switch n.name {
	case "attribute_exists":
		return exists, nil
	case "attribute_not_exists":
		return !exists, nil
	case "begins_with":
		if !exists {
			return false, nil
		}
		want, err := resolveValue(values, n.ref)
		if err != nil {
			return false, err
		}
		return avBeginsWith(have, want), nil
	case "contains":
		if !exists {
			return false, nil
		}
		want, err := resolveValue(values, n.ref)
		if err != nil {
			return false, err
		}
		return avContains(have, want), nil
	}
	return false, fmt.Errorf("unknown function %q", n.name)
}

// Condition is a parsed ConditionExpression / FilterExpression.
type Condition struct{ root condNode }

// ParseCondition compiles the supported subset; names maps #aliases.
func ParseCondition(src string, names map[string]string) (*Condition, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, names: names}
	root, err := p.orExpr()
	if err != nil {
		return nil, err
	}
	if !p.atEOF() {
		return nil, fmt.Errorf("unexpected %q after the end of the expression", p.peek().text)
	}
	return &Condition{root: root}, nil
}

// Eval runs the condition against an item.
func (c *Condition) Eval(item map[string]any, values map[string]any) (bool, error) {
	if item == nil {
		item = map[string]any{}
	}
	return c.root.eval(item, values)
}

func (p *parser) orExpr() (condNode, error) {
	l, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for p.isKw("OR") {
		p.next()
		r, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		l = nBool{op: "OR", l: l, r: r}
	}
	return l, nil
}

func (p *parser) andExpr() (condNode, error) {
	l, err := p.notExpr()
	if err != nil {
		return nil, err
	}
	for p.isKw("AND") {
		p.next()
		r, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		l = nBool{op: "AND", l: l, r: r}
	}
	return l, nil
}

func (p *parser) notExpr() (condNode, error) {
	if p.isKw("NOT") {
		p.next()
		c, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		return nNot{c: c}, nil
	}
	return p.primary()
}

var condFuncs = map[string]bool{
	"attribute_exists": true, "attribute_not_exists": true,
	"begins_with": true, "contains": true,
}

// unsupported constructs get named, explanatory rejections.
var knownUnsupported = map[string]string{
	"size":           "size()",
	"attribute_type": "attribute_type()",
	"list_append":    "list_append()",
	"contains_any":   "contains_any()",
}

func (p *parser) primary() (condNode, error) {
	if p.peek().kind == tLParen {
		p.next()
		inner, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tRParen {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.next()
		return inner, nil
	}

	// Function call?
	if t := p.peek(); t.kind == tIdent && p.toks[p.pos+1].kind == tLParen {
		fn := strings.ToLower(t.text)
		if hint, bad := knownUnsupported[fn]; bad {
			return nil, fmt.Errorf("%s is not in pulse's local expression subset yet — see docs/GUIDE.md for what is supported", hint)
		}
		if !condFuncs[fn] {
			return nil, fmt.Errorf("unknown function %q in condition", t.text)
		}
		p.next() // fn
		p.next() // (
		path, err := p.path()
		if err != nil {
			return nil, err
		}
		node := nFunc{name: fn, path: path}
		if fn == "begins_with" || fn == "contains" {
			if p.peek().kind != tComma {
				return nil, fmt.Errorf("%s needs two arguments: (path, :value)", fn)
			}
			p.next()
			ref, err := p.valueRef()
			if err != nil {
				return nil, err
			}
			node.ref = ref
		}
		if p.peek().kind != tRParen {
			return nil, fmt.Errorf("missing closing parenthesis after %s(...)", fn)
		}
		p.next()
		return node, nil
	}

	// path <comparator> :v | path BETWEEN :a AND :b | path IN (...)
	path, err := p.path()
	if err != nil {
		return nil, err
	}
	if p.isKw("BETWEEN") {
		p.next()
		lo, err := p.valueRef()
		if err != nil {
			return nil, err
		}
		if !p.isKw("AND") {
			return nil, fmt.Errorf("BETWEEN needs the form: path BETWEEN :low AND :high")
		}
		p.next()
		hi, err := p.valueRef()
		if err != nil {
			return nil, err
		}
		return nBetween{path: path, lo: lo, hi: hi}, nil
	}
	if p.isKw("IN") {
		return nil, fmt.Errorf("IN (...) is not in pulse's local expression subset yet — rewrite as OR-ed equality checks")
	}
	if p.peek().kind != tOp {
		return nil, fmt.Errorf("expected a comparator after %q", path)
	}
	op := p.next().text
	ref, err := p.valueRef()
	if err != nil {
		return nil, err
	}
	return nCompare{path: path, op: op, ref: ref}, nil
}

// ---- key conditions (Query) ----

// KeyCond is the restricted grammar of KeyConditionExpression:
// pk = :v [AND sk <op> :v | begins_with(sk, :v) | sk BETWEEN :a AND :b]
type KeyCond struct {
	Parts []KeyCondPart
}

type KeyCondPart struct {
	Name string
	Op   string // "=", "<", "<=", ">", ">=", "begins_with", "between"
	Ref1 string
	Ref2 string // between only
}

// ParseKeyCondition compiles a KeyConditionExpression.
func ParseKeyCondition(src string, names map[string]string) (*KeyCond, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, names: names}
	kc := &KeyCond{}
	for {
		part, err := p.keyCondPart()
		if err != nil {
			return nil, err
		}
		kc.Parts = append(kc.Parts, *part)
		if p.isKw("AND") {
			p.next()
			continue
		}
		break
	}
	if !p.atEOF() {
		return nil, fmt.Errorf("unexpected %q in KeyConditionExpression", p.peek().text)
	}
	if len(kc.Parts) > 2 {
		return nil, fmt.Errorf("a key condition can reference at most the partition key and the sort key")
	}
	return kc, nil
}

func (p *parser) keyCondPart() (*KeyCondPart, error) {
	if t := p.peek(); t.kind == tIdent && strings.EqualFold(t.text, "begins_with") {
		p.next()
		if p.peek().kind != tLParen {
			return nil, fmt.Errorf("begins_with needs the form begins_with(path, :value)")
		}
		p.next()
		path, err := p.path()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tComma {
			return nil, fmt.Errorf("begins_with needs two arguments")
		}
		p.next()
		ref, err := p.valueRef()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tRParen {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.next()
		return &KeyCondPart{Name: path, Op: "begins_with", Ref1: ref}, nil
	}

	path, err := p.path()
	if err != nil {
		return nil, err
	}
	if p.isKw("BETWEEN") {
		p.next()
		lo, err := p.valueRef()
		if err != nil {
			return nil, err
		}
		if !p.isKw("AND") {
			return nil, fmt.Errorf("BETWEEN needs the form: path BETWEEN :low AND :high")
		}
		p.next()
		hi, err := p.valueRef()
		if err != nil {
			return nil, err
		}
		return &KeyCondPart{Name: path, Op: "between", Ref1: lo, Ref2: hi}, nil
	}
	if p.peek().kind != tOp {
		return nil, fmt.Errorf("expected a comparator after %q in KeyConditionExpression", path)
	}
	op := p.next().text
	if op == "<>" {
		return nil, fmt.Errorf("<> is not valid in a key condition")
	}
	ref, err := p.valueRef()
	if err != nil {
		return nil, err
	}
	return &KeyCondPart{Name: path, Op: op, Ref1: ref}, nil
}

// ---- update expressions ----

type setTerm struct {
	kind string // "value" | "path" | "if_not_exists"
	ref  string // value ref
	path string
	// if_not_exists fallback
	fbKind string // "value" | "path"
	fbRef  string
	fbPath string
}

type setAction struct {
	path string
	a    setTerm
	op   byte // 0, '+', '-'
	b    setTerm
}

type addAction struct {
	path string
	ref  string
}

// Update is a parsed UpdateExpression.
type Update struct {
	sets    []setAction
	removes []string
	adds    []addAction
}

// ParseUpdate compiles the SET/REMOVE/ADD subset.
func ParseUpdate(src string, names map[string]string) (*Update, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, names: names}
	u := &Update{}
	for !p.atEOF() {
		switch {
		case p.isKw("SET"):
			p.next()
			for {
				act, err := p.setAction()
				if err != nil {
					return nil, err
				}
				u.sets = append(u.sets, *act)
				if p.peek().kind == tComma {
					p.next()
					continue
				}
				break
			}
		case p.isKw("REMOVE"):
			p.next()
			for {
				path, err := p.path()
				if err != nil {
					return nil, err
				}
				u.removes = append(u.removes, path)
				if p.peek().kind == tComma {
					p.next()
					continue
				}
				break
			}
		case p.isKw("ADD"):
			p.next()
			for {
				path, err := p.path()
				if err != nil {
					return nil, err
				}
				ref, err := p.valueRef()
				if err != nil {
					return nil, err
				}
				u.adds = append(u.adds, addAction{path: path, ref: ref})
				if p.peek().kind == tComma {
					p.next()
					continue
				}
				break
			}
		case p.isKw("DELETE"):
			return nil, fmt.Errorf("the DELETE update clause (set-element removal) is not in pulse's local subset yet — use SET/REMOVE/ADD")
		default:
			return nil, fmt.Errorf("expected SET, REMOVE, or ADD, got %q", p.peek().text)
		}
	}
	if len(u.sets)+len(u.removes)+len(u.adds) == 0 {
		return nil, fmt.Errorf("empty update expression")
	}
	return u, nil
}

func (p *parser) setAction() (*setAction, error) {
	path, err := p.path()
	if err != nil {
		return nil, err
	}
	if !(p.peek().kind == tOp && p.peek().text == "=") {
		return nil, fmt.Errorf("SET needs the form: attribute = value")
	}
	p.next()
	a, err := p.setTerm()
	if err != nil {
		return nil, err
	}
	act := &setAction{path: path, a: *a}
	if k := p.peek().kind; k == tPlus || k == tMinus {
		act.op = p.next().text[0]
		b, err := p.setTerm()
		if err != nil {
			return nil, err
		}
		act.b = *b
	}
	return act, nil
}

func (p *parser) setTerm() (*setTerm, error) {
	t := p.peek()
	switch {
	case t.kind == tValue:
		p.next()
		return &setTerm{kind: "value", ref: t.text}, nil
	case t.kind == tIdent && strings.EqualFold(t.text, "if_not_exists"):
		p.next()
		if p.peek().kind != tLParen {
			return nil, fmt.Errorf("if_not_exists needs the form if_not_exists(path, fallback)")
		}
		p.next()
		path, err := p.path()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tComma {
			return nil, fmt.Errorf("if_not_exists needs two arguments")
		}
		p.next()
		term := &setTerm{kind: "if_not_exists", path: path}
		fb := p.peek()
		switch fb.kind {
		case tValue:
			p.next()
			term.fbKind, term.fbRef = "value", fb.text
		case tIdent, tName:
			fbPath, err := p.path()
			if err != nil {
				return nil, err
			}
			term.fbKind, term.fbPath = "path", fbPath
		default:
			return nil, fmt.Errorf("if_not_exists fallback must be a :value or an attribute")
		}
		if p.peek().kind != tRParen {
			return nil, fmt.Errorf("missing closing parenthesis after if_not_exists(...)")
		}
		p.next()
		return term, nil
	case t.kind == tIdent && strings.EqualFold(t.text, "list_append"):
		return nil, fmt.Errorf("list_append() is not in pulse's local subset yet — read-modify-write the list in your code instead")
	case t.kind == tIdent || t.kind == tName:
		path, err := p.path()
		if err != nil {
			return nil, err
		}
		return &setTerm{kind: "path", path: path}, nil
	}
	return nil, fmt.Errorf("expected a :value, attribute, or if_not_exists(...) in SET, got %q", t.text)
}

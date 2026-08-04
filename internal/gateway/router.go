// Package gateway is pulse's local API front door: an HTTP server that maps
// requests onto http triggers, hands functions AWS-shaped proxy events, and
// maps their return values back onto real HTTP responses.
package gateway

import (
	"strings"

	"pulse/internal/config"
)

type Router struct {
	routes []*route
}

type route struct {
	method   string // GET, POST, ... or ANY
	rawPath  string // the declared template, e.g. /orders/{id}
	segs     []seg
	fn       string
	format   string // "2.0" | "1.0"
	literals int
	greedy   bool
}

type seg struct {
	literal string
	param   string
	greedy  bool
}

// Match describes which function a request landed on.
type Match struct {
	Function string
	RouteKey string // "POST /orders/{id}" — stamped into the proxy event
	Resource string // the raw path template
	Format   string
	Params   map[string]string
}

// RouteInfo is the display/API view of one route.
type RouteInfo struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Function string `json:"function"`
	Format   string `json:"payloadFormat"`
}

func NewRouter(cfg *config.Config) *Router {
	rt := &Router{}
	for _, t := range cfg.Triggers {
		if t.Type != "http" {
			continue
		}
		r := &route{method: t.Method, rawPath: t.Path, fn: t.Function, format: t.PayloadFormat}
		for _, s := range splitPath(t.Path) {
			switch {
			case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "+}"):
				r.segs = append(r.segs, seg{param: s[1 : len(s)-2], greedy: true})
				r.greedy = true
			case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
				r.segs = append(r.segs, seg{param: s[1 : len(s)-1]})
			default:
				r.segs = append(r.segs, seg{literal: s})
				r.literals++
			}
		}
		rt.routes = append(rt.routes, r)
	}
	return rt
}

func (rt *Router) Routes() []RouteInfo {
	out := make([]RouteInfo, 0, len(rt.routes))
	for _, r := range rt.routes {
		out = append(out, RouteInfo{Method: r.method, Path: r.rawPath, Function: r.fn, Format: r.format})
	}
	return out
}

// Match picks the winning route for a request, mirroring API Gateway
// precedence: exact method beats ANY, more literal segments beat fewer,
// non-greedy beats greedy. Returns false when nothing matches.
func (rt *Router) Match(method, path string) (*Match, bool) {
	pathSegs := splitPath(path)

	var best *route
	var bestParams map[string]string
	var bestRank [3]int

	for _, r := range rt.routes {
		mrank := 0
		switch {
		case r.method == method:
		case r.method == "ANY":
			mrank = 1
		default:
			continue
		}
		params, ok := r.match(pathSegs)
		if !ok {
			continue
		}
		grank := 0
		if r.greedy {
			grank = 1
		}
		rank := [3]int{mrank, grank, -r.literals}
		if best == nil || rankLess(rank, bestRank) {
			best, bestParams, bestRank = r, params, rank
		}
	}
	if best == nil {
		return nil, false
	}
	return &Match{
		Function: best.fn,
		RouteKey: best.method + " " + best.rawPath,
		Resource: best.rawPath,
		Format:   best.format,
		Params:   bestParams,
	}, true
}

func (r *route) match(pathSegs []string) (map[string]string, bool) {
	var params map[string]string
	capture := func(name, value string) {
		if params == nil {
			params = map[string]string{}
		}
		params[name] = value
	}

	if r.greedy {
		need := len(r.segs) - 1
		if len(pathSegs) < need+1 {
			return nil, false // greedy segment must consume at least one segment
		}
		for i := 0; i < need; i++ {
			if !matchSeg(r.segs[i], pathSegs[i], capture) {
				return nil, false
			}
		}
		capture(r.segs[need].param, strings.Join(pathSegs[need:], "/"))
		return params, true
	}

	if len(pathSegs) != len(r.segs) {
		return nil, false
	}
	for i, s := range r.segs {
		if !matchSeg(s, pathSegs[i], capture) {
			return nil, false
		}
	}
	return params, true
}

func matchSeg(s seg, value string, capture func(name, value string)) bool {
	if s.param != "" {
		capture(s.param, value)
		return true
	}
	return s.literal == value
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func rankLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

package config

import "fmt"

// DidYouMean returns ` (did you mean "X"?)` when a close match exists, else "".
// Exported because a mistyped name is a mistyped name wherever it appears —
// a runtime in pulse.yaml, or a Lambda name passed to `pulse import aws`.
func DidYouMean(input string, candidates []string) string {
	if s := closest(input, candidates); s != "" {
		return fmt.Sprintf(" (did you mean %q?)", s)
	}
	return ""
}

// closest returns the candidate within edit distance 2, preferring the nearest.
func closest(input string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(input, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance; inputs here are short config
// identifiers, so the simple O(len·len) table is plenty.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

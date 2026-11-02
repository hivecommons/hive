package toolapprove

import (
	"math"
	"strconv"
	"strings"
)

// braceExpand expands the unquoted brace expressions in w the way bash
// does, into at most maxBraceWords words.
func (a *rmAnalysis) braceExpand(w []shUnit, depth int) [][]shUnit {
	if depth > maxBraceDepth {
		a.overBound = true
	}
	if a.overBound {
		return [][]shUnit{w}
	}
	open, close, alts := findBraceExpression(w)
	if open < 0 {
		return [][]shUnit{w}
	}
	if alts == nil {
		if alts = a.braceSequence(w[open+1 : close]); a.overBound {
			return [][]shUnit{w}
		}
	}
	suffixes := a.braceExpand(w[close+1:], depth+1)
	var out [][]shUnit
	for _, alt := range alts {
		for _, mid := range a.braceExpand(alt, depth+1) {
			for _, suf := range suffixes {
				n := open + len(mid) + len(suf)
				if len(out) == maxBraceWords || !a.charge(n) {
					a.overBound = true
					return [][]shUnit{w}
				}
				word := make([]shUnit, 0, n)
				word = append(append(append(word, w[:open]...), mid...), suf...)
				out = append(out, word)
			}
		}
	}
	return out
}

// findBraceExpression returns the outermost leftmost brace expression in w:
// the indexes of its { and }, and its comma-separated alternatives, or nil
// alternatives for a {x..y} candidate sequence. open is -1 when w has none.
func findBraceExpression(w []shUnit) (open, close int, alts [][]shUnit) {
	type group struct {
		open   int
		commas []int
	}
	var stack []group
	open, close = -1, -1
	for i, u := range w {
		if u.kind != uLit {
			continue
		}
		switch u.c {
		case '{':
			stack = append(stack, group{open: i})
		case ',':
			if len(stack) > 0 {
				stack[len(stack)-1].commas = append(stack[len(stack)-1].commas, i)
			}
		case '}':
			if len(stack) == 0 {
				continue
			}
			g := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if open >= 0 && g.open > open {
				continue
			}
			if len(g.commas) == 0 && !isBraceSequence(w[g.open+1:i]) {
				continue
			}
			open, close, alts = g.open, i, nil
			if len(g.commas) > 0 {
				start := g.open + 1
				for _, c := range g.commas {
					alts = append(alts, w[start:c])
					start = c + 1
				}
				alts = append(alts, w[start:i])
			}
		}
	}
	return open, close, alts
}

// parseBraceSequence splits a {x..y[..incr]} body into its fields.
func parseBraceSequence(body []shUnit) (x, y string, incr int64, ok bool) {
	s, lit := literalWord(body)
	if !lit {
		return "", "", 0, false
	}
	for _, u := range body {
		if u.kind != uLit {
			return "", "", 0, false
		}
	}
	parts := strings.Split(s, "..")
	if len(parts) != 2 && len(parts) != 3 {
		return "", "", 0, false
	}
	incr = 1
	if len(parts) == 3 {
		n, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return "", "", 0, false
		}
		incr = n
	}
	x, y = parts[0], parts[1]
	_, ex := strconv.ParseInt(x, 10, 64)
	_, ey := strconv.ParseInt(y, 10, 64)
	if ex == nil && ey == nil {
		return x, y, incr, true
	}
	if len(x) == 1 && len(y) == 1 && isASCIILetter(x[0]) && isASCIILetter(y[0]) {
		return x, y, incr, true
	}
	return "", "", 0, false
}

func isBraceSequence(body []shUnit) bool {
	_, _, _, ok := parseBraceSequence(body)
	return ok
}

// braceSequence returns the words of a valid {x..y[..incr]} body, refusing
// (overBound) to produce more than maxBraceWords.
func (a *rmAnalysis) braceSequence(body []shUnit) [][]shUnit {
	x, y, incr, _ := parseBraceSequence(body)
	if incr < 0 {
		incr = -incr
	}
	if incr == 0 {
		incr = 1
	}
	var from, to int64
	width := 0
	char := !isASCIIDigit(x[len(x)-1])
	if char {
		from, to = int64(x[0]), int64(y[0])
	} else {
		from, _ = strconv.ParseInt(x, 10, 64)
		to, _ = strconv.ParseInt(y, 10, 64)
		if zeroPadded(x) || zeroPadded(y) {
			width = max(len(x), len(y))
		}
	}
	// Unsigned arithmetic: to-from and -incr overflow int64 for extreme
	// bounds, which would let the guard pass and the loop below run
	// forever.
	var span uint64
	if to >= from {
		span = uint64(to) - uint64(from)
	} else {
		span = uint64(from) - uint64(to)
	}
	if incr == math.MinInt64 || span/uint64(incr) >= maxBraceWords {
		a.overBound = true
		return nil
	}
	count := span/uint64(incr) + 1
	step := incr
	if to < from {
		step = -incr
	}
	var out [][]shUnit
	v := from
	for n := uint64(0); n < count; n, v = n+1, v+step {
		var s string
		if char {
			s = string(rune(v))
		} else {
			s = strconv.FormatInt(v, 10)
			if neg := strings.HasPrefix(s, "-"); len(s) < width {
				digits := strings.TrimPrefix(s, "-")
				pad := width - len(digits)
				if neg {
					pad--
				}
				s = strings.Repeat("0", max(pad, 0)) + digits
				if neg {
					s = "-" + s
				}
			}
		}
		word := make([]shUnit, len(s))
		for i := range len(s) {
			word[i] = shUnit{c: s[i], kind: uQuoted}
		}
		out = append(out, word)
	}
	return out
}

func zeroPadded(s string) bool {
	s = strings.TrimPrefix(s, "-")
	return len(s) > 1 && s[0] == '0'
}

// Package textutil provides rune-aware string truncation helpers.
package textutil

const ellipsis = "..."

// Head keeps the first n runes of s and appends an ellipsis when s is longer.
func Head(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + ellipsis
}

// Tail keeps the last n runes of s and prepends an ellipsis when s is longer.
func Tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return ellipsis + string(runes[len(runes)-n:])
}

// Runes hard-cuts s to at most max runes with no ellipsis. A max <= 0 returns
// s unchanged.
func Runes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

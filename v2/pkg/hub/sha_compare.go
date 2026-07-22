package hub

import "strings"

// sameCommit reports whether two git SHAs refer to the same commit even when
// one is a short (e.g. 7-char) prefix of the other. The hub stores 7-char
// short SHAs (candidateSHA[:shortSHALen]) while a spoke may report a longer
// gitShort — a raw != comparison then loops the spoke through endless
// "upgrade" instructions to a version it is already running. Comparing on the
// shorter length fixes that. Empty inputs are never equal (unknown state must
// not be treated as up-to-date).
func sameCommit(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}

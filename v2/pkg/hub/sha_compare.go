package hub

import "strings"

// StandardSHALen is the canonical short-SHA length used across hive: the hub
// stores branch SHAs as Commit.SHA[:StandardSHALen], spoke and hub binaries
// build gitShort with `git rev-parse --short=7`, and the hub normalizes any
// SHA a spoke reports to this length on ingest. Standardizing the length is
// what removes short-vs-full mismatch at the source (sameCommit remains a
// defensive backstop for any SHA that slips through un-normalized).
const StandardSHALen = 7

// shortSHA normalizes a git SHA to the canonical short length. A shorter or
// empty value is returned unchanged (never padded); a longer one is truncated.
func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= StandardSHALen {
		return s
	}
	return s[:StandardSHALen]
}

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

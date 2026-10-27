package review

import "strings"

// MinAbbreviatedSHALen is the shortest head SHA a reviewer may report and
// still be matched to a full one. Seven hex digits is what `git rev-parse
// --short` and GitHub's UI print, and what models copy from a PR page.
const MinAbbreviatedSHALen = 7

// fullSHALen is the length of a full git object id (SHA-1).
const fullSHALen = 40

// SameHeadSHA reports whether a and b name the same head. Besides the exact
// (trimmed, case-folded) match, an abbreviated SHA of at least
// MinAbbreviatedSHALen hex digits matches the full SHA it prefixes. Reviewer
// agents report `head_sha` from a PR page or `git rev-parse --short`, so the
// artifact ends up keyed by "98489e7" while every lane looks it up by the
// 40-character head GitHub returns; an exact compare left such PRs at
// "no-verdict" forever even though the swarm had answered the current head.
// Two blank values compare equal, as the exact compare it replaces did.
func SameHeadSHA(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) < MinAbbreviatedSHALen || !isHex(short) {
		return false
	}
	return strings.HasPrefix(long, short)
}

// HeadSHAMatchesOrBlank is SameHeadSHA with the historical blank tolerance:
// a side that recorded no head constrains nothing.
func HeadSHAMatchesOrBlank(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return true
	}
	return SameHeadSHA(a, b)
}

// CanonicalHeadSHA returns full when short abbreviates it (SameHeadSHA) and
// full is the longer spelling; otherwise short unchanged. The relay uses it
// to store the 40-character head the lanes query by, whatever the reviewer
// typed.
func CanonicalHeadSHA(short, full string) string {
	s, f := strings.TrimSpace(short), strings.TrimSpace(full)
	if f == "" || len(f) <= len(s) || !SameHeadSHA(s, f) {
		return s
	}
	return f
}

// IsAbbreviatedSHA reports whether sha is a hex object id shorter than a
// full one (and at least MinAbbreviatedSHALen digits).
func IsAbbreviatedSHA(sha string) bool {
	s := strings.TrimSpace(sha)
	return len(s) >= MinAbbreviatedSHALen && len(s) < fullSHALen && isHex(s)
}

// IsFullHexSHA reports whether sha is a full 40-digit hex object id.
func IsFullHexSHA(sha string) bool {
	s := strings.TrimSpace(sha)
	return len(s) == fullSHALen && isHex(s)
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return s != ""
}

package intent

import (
	"regexp"
	"strings"
)

// The action-sha-ref kind: a bare commit line that moves in lockstep with a
// `uses:` pin the same pull request bumps.
//
// A workflow that pins a reusable workflow by commit often passes the same
// commit on as an input, so the code that workflow checks out is the code it
// was written against:
//
//	uses: some-org/shared/.github/workflows/test.yml@<sha> # v1
//	with:
//	  test_ref: <sha> # v1
//
// A dependency bot bumps both lines together. The second line is not a pin
// by itself: nothing in it names a repository, so nothing could be resolved
// upstream for it. What makes it safe to admit is that it carries exactly the
// move (old commit to new commit) of a `uses:` pin the same diff changes.
// That pin is returned to the caller and resolved upstream as always, so the
// companion line can only ever follow a commit that has passed every
// action-sha check. It adds no pin and no provenance surface of its own.
//
// The rule fails closed: a candidate that no accepted `uses:` move vouches
// for refuses the whole diff, exactly as any other non-pin line does.

// pinSHARefRE is one `key: <40-hex>` line with an optional `# vN[.N[.N]]`
// comment. The key is a plain block-mapping key (no list dash, no quoting, no
// flow syntax) and the commit is the whole unquoted value. A comment must be
// set off by whitespace: YAML reads `<sha># v1` as one scalar, not a commit
// followed by a comment, so that form is not a bare commit line at all.
var pinSHARefRE = regexp.MustCompile(`^([ \t]*[A-Za-z_][A-Za-z0-9_-]*: )([0-9a-fA-F]{40})([ \t]*|[ \t]+# v[0-9]+(?:\.[0-9]+){0,2})$`)

// pinSHAMove is one commit replacement, both commits lowercased.
type pinSHAMove struct {
	old, next string
}

// pinSHARefCandidate is a companion line awaiting a `uses:` pin with the
// same move, and the file it is in (for the refusal reason).
type pinSHARefCandidate struct {
	file string
	move pinSHAMove
}

// pinSHARefMove reports whether old -> next has the shape of a companion
// line and returns its move. It is only a candidate: the caller must still
// find an accepted action-sha replacement with the same move
// (pinSHARefsUnmatched). It is called only for a pair every line kind
// declined, and repeats the unsafe-word check rather than rely on the caller.
//
// Both lines must match pinSHARefRE with a byte-identical key prefix
// (indentation included). As for action-sha, an existing version comment may
// change but may not appear or disappear. The commit itself must change:
// a comment-only or letter-case-only edit is not a bump.
func pinSHARefMove(old, next string, kinds map[string]bool) (pinSHAMove, bool) {
	if !kinds[pinKindActionSHARef] || old == next || unsafePinRE.MatchString(old) || unsafePinRE.MatchString(next) {
		return pinSHAMove{}, false
	}
	a, b := pinSHARefRE.FindStringSubmatch(old), pinSHARefRE.FindStringSubmatch(next)
	if a == nil || b == nil || a[1] != b[1] {
		return pinSHAMove{}, false
	}
	if strings.Contains(a[3], "#") != strings.Contains(b[3], "#") {
		return pinSHAMove{}, false
	}
	move := pinSHAMove{old: strings.ToLower(a[2]), next: strings.ToLower(b[2])}
	if move.old == move.next {
		return pinSHAMove{}, false
	}
	return move, true
}

// pinSHARefsUnmatched returns "" when every candidate carries the move of an
// accepted action-sha replacement somewhere in the diff, and the refusal
// reason for the first one that does not. moves is empty when action-sha is
// not enabled or the diff bumps no `uses:` pin, so every candidate is then
// refused: action-sha-ref never admits a line on its own.
func pinSHARefsUnmatched(refs []pinSHARefCandidate, moves map[pinSHAMove]bool) string {
	for _, ref := range refs {
		if !moves[ref.move] {
			return ref.file + ": bare SHA line with no matching uses: pin"
		}
	}
	return ""
}

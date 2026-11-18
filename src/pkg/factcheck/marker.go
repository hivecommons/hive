// Package factcheck holds the decisions behind the independent fact-check
// pass for analysis and report PRs (hivecommons/hive#19074): which PRs
// qualify, which agent may review one, what round the author and reviewer are
// in, and whether a draft may move to ready.
//
// It is pure: no GitHub calls, no clock reads, no agent manager. The sweep in
// cmd/hive gathers the facts, calls Decide and carries the answer out, so the
// loop-safety properties (bounded rounds, trusted markers, human takeover) can
// be tested without a cluster.
package factcheck

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Verdict is the reviewer's summary judgement for one round.
type Verdict string

// Verdicts. Pass means no finding the author must act on remains.
// Findings means at least one verified problem was posted. Inconclusive means
// the reviewer could not verify enough to judge (a source was unreachable),
// which is a person's call, not a reason to loop.
const (
	VerdictPass         Verdict = "pass"
	VerdictFindings     Verdict = "findings"
	VerdictInconclusive Verdict = "inconclusive"
)

// MarkerPrefix opens the hidden marker on the reviewer's review body.
const MarkerPrefix = "<!-- hive:factcheck"

// NeedsHumanMarker tags the one-time note posted when the loop stops without
// a passing verdict, so a later pass never posts it twice.
const NeedsHumanMarker = "<!-- hive:factcheck-needs-human -->"

const (
	// maxMarkerRound bounds the round number a marker may carry, so a forged
	// or garbled one cannot overflow the counters.
	maxMarkerRound = 99
	shaMinLen      = 7
)

var markerRe = regexp.MustCompile(`<!-- hive:factcheck round=(\d{1,2}) verdict=(pass|findings|inconclusive) head=([0-9a-f]{7,40}) -->`)

// Marker is what the reviewer states about one round.
type Marker struct {
	Round   int
	Verdict Verdict
	Head    string
}

// FormatMarker renders the hidden marker for a review body.
func FormatMarker(m Marker) string {
	return fmt.Sprintf("%s round=%d verdict=%s head=%s -->", MarkerPrefix, m.Round, m.Verdict, strings.ToLower(strings.TrimSpace(m.Head)))
}

// ParseMarker reads the marker out of a review body. When the body carries
// several (a quotation of an earlier review, say) the last one wins, the way a
// reviewer appends its own at the end. A marker that is malformed, names round
// 0 or an unknown verdict, or carries a short head is not a marker.
func ParseMarker(body string) (Marker, bool) {
	all := markerRe.FindAllStringSubmatch(body, -1)
	if len(all) == 0 {
		return Marker{}, false
	}
	m := all[len(all)-1]
	round, err := strconv.Atoi(m[1])
	if err != nil || round < 1 || round > maxMarkerRound {
		return Marker{}, false
	}
	return Marker{Round: round, Verdict: Verdict(m[2]), Head: m[3]}, true
}

// IsMarkedReview reports whether body is a fact-check round. The relay uses it
// to let a fact-check review through the per-head review cap, which exists to
// stop a loop of the swarm's own reviews and must not swallow the one review
// this pass is asked to post on a head the swarm already covered.
func IsMarkedReview(body string) bool {
	_, ok := ParseMarker(body)
	return ok
}

// SameHead reports whether two commit ids name the same commit, tolerating one
// being an abbreviation of the other (the reviewer may write a short sha).
func SameHead(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) < shaMinLen || len(b) < shaMinLen {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// HasNeedsHumanMarker reports whether a comment body is the hand-off note.
func HasNeedsHumanMarker(body string) bool {
	return strings.Contains(body, NeedsHumanMarker)
}

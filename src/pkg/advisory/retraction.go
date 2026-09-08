package advisory

import (
	"regexp"
	"strings"
)

// A finding can stop being worth a top-N slot in two very different ways, and
// the digest only ever handled one of them.
//
// The three signals behind evidenceUnverified all say the same thing: NOBODY
// re-checked this. The path is gone (#3704), the evidence came from another
// commit (#5130), the repetition was cached text (#5236). None is a statement
// about whether the finding is true, which is exactly why applyTopN demotes on
// them WITHIN a severity band rather than across bands -- an unverified
// critical may still be the worst thing in the report.
//
// This file covers the other way: the agent that filed the finding came back,
// re-checked it, and said in the finding's own detail that it does not stand.
// That is not an absence of verification. It is verification with a negative
// result, from the only party that ever looked, and it was the one such signal
// the ranking ignored entirely.
//
// The live case, from the 2026-09-05 digest on hivecommons/hive#2364, held one
// of the five CRITICAL slots while 282 findings went unshown:
//
//	[regression-risk] heartbeatBearerOK per-hive binding removed without
//	covering test
//	> REVISED: Per-hive identity binding was MOVED from heartbeatBearerOK to
//	> verifyHeartbeatBearer in hub_keys.go, not removed. ... The change is
//	> refactoring, not a security regression. Downgrading priority.
//
// "Downgrading priority" never reached the bead: beadPriorityToSeverity reads
// b.Priority, the agent edited only the notes, so the finding kept rendering at
// critical and winning a slot every cycle. The maintainer working the tracker
// wrote it up as "refuted by its own detail line and should be closed rather
// than fixed -- its bead is still open at critical, which is why it keeps
// consuming a slot".

// TWO SIGNALS ARE REQUIRED, and the asymmetry is deliberate.
//
// "REVISED:" alone is not a withdrawal. An agent can equally write "REVISED:
// raising this to critical after finding a second call site", and demoting THAT
// below every other finding would bury the most urgent thing in the report
// under a caption telling the reader to disregard it. So a finding counts as
// retracted only when its detail opens with a revision marker AND says
// somewhere that the finding does not stand.
//
// Missing a real retraction costs nothing beyond the status quo: the finding
// keeps its filed severity, exactly as today. Inventing one costs a live
// critical its slot. The rule fails toward KEEPING findings, and both
// directions are tested.

// retractionMarkerPattern matches an explicit revision lead-in at the very
// START of a finding's detail: "REVISED:", "**CORRECTION:**", "> RETRACTED --".
//
// Anchored on purpose. A finding whose body merely mentions the word revised
// ("the workflow was revised in #4102") is reporting something, not retracting
// it, and only the lead-in position separates the two. The leading character
// class absorbs the markdown and quoting agents decorate these with.
//
// "revision" is deliberately NOT in this list, though "revised" is. This
// package already uses "revision <sha>" as PROVENANCE vocabulary --
// provenanceRefPattern matches exactly that -- so a detail opening
// "revision c9546a8 ..." states where the evidence came from rather than
// withdrawing it. The collision is not hypothetical: provenance_test.go builds
// a fixture that way, and it is what caught this while the rule was written.
var retractionMarkerPattern = regexp.MustCompile(
	"(?i)^[[:space:]>*_#-]*(revised|correction|corrected|retracted|retraction|withdrawn)\\b[[:space:]:.,-]*")

// retractionWithdrawalCues are the phrases that turn a revision into a
// withdrawal. One is enough. They are matched as substrings of the LOWERCASED
// detail rather than as a regex, so extending the list later needs no thought
// about escaping.
//
// Deliberately short: every entry is something an agent would only write about
// a finding it no longer stands behind.
var retractionWithdrawalCues = []string{
	"downgrad", // "Downgrading priority", "downgraded to low"
	"retract",  // "retracting this finding"
	"withdraw", //
	"false positive",
	"not a regression",
	"not a security regression",
	"does not stand",
	"no longer applies",
	"no longer valid",
	"was incorrect",
	"is incorrect",
	"not an issue",
	"superseded",
}

// findingRetracted reports whether the agent that filed this finding has since
// withdrawn it in the finding's own detail.
//
// It reads only the detail. A title is written once, when the finding is first
// filed; the notes are where an agent goes back and revises, which is why the
// live case says nothing about the retraction in its title and why the bead's
// priority -- and therefore its rendered severity -- never moved.
func findingRetracted(f Finding) bool {
	detail := strings.TrimSpace(f.Detail)
	if detail == "" {
		return false
	}
	if !retractionMarkerPattern.MatchString(detail) {
		return false
	}
	lower := strings.ToLower(detail)
	for _, cue := range retractionWithdrawalCues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

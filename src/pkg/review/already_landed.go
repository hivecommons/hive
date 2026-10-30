package review

import (
	"regexp"
	"strconv"
	"strings"
)

// Already-landed detection (hivecommons/hive#15647, #15648).
//
// A reviewer that finds a PR's change already on the base branch says so
// in words — "close this adoption of #15324 as already landed, since v5
// already contains the identical two-line test change via #15506" — and
// usually also sets `<!-- hive-redundant-of: #15506 -->`. The marker is
// the contract; the wording is the fallback for a review that forgot it.
// The fallback is conservative on purpose: it needs an already-landed
// phrase AND exactly one merged-PR reference introduced by a landing
// preposition (via / through / in / by / since). A question that merely
// mentions a PR ("should #15506's flag default on?") names nothing, and a
// question whose only reference is the adoption's own lineage ("close this
// adoption of #15324 as already landed") names nothing either: the number
// after "adoption of" is the superseded original, not the PR that landed
// the change.

var (
	alreadyLandedPhraseRE = regexp.MustCompile(`(?i)\b(already (landed|merged|on (the )?base|on [a-z0-9._/-]+|contains?|in place|there|done)|no-op|redundant|landed (this|the|it)|superseded by|identical (change|fix|section|lines?|diff|two-line)|same (change|fix|lines?) (already|landed|merged))\b`)
	// alreadyLandedRefRE captures "<preposition> #N" (and "<preposition>
	// owner/repo#N" / ".../pull/N"); the preposition is what tells the
	// landing PR from a lineage mention.
	alreadyLandedRefRE     = regexp.MustCompile(`(?i)\b(via|through|in|by|since|landed in|merged (?:in|as|via|through))\s+(?:[\w.-]+/[\w.-]+)?#(\d+)\b`)
	alreadyLandedPullURLRE = regexp.MustCompile(`(?i)\b(via|through|in|by|since)\s+https?://[^\s/]+/[\w.-]+/[\w.-]+/pull/(\d+)\b`)
	// alreadyLandedMergedRefRE captures "#N (merged)" — the reviewer
	// annotating the reference with its state names the landing PR as
	// surely as a preposition does (hivecommons/hive#15732: "now that
	// #15545 (merged) carries this change").
	alreadyLandedMergedRefRE = regexp.MustCompile(`(?i)(?:[\w.-]+/[\w.-]+)?#(\d+)\s*\((?:already |now )?(?:merged|landed)\)`)
)

// AlreadyLandedClaim reports whether any of texts says the change is
// already on the base (an "already landed" / "no-op" / "redundant"
// phrase), whether or not it names the PR that landed it.
func AlreadyLandedClaim(texts ...string) bool {
	for _, t := range texts {
		if alreadyLandedPhraseRE.MatchString(t) {
			return true
		}
	}
	return false
}

// AlreadyLandedPR returns the PR the review found already carries this
// change: redundantOf (the `hive-redundant-of` marker) when set, else the
// one PR the texts name as having landed it, else 0. A text with an
// already-landed phrase but no landing reference, or with two different
// landing references, yields 0: the hive then proves redundancy
// mechanically or leaves the question to a person.
func AlreadyLandedPR(redundantOf int, texts ...string) int {
	if redundantOf > 0 {
		return redundantOf
	}
	found := 0
	for _, t := range texts {
		if !alreadyLandedPhraseRE.MatchString(t) {
			continue
		}
		for _, m := range alreadyLandedRefRE.FindAllStringSubmatch(t, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 {
				continue
			}
			if found != 0 && found != n {
				return 0
			}
			found = n
		}
		for _, m := range alreadyLandedPullURLRE.FindAllStringSubmatch(t, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 {
				continue
			}
			if found != 0 && found != n {
				return 0
			}
			found = n
		}
		for _, m := range alreadyLandedMergedRefRE.FindAllStringSubmatch(t, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil || n <= 0 {
				continue
			}
			if found != 0 && found != n {
				return 0
			}
			found = n
		}
	}
	return found
}

// alreadyLandedTexts is the verdict text the stage engine and the relay
// read for the claim: the decision question first, then the per-perspective
// human summaries.
func alreadyLandedTexts(question string, summaries []string) []string {
	out := make([]string, 0, 1+len(summaries))
	if q := strings.TrimSpace(question); q != "" {
		out = append(out, q)
	}
	for _, s := range summaries {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// AlreadyLandedFromVerdict is AlreadyLandedPR over a verdict's decision
// question and human summaries; claimed is AlreadyLandedClaim over the
// same texts (true whenever landed > 0).
func AlreadyLandedFromVerdict(redundantOf int, question string, summaries []string) (landed int, claimed bool) {
	texts := alreadyLandedTexts(question, summaries)
	landed = AlreadyLandedPR(redundantOf, texts...)
	return landed, landed > 0 || AlreadyLandedClaim(texts...)
}

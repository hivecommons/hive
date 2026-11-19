package review

import "strings"

// preflightSection is the hive-gathered evidence block for a PR (repository
// rule documents, named operator rules, dependency advisories and release age;
// hivecommons/hive#19069). It is empty unless the caller attached evidence, so
// a hive with the checks off sends exactly the prompt it always did.
func preflightSection(pr PullRequest) string {
	s := strings.TrimSpace(pr.PreflightSection)
	if s == "" {
		return ""
	}
	return "\nEVIDENCE GATHERED BY THE HIVE BEFORE YOU STARTED. It was computed outside the diff, so use it where it applies and ignore it where it does not. Every finding that rests on it must quote the rule, advisory id, version or date it came from; a finding without that is noise.\n" + s + "\n"
}

// understandingGuidance asks the correctness reviewer to understand the change
// rather than match it against a list of known bug patterns
// (hivecommons/hive#19106). It is a method, not a checklist: no defect classes
// are named, so it applies to any change in any language.
const understandingGuidance = "\nUNDERSTAND THE CHANGE BEFORE JUDGING IT.\n" +
	"1. State, in your own words, the behavior change this PR intends, from the diff and the PR description.\n" +
	"2. Trace the changed code paths and their callers far enough to see what now actually happens.\n" +
	"3. Identify the invariants and contracts the change relies on or alters, and check each one against the code.\n" +
	"4. Check that everything that describes the behavior (comments, docs, messages, tests) still matches the new behavior.\n" +
	"Report only findings you can back with concrete evidence from the code.\n"

// perspectiveGuidance returns the extra guidance for a perspective: the
// understanding method belongs to correctness.
func perspectiveGuidance(p Perspective) string {
	if p == PerspectiveCorrectness {
		return understandingGuidance
	}
	return ""
}

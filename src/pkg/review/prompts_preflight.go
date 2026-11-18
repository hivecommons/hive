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

// subtleDefectGuidance names two defect classes a diff-only read tends to miss.
// It is deliberately generic: it asks for the same two checks on any language.
const subtleDefectGuidance = "\nTWO CHECKS DIFFS HIDE.\n" +
	"- Messages that stopped being true. When the diff changes what a function, branch or condition does, search the changed files and their callers for error strings, log lines, panic or assert texts, test failure messages and comments that still describe the old behavior. Report one only with the line that is now wrong and the changed behavior that made it wrong.\n" +
	"- Sentinels in aggregates. When a value can be a sentinel (zero, -1, an empty string, a maximum integer, a nil or unset marker) and it flows into a min, max, sum, count, average or comparison, check that the sentinel cannot win or skew the aggregate (a 0 that always becomes the minimum, a -1 summed into a total). Report one only with the sentinel source, the aggregate and a concrete input that produces the wrong result.\n"

// perspectiveGuidance returns the extra guidance for a perspective: the
// subtle-defect checks belong to correctness.
func perspectiveGuidance(p Perspective) string {
	if p == PerspectiveCorrectness {
		return subtleDefectGuidance
	}
	return ""
}

package github

import "strconv"

// PRSupersedesNumber returns the first pull request a PR body declares it
// supersedes ("Supersedes #N", the line the hive's adoption / takeover /
// re-author flows write — AdoptionPRBody), or 0, false when it declares
// none.
func PRSupersedesNumber(body string) (int, bool) {
	for _, m := range prSupersedesRe.FindAllStringSubmatch(body, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			return v, true
		}
	}
	return 0, false
}

// HiveSupersedingPR reports whether a PR is the hive's own superseding
// copy of another PR: authored by the hive App identity (hiveAuthored) and
// declaring "Supersedes #N" in its body. Owner rule (@clubanderson): "if a
// PR is superseded by a PR that a hive agent creates, it should auto merge"
// — such a PR never waits on an operator decision for an ordinary reviewer
// finding; only a sentinel finding or an emergent hard-stop parks it
// (hiveSupersederHardStop in pkg/github/automerge).
func HiveSupersedingPR(hiveAuthored bool, body string) bool {
	if !hiveAuthored {
		return false
	}
	_, ok := PRSupersedesNumber(body)
	return ok
}

// IsHiveSupersedingPR is HiveSupersedingPR over an enumerated PullRequest:
// the App's own login (AppAuthored) with a recorded Supersedes number.
func IsHiveSupersedingPR(pr PullRequest) bool {
	return pr.AppAuthored && pr.Supersedes > 0
}

// prSupersedesNumberOrZero is PRSupersedesNumber for struct literals.
func prSupersedesNumberOrZero(body string) int {
	n, _ := PRSupersedesNumber(body)
	return n
}

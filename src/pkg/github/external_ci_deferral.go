package github

import (
	"regexp"
	"strconv"
	"strings"
)

// externalCIMarkerRE matches the hidden marker a fix lane stamps on the one
// comment it leaves when hive-baseline-check.sh returns DEFER_EXTERNAL for a
// red PR: `<!-- hive-ci-external head=<sha> check=<name> run=<id> -->`, where
// <sha> is the full head commit the triage ran against, <name> the failing
// check (percent-encoded, for people reading the comment) and <id> the check
// run that failed. The helper prints the exact string as `external_marker`.
// hivecommons/hive#11609.
var externalCIMarkerRE = regexp.MustCompile(`<!-- hive-ci-external head=([0-9a-fA-F]{40}) check=[^\s>]+ run=([1-9][0-9]*) -->`)

// externalCIDeferred reports whether every failing check run on a red PR
// carries an external-CI marker for the PR's current head and that exact run.
// texts are the PR's comment bodies. The marker is pinned to the run, not
// just the check name: a check that went green and then failed again at the
// same head is a new run, so the PR is triaged again rather than hidden by a
// verdict about an earlier failure. A marker for another head is stale too,
// and a failing run with no marker keeps the PR listed. A PR with no known
// failing run is never deferred: there is nothing to check the markers
// against.
func externalCIDeferred(texts []string, headSHA string, failingRunIDs []int64) bool {
	if headSHA == "" || len(failingRunIDs) == 0 {
		return false
	}
	external := map[int64]bool{}
	for _, text := range texts {
		for _, m := range externalCIMarkerRE.FindAllStringSubmatch(text, -1) {
			if !strings.EqualFold(m[1], headSHA) {
				continue
			}
			if id, err := strconv.ParseInt(m[2], 10, 64); err == nil {
				external[id] = true
			}
		}
	}
	for _, id := range failingRunIDs {
		if !external[id] {
			return false
		}
	}
	return true
}

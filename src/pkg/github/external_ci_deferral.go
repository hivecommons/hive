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
	return everyFailingRunMarked(externalCIMarkerRE, texts, headSHA, failingRunIDs)
}

// transientCIMarkerRE matches the hidden marker an agent stamps on its triage
// comment when it diagnoses a red check on a PR as transient or infrastructure
// (a flaky job, a runner outage, a race against a cache the default branch
// was still publishing) rather than caused by the PR's diff:
// `<!-- hive-ci-transient head=<sha> check=<name> run=<id> -->`, with the same
// fields and pinning as the hive-ci-external marker. hivecommons/hive#12977.
var transientCIMarkerRE = regexp.MustCompile(`<!-- hive-ci-transient head=([0-9a-fA-F]{40}) check=[^\s>]+ run=([1-9][0-9]*) -->`)

// transientCITriaged reports whether every failing check run on a red PR
// carries a transient-CI marker for the PR's current head and that exact
// run, under the same rules as externalCIDeferred: a push or a new failing
// run makes the triage stale.
func transientCITriaged(texts []string, headSHA string, failingRunIDs []int64) bool {
	return everyFailingRunMarked(transientCIMarkerRE, texts, headSHA, failingRunIDs)
}

// everyFailingRunMarked reports whether every id in failingRunIDs is named by
// a marker matching re (submatch 1 the head sha, 2 the run id) for headSHA.
func everyFailingRunMarked(re *regexp.Regexp, texts []string, headSHA string, failingRunIDs []int64) bool {
	if headSHA == "" || len(failingRunIDs) == 0 {
		return false
	}
	marked := map[int64]bool{}
	for _, text := range texts {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if !strings.EqualFold(m[1], headSHA) {
				continue
			}
			if id, err := strconv.ParseInt(m[2], 10, 64); err == nil {
				marked[id] = true
			}
		}
	}
	for _, id := range failingRunIDs {
		if !marked[id] {
			return false
		}
	}
	return true
}

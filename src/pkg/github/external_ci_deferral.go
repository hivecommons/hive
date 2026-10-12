package github

import (
	"net/url"
	"regexp"
	"strings"
)

// externalCIMarkerRE matches the hidden marker a fix lane stamps on the one
// comment it leaves when hive-baseline-check.sh returns DEFER_EXTERNAL for a
// red PR: `<!-- hive-ci-external head=<sha> check=<name> -->`, where <sha> is
// the full head commit the triage ran against and <name> is the failing
// check, percent-encoded (the helper prints the exact string as
// `external_marker`). hivecommons/hive#11609.
var externalCIMarkerRE = regexp.MustCompile(`<!-- hive-ci-external head=([0-9a-fA-F]{40}) check=([^\s>]+) -->`)

// externalCIDeferred reports whether every failing check on a red PR carries
// an external-CI marker for the PR's current head. texts are the PR's comment
// bodies. A marker for another head is stale: a push may have changed what
// fails, so the PR is triaged again. A failing check with no marker means the
// failure is not (yet) known to be upstream's, so the PR stays listed. A PR
// with no named failing check is never deferred: there is nothing the
// markers can be checked against.
func externalCIDeferred(texts []string, headSHA string, failingChecks []string) bool {
	if headSHA == "" || len(failingChecks) == 0 {
		return false
	}
	external := map[string]bool{}
	for _, text := range texts {
		for _, m := range externalCIMarkerRE.FindAllStringSubmatch(text, -1) {
			if !strings.EqualFold(m[1], headSHA) {
				continue
			}
			name, err := url.PathUnescape(m[2])
			if err != nil {
				continue
			}
			external[name] = true
		}
	}
	for _, check := range failingChecks {
		if !external[check] {
			return false
		}
	}
	return true
}

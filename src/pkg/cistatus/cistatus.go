// Package cistatus holds commit-CI reason predicates shared by pkg/github
// (which produces the reasons) and pkg/stageengine (which classifies them
// into PR stages). pkg/github imports pkg/stageengine, so the predicate
// cannot live in either without one copying the other
// (hivecommons/hive#13823).
//
// This package has no hive imports so anything may use it.
package cistatus

import "strings"

// ReasonIsPending reports whether a commit-CI skip reason means the checks
// are still running or not yet reported, as opposed to failing.
func ReasonIsPending(reason string) bool {
	return strings.HasPrefix(reason, "ci-reporting-stalled:") ||
		strings.HasPrefix(reason, "ci-reporting-unverified:") ||
		strings.HasSuffix(reason, "-pending") ||
		strings.HasPrefix(reason, "pending: ") ||
		strings.HasPrefix(reason, "required-check-missing:")
}

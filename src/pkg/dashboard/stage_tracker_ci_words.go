package dashboard

import "strings"

// stageTrackerCIStateWords renders a commit-CI reason token
// (github.CommitCIState.Reason: "check-pending", "status-pending",
// "check-<conclusion>", "status-<state>", "check-failing:<name>") as the
// state it names. Pending is not red: the tracker once captioned a 060
// row "CI is red (check-pending)" while its checks were still running
// (hivecommons/docs#512). ok is false for anything that is not a CI token.
func stageTrackerCIStateWords(reason string) (words string, ok bool) {
	r := strings.TrimSpace(reason)
	lower := strings.ToLower(r)
	var kind, rest string
	switch {
	case strings.HasPrefix(lower, "check-"):
		kind, rest = "check", r[len("check-"):]
	case strings.HasPrefix(lower, "status-"):
		kind, rest = "status", r[len("status-"):]
	default:
		return "", false
	}
	if name, found := strings.CutPrefix(rest, "failing:"); found {
		return "CI failed: " + name, true
	}
	switch strings.ToLower(rest) {
	case "pending", "queued", "in_progress", "waiting", "requested", "expected":
		return "CI running", true
	case "success":
		return "CI green", true
	case "failure", "error":
		return "CI failed", true
	case "neutral", "skipped":
		return "CI skipped (" + kind + " " + strings.ToLower(rest) + ")", true
	}
	return "CI failed (" + strings.ReplaceAll(strings.ToLower(rest), "_", " ") + ")", true
}

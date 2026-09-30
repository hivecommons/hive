package releasesentinel

import (
	"fmt"
	"regexp"
	"strings"
)

// statusCompleted is the workflow-run status GitHub reports once a run has a
// conclusion; every other status (queued, in_progress, waiting, requested,
// pending) is still in flight.
const statusCompleted = "completed"

// IsCompleted reports whether a workflow run status means the run finished.
func IsCompleted(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), statusCompleted)
}

// blockingConclusions are the completed-run conclusions that block a release.
// failure and timed_out are the issue's contract; startup_failure (the
// workflow file itself was rejected) is a failure GitHub merely spells
// differently, and an agent can fix it. Everything else - success, cancelled
// (usually superseded by a newer run), skipped, neutral, stale and
// action_required (a run waiting on approval has not failed) - does not block.
var blockingConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"startup_failure": true,
}

// IsBlockingConclusion reports whether a completed run's conclusion blocks
// the release.
func IsBlockingConclusion(conclusion string) bool {
	return blockingConclusions[strings.ToLower(strings.TrimSpace(conclusion))]
}

// FailureClass separates failures an agent can repair from ones it cannot.
type FailureClass string

const (
	// ClassFixable: code, test or workflow-content failures a commit can fix.
	ClassFixable FailureClass = "fixable"
	// ClassPolicy: an org/repo setting, permission, secret or approval gate. No
	// commit can fix it; a human must change a setting.
	ClassPolicy FailureClass = "policy"
)

// policyPattern is one known "a human must change a setting" signature.
type policyPattern struct {
	re     *regexp.Regexp
	reason string
}

// policyPatterns are matched case-insensitively against a failed run's
// evidence. Each one is a real failure mode:
//   - #5875: the org/repo setting that lets Actions open PRs was off.
//   - #6804: the App token lacked the workflows permission, so GitHub refused
//     a push that touched .github/workflows/.
//   - the tagged-release cross-registry mirror fails hard on a missing secret.
//   - 403 / "Resource not accessible by integration": a token scope problem.
//   - billing / spending limit: an account setting.
var policyPatterns = []policyPattern{
	{regexp.MustCompile(`(?i)not permitted to create or approve pull requests`), "GitHub Actions is not permitted to create or approve pull requests (org/repo Actions setting)"},
	{regexp.MustCompile("(?i)refusing to allow a github app to create or update workflow|without [`']?workflows[`']? permission"), "the GitHub App token lacks the workflows permission"},
	{regexp.MustCompile(`(?i)resource not accessible by integration`), "the workflow token lacks a required permission (Resource not accessible by integration)"},
	{regexp.MustCompile(`(?i)\b403\b[^\n]*(permission|forbidden)|(permission|forbidden)[^\n]*\b403\b`), "403 permission denied"},
	{regexp.MustCompile(`(?i)\bsecret\b[^\n]*\b(is not set|not found|missing)\b`), "a required repository or org secret is not set"},
	{regexp.MustCompile(`(?i)spending limit|recent account payments have failed`), "GitHub Actions billing or spending limit"},
	{regexp.MustCompile(`(?i)must have admin rights`), "an operation requires repository admin rights"},
}

// Classify decides whether one blocking run is agent-fixable. A failed run
// that produced no jobs never executed anything, so the cause is outside the
// code (approval gate, billing, org policy); the same holds for any run whose
// evidence matches a known policy signature.
func Classify(r BlockingRun) (FailureClass, string) {
	if r.JobCount == 0 {
		return ClassPolicy, fmt.Sprintf("workflow run %q (%d) failed without running any job (approval gate, billing or org policy)", r.Name, r.ID)
	}
	text := strings.Join(append(append([]string{}, r.FailedJobs...), r.Evidence...), "\n")
	for _, p := range policyPatterns {
		if p.re.MatchString(text) {
			return ClassPolicy, fmt.Sprintf("workflow run %q (%d): %s", r.Name, r.ID, p.reason)
		}
	}
	return ClassFixable, ""
}

// ClassifyFailures classifies a round's blocking runs as a whole: ONE policy
// failure makes the whole round a human's problem, because a code fix pushed
// alongside an unfixable setting cannot turn the release green and would only
// burn a round (and, once retagging lands, a tag move).
func ClassifyFailures(runs []BlockingRun) (FailureClass, string) {
	var reasons []string
	for _, r := range runs {
		if class, why := Classify(r); class == ClassPolicy {
			reasons = append(reasons, why)
		}
	}
	if len(reasons) == 0 {
		return ClassFixable, ""
	}
	return ClassPolicy, strings.Join(reasons, "; ")
}

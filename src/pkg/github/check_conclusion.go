package github

// Completed check-run conclusions the merge CI gate lets through without a
// success: a job skipped by its own `if:` condition (a deploy job gated on
// `github.event_name != 'pull_request'`) or one that reported neutral. The
// gate neither blocks on them nor counts them as success evidence, and
// GitHub's branch protection treats them as satisfying a required check.
const (
	CheckConclusionNeutral = "neutral"
	CheckConclusionSkipped = "skipped"
)

// CheckConclusionNonBlocking reports a completed check-run conclusion the
// merge CI gate does not block on and does not call red (skipped, neutral).
// The dashboard's CI track reads it so the strip and the gate agree on what
// a finished check that did not run means.
func CheckConclusionNonBlocking(conclusion string) bool {
	switch conclusion {
	case CheckConclusionNeutral, CheckConclusionSkipped:
		return true
	}
	return false
}

// CheckConclusionIsRed reports a completed check-run conclusion the merge
// CI gate treats as a failure.
func CheckConclusionIsRed(conclusion string) bool {
	return checkConclusionIsRed(conclusion)
}

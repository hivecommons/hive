package github

import (
	"context"
	"fmt"
)

// Missing-check base refresh: an open PR whose head predates a newly required
// check.
//
// When a repository makes a new check required, a PR opened before that has
// no run for it: its head predates the workflow, so nothing ever reports the
// check, and the merge gate skips it as `required-check-missing:<ctx>` every
// cycle. That is not red, so the stale-red base refresh never touches it, and
// a re-run or a close/reopen reuses the PR's stale merge ref. Only a new head
// picks the workflow up from the current base. On projectbluefin/testsuite#1245
// `dashboard-build` became required while the PR was open; it sat skipped for
// about 29 hours until a person pressed "Update branch", and the check started
// within seconds on the new head.
//
// The caller (cmd/hive) picks the candidates (the check has no run or status
// at all on the head, CI is otherwise settled green, and the check was seen
// on the base tip or on another open PR's head in the same repository), caps
// the updates, and remembers what it settled, so a PR is updated at most once
// per (head, base tip) and once per missing check.

// RefreshMissingCheckPR merges base.SHA into pr's branch when the head is
// behind it, so the workflows that report the required check named check run
// on a head that has them. It shares the stale-red refresh's update: pinned
// to the evaluated head, gated as KindBaseRefresh, and left to the automerge
// preparation front where auto_merge.require_current_base owns it.
func (c *Client) RefreshMissingCheckPR(ctx context.Context, pr PullRequest, base StaleRedBaseState, check string) (StaleRedRefreshOutcome, error) {
	return c.refreshPRBehindBase(ctx, pr, base.SHA, "hive:missing_check_base_refresh",
		fmt.Sprintf("required check %q never reported on the head", check))
}

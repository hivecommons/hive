package github

// ReviewVerdictHook is told that the relay recorded at least one verdict
// bound to a review dispatch for PR number in repo. The head that wrote it
// is free again, and the PR may now be ready for its next head or its
// decision kick, so the hive uses it to wake the governor loop rather than
// leave the reviewers idle until the next eval cycle
// (hivecommons/hive#14705).
type ReviewVerdictHook func(repo string, number int)

// SetReviewVerdictHook installs (or with nil, removes) the verdict hook.
// Safe to call before or after the watchers start.
func (c *Client) SetReviewVerdictHook(fn ReviewVerdictHook) {
	if c == nil {
		return
	}
	if fn == nil {
		c.reviewVerdictHook.Store(nil)
		return
	}
	c.reviewVerdictHook.Store(&fn)
}

// notifyReviewVerdict reports a recorded dispatched verdict to the installed
// hook, if any.
func (c *Client) notifyReviewVerdict(repo string, number int) {
	if c == nil {
		return
	}
	if hook := c.reviewVerdictHook.Load(); hook != nil && *hook != nil {
		(*hook)(repo, number)
	}
}

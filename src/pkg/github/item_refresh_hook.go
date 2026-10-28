package github

import "context"

// ItemRefreshHook requests a targeted re-read after the hive changes an item's
// state. It must enqueue work without blocking on a GitHub read. isPR selects
// the issue or pull-request endpoint for the read.
type ItemRefreshHook func(repo string, number int, isPR bool)

// SetItemRefreshHook installs (or with nil, removes) the refresh hook.
// Safe to call before or after the watchers start.
func (c *Client) SetItemRefreshHook(fn ItemRefreshHook) {
	if c == nil {
		return
	}
	if fn == nil {
		c.itemRefreshHook.Store(nil)
		return
	}
	c.itemRefreshHook.Store(&fn)
}

// unblockIssue requests a refresh as soon as removal succeeds, even if posting
// the explanatory comment later fails. Failed or refused writes never notify.
func (c *Client) unblockIssue(ctx context.Context, repo string, number int) error {
	if err := c.RemoveLabel(ctx, repo, number, BlockedLabel); err != nil {
		return err
	}
	if hook := c.itemRefreshHook.Load(); hook != nil && *hook != nil {
		(*hook)(repo, number, false)
	}
	return nil
}

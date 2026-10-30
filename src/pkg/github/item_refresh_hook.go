package github

import (
	"context"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// ItemRefreshHook is told, with the configured repo name and the item number,
// that the hive itself just changed an issue or pull request on GitHub in a
// way the cached enumeration should reflect at once. The dashboard registers
// one so a self-initiated unblock re-reads the row the same way an inbound
// label webhook does, instead of showing the stale `blocked` label until the
// next full enumeration pass.
type ItemRefreshHook func(repo string, number int, isPR bool)

var (
	itemRefreshHookMu sync.RWMutex
	itemRefreshHookFn ItemRefreshHook
)

// SetItemRefreshHook installs (or, with nil, removes) the process-wide
// ItemRefreshHook.
func SetItemRefreshHook(fn ItemRefreshHook) {
	itemRefreshHookMu.Lock()
	itemRefreshHookFn = fn
	itemRefreshHookMu.Unlock()
}

func itemRefreshHook() ItemRefreshHook {
	itemRefreshHookMu.RLock()
	defer itemRefreshHookMu.RUnlock()
	return itemRefreshHookFn
}

// notifyItemRefresh runs the registered ItemRefreshHook, if any.
func notifyItemRefresh(repo string, number int, isPR bool) {
	if hook := itemRefreshHook(); hook != nil && number > 0 {
		hook(repo, number, isPR)
	}
}

// unblockIssue removes the `blocked` label from an issue the hive itself
// blocked and tells the ItemRefreshHook, so the cached actionable snapshot and
// the Activity Monitor re-derive the row immediately rather than after the
// next enumeration.
func (c *Client) unblockIssue(ctx context.Context, repo string, number int) error {
	if err := c.RemoveLabel(ctx, repo, number, BlockedLabel); err != nil {
		return err
	}
	// The row's since-time restarts at the lift (hivecommons/hive#15779).
	stagelabels.Shared().NoteUnblocked(stagelabels.Key(repo, number), time.Now())
	notifyItemRefresh(repo, number, false)
	return nil
}

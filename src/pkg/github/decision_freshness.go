package github

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DecisionFreshnessBound is how old the governor's copy of an issue or PR
// may be before an irreversible action on it (close, merge) must re-read the
// item first. Webhook wakes plan from a cached snapshot whose enumeration may
// be a full reconcile interval old; the bound keeps a close or merge from
// acting on a view older than this without one targeted GET.
const DecisionFreshnessBound = 60 * time.Second

// ErrSnapshotStaleForDecision is returned when an irreversible action is
// refused because the repo's share of the snapshot is last-good data served
// after GitHub refused the enumeration: nothing about it is trustworthy
// enough to close or merge on.
var ErrSnapshotStaleForDecision = errors.New("snapshot is stale (last-good after a GitHub refusal); irreversible action deferred")

// ErrItemNotOpen is returned when the confirming re-read finds the item
// already closed or merged.
var ErrItemNotOpen = errors.New("item is no longer open; irreversible action skipped")

// IrreversibleGuard decides whether an irreversible action may proceed on
// one issue (isPR=false) or PR. A nil error means go ahead.
type IrreversibleGuard func(ctx context.Context, repo string, number int, isPR bool) error

type irreversibleGuardKey struct{}

// WithIrreversibleGuard attaches a guard that CloseIssue and MergePR consult
// before writing. Snapshot-driven passes (the issue stage sync, the sweeps
// the run loop schedules after a cycle) install one built from the snapshot
// they planned from so a stale or old view never closes or merges anything.
func WithIrreversibleGuard(ctx context.Context, guard IrreversibleGuard) context.Context {
	if guard == nil {
		return ctx
	}
	return context.WithValue(ctx, irreversibleGuardKey{}, guard)
}

// CheckIrreversible runs the guard attached to ctx, if any.
func CheckIrreversible(ctx context.Context, repo string, number int, isPR bool) error {
	if ctx == nil {
		return nil
	}
	guard, _ := ctx.Value(irreversibleGuardKey{}).(IrreversibleGuard)
	if guard == nil {
		return nil
	}
	return guard(ctx, repo, number, isPR)
}

// SnapshotFreshnessGuard builds the guard for actions planned from result:
//
//   - a repo listed in result.StaleRepos refuses outright
//     (ErrSnapshotStaleForDecision);
//   - an item whose copy is younger than DecisionFreshnessBound proceeds
//     without a call;
//   - otherwise one targeted GET re-reads the item, and the action proceeds
//     only if it is still open (ErrItemNotOpen otherwise). A read GitHub
//     refuses (budget critical, caution window, rate limit) is an error, so
//     the action waits for the next pass rather than acting blind.
//
// A nil result installs no bound: the caller is not planning from a
// snapshot (the merge relay, agent-driven closes).
func (c *Client) SnapshotFreshnessGuard(result *ActionableResult, now func() time.Time) IrreversibleGuard {
	if c == nil || result == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, repo string, number int, isPR bool) error {
		if result.RepoStale(repo) {
			return fmt.Errorf("%s#%d: %w", repo, number, ErrSnapshotStaleForDecision)
		}
		if age := now().Sub(result.ItemFetchedAt(repo, number, isPR)); age <= DecisionFreshnessBound {
			return nil
		}
		snap, err := c.FetchWebhookItemSnapshot(ctx, repo, number, isPR)
		if err != nil {
			return fmt.Errorf("confirming %s#%d before irreversible action: %w", repo, number, err)
		}
		if !snap.Open() || snap.Merged {
			return fmt.Errorf("%s#%d: %w", repo, number, ErrItemNotOpen)
		}
		return nil
	}
}

package github

// Approval-desk hook for the self-authored auto-merge sweep — the ONE real
// producer wired in the RFC #4000 vertical slice.
//
// Why this producer: the self-merge gate is the cheapest honest one to migrate.
// It is already the exact shape the desk generalizes — (requested action, ACMM
// level, identity) → verdict — it is the gate whose history the RFC cites (the
// sweep originally had NO ACMM check, which is how an L4 hive wrongly
// self-merged its own PR), and it needs no new call site: the sweep already
// computes everything the desk's Request wants (repo, number, author, and a
// real ChecksGreen from commitGreen).
//
// DEFAULT OFF. The hook is nil unless a caller installs one via
// SetApprovalDesk, and a nil hook means trySweepSelfAuthoredPR behaves exactly
// as it does today. This is the "live behavior is unchanged until enabled"
// requirement: shipping this file changes nothing about any running hive.
//
// When the desk IS enabled, the sweep's own ACMM gate stays in place upstream
// (StartSelfAuthoredAutoMergeSweep still refuses to start below the floor), so
// the desk is strictly an ADDITIONAL, inspectable consultation on top — it can
// withhold a merge the legacy gate would have allowed, and it can route one
// into the operator inbox, but it can never permit a merge the legacy gate
// refused. That asymmetry is what makes enabling the flag safe to try on a
// live hive.

import (
	"context"

	gh "github.com/google/go-github/v72/github"
)

// labelNames flattens a PR's labels to their names for rule matching. A nil
// label (possible in sparse API responses) is skipped rather than panicking.
func labelNames(labels []*gh.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		if name := l.GetName(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ApprovalDeskHook is the desk consultation the sweep performs per PR. It
// returns allow=true to proceed with the merge, or allow=false with a reason
// that the sweep reports as a normal skip.
//
// The signature is deliberately narrow — no toolapprove types — so pkg/github
// does not take a dependency on the desk package. The caller (cmd/hive, which
// already owns both) closes over the desk and the inbox and adapts. This keeps
// the concurrent #4002 turn work free to reshape toolapprove internals without
// touching the GitHub client.
type ApprovalDeskHook func(ctx context.Context, req ApprovalDeskRequest) (allow bool, reason string)

// ApprovalDeskRequest carries what the desk needs to resolve a self-merge.
type ApprovalDeskRequest struct {
	// Kind is always the self-merge lane for this producer; carried explicitly
	// so one hook implementation can serve additional producers later.
	Kind string
	// Repo is the display repo, "org/name".
	Repo string
	// Number is the PR number.
	Number int
	// Author is the PR author's login (the App bot, on this path).
	Author string
	// Title is the PR title, for rule matching.
	Title string
	// Labels are the PR's labels, for rule matching.
	Labels []string
	// ChecksGreen reports the sweep's own commitGreen verdict. The canonical
	// bulk-approve rule ("fifty green dependabot PRs") is written over this.
	ChecksGreen bool
	// HeadSHA pins which commit was evaluated, so a verdict is attributable to
	// a specific tree rather than to "the PR".
	HeadSHA string
}

// ApprovalDeskKindSelfMerge names the lane this producer feeds. It mirrors
// toolapprove.KindSelfMerge without importing it.
const ApprovalDeskKindSelfMerge = "self-merge"

// ApprovalDeskKindQueuedMerge names the trusted-human merge-queue lane. It
// mirrors toolapprove.KindQueuedMerge without importing it.
const ApprovalDeskKindQueuedMerge = "queued-merge"

// SetApprovalDesk installs the approval-desk hook. Passing nil removes it,
// restoring pre-desk behavior. A nil client is a no-op.
//
// Not guarded by a mutex because it is called once during startup wiring,
// before any sweep goroutine starts — the same lifecycle every other
// Client field setter on this type assumes.
func (c *Client) SetApprovalDesk(hook ApprovalDeskHook) {
	if c == nil {
		return
	}
	c.approvalDesk = hook
}

// consultApprovalDesk runs the desk hook if one is installed. With no hook it
// returns allow=true, which is what makes this whole feature a no-op by
// default: the sweep proceeds exactly as it did before the desk existed.
func (c *Client) consultApprovalDesk(ctx context.Context, req ApprovalDeskRequest) (bool, string) {
	if c == nil || c.approvalDesk == nil {
		return true, ""
	}
	allow, reason := c.approvalDesk(ctx, req)
	if !allow && reason == "" {
		// A hook that refuses without a reason still produces an actionable
		// skip line rather than a mystery.
		reason = "approval-desk-withheld"
	}
	return allow, reason
}

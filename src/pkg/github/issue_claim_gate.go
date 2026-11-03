package github

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/claims"
)

// ClaimBlockReasonPullRequest is the IssueClaimBlockReason for a number that
// names a pull request. Issue claims (the ledger, the `claim` relay request,
// the contributor lane) stage an item in the issue family — stage/020,
// stage/030 — and a pull request must never carry one (ADR-0025: the
// families are disjoint). A PR's stage is the PR sweep's alone.
const ClaimBlockReasonPullRequest = "pull request"

// ClaimBlockReasonIssueClosed is the IssueDispatchBlockReason for an issue
// the forge already reports closed (hivecommons/hive#14814): a closing
// keyword on a merged PR, or a person, closed it after the snapshot the
// producer is dispatching from was listed. There is nothing to start.
const ClaimBlockReasonIssueClosed = "issue closed"

// Prefixes of the IssueClaimBlockReason values that name the label at fault:
// "never-claim label: Epic" for an umbrella issue
// (governor.claims.never_claim_labels), "parked: stage/045-needs-decision"
// for an issue a person holds a decision on.
const (
	ClaimBlockReasonNeverClaimPrefix = "never-claim label: "
	ClaimBlockReasonParkedPrefix     = "parked: "
)

// umbrellaLabels is the never-claim set the pure derivation (IssueStageFor)
// reads, kept in step with the client by SetNeverClaimLabels. nil → the
// package default.
var umbrellaLabels atomic.Pointer[[]string]

// SetNeverClaimLabels installs governor.claims.never_claim_labels: the labels
// that mark an issue as not a unit of agent work (default
// claims.DefaultNeverClaimLabels, i.e. `epic`). IssueClaimBlockReason refuses
// a claim on an issue wearing one and IssueStageFor never stages a claim on
// one. nil restores the default; an empty list turns the gate off.
func (c *Client) SetNeverClaimLabels(labels []string) {
	var copied []string
	if labels != nil {
		copied = append([]string{}, labels...)
	}
	if copied == nil {
		umbrellaLabels.Store(nil)
	} else {
		umbrellaLabels.Store(&copied)
	}
	if c == nil {
		return
	}
	c.neverClaimMu.Lock()
	defer c.neverClaimMu.Unlock()
	c.neverClaimLabels = copied
}

// neverClaimLabelSet returns the configured never-claim set (nil = default).
func (c *Client) neverClaimLabelSet() []string {
	if c == nil {
		return nil
	}
	c.neverClaimMu.RLock()
	defer c.neverClaimMu.RUnlock()
	return c.neverClaimLabels
}

// issueUmbrellaLabel returns the never-claim label labels carry under the
// set SetNeverClaimLabels last installed (package default until then), or "".
func issueUmbrellaLabel(labels []string) string {
	var never []string
	if p := umbrellaLabels.Load(); p != nil {
		never = *p
	}
	return claims.NeverClaimLabel(labels, never)
}

// issueClaimParkLabel returns the park label that puts labels out of the
// agent queue on a person's decision — the issue parking stage
// (stage/045-needs-decision, or the operator's renamed spelling), the agent
// escalation labels (needs-spec, needs-reporter-confirmation, …) and the
// retired decision labels — or "". needs-human and the hold family are
// reported separately by the caller.
func (c *Client) issueClaimParkLabel(labels []string) string {
	if label := parkedOnPersonLabel(IssueStageInput{Labels: labels}); label != "" {
		return label
	}
	if renamed := c.needsDecisionLabel(); renamed != "" {
		for _, l := range labels {
			if trimmed := strings.TrimSpace(l); strings.EqualFold(trimmed, renamed) {
				return trimmed
			}
		}
	}
	return ""
}

// IssueClaimBlockReason checks current forge labels before an automated claim.
// It is the admission gate for EVERY automated claim producer — the ledger
// (start signals, kick listings, forced takeovers, renewals), the `claim`
// relay request and the contributor lane. It refuses:
//
//   - a pull request (ClaimBlockReasonPullRequest);
//   - needs-human and the hold family, like a hold, even when forced;
//   - an issue blocked on an unmerged PR (`blocked` label, hivecommons/hive#13093):
//     the work it names is not on any branch yet;
//   - an issue parked on a person's decision (stage/045-needs-decision, the
//     agent escalation labels, the retired decision labels): a maintainer
//     holds it, so nobody starts on it until the un-park;
//   - an issue wearing a never-claim label (governor.claims.never_claim_labels,
//     default `epic`): an umbrella issue is the list of work, not the work.
//
// Lookup errors are returned so callers cannot mistake unknown labels for
// permission to claim. Human claims do not use this gate. A producer about
// to start fresh work uses IssueDispatchBlockReason, which adds the open-PR
// guard (hivecommons/hive#14799).
func (c *Client) IssueClaimBlockReason(ctx context.Context, repo string, number int) (string, error) {
	gate, err := c.issueClaimGate(ctx, repo, number)
	return gate.reason, err
}

// issueClaimGateResult is what one issue read told the claim gate: the
// label-based refusal, the labels for the dispatch gate's open-PR step, and
// whether the forge reports the issue closed.
type issueClaimGateResult struct {
	reason string
	labels []string
	closed bool
}

// issueClaimGate reads the issue once and applies every label-based refusal.
func (c *Client) issueClaimGate(ctx context.Context, repo string, number int) (issueClaimGateResult, error) {
	if c == nil || c.client == nil {
		return issueClaimGateResult{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return issueClaimGateResult{}, fmt.Errorf("checking issue claim labels: %w", err)
	}
	if issue.IsPullRequest() {
		return issueClaimGateResult{reason: ClaimBlockReasonPullRequest}, nil
	}
	res := issueClaimGateResult{
		labels: extractLabels(issue.Labels),
		closed: strings.EqualFold(issue.GetState(), "closed"),
	}
	labels := res.labels
	switch {
	case c.hasNeedsHumanLabel(labels):
		res.reason = "needs-human"
	case c.isHeld(labels):
		res.reason = "hold"
	case hasBlockedLabel(labels):
		res.reason = blockedClaimReason(issue.GetBody())
	}
	if res.reason != "" {
		return res, nil
	}
	if label := c.issueClaimParkLabel(labels); label != "" {
		res.reason = ClaimBlockReasonParkedPrefix + label
	} else if label := claims.NeverClaimLabel(labels, c.neverClaimLabelSet()); label != "" {
		res.reason = ClaimBlockReasonNeverClaimPrefix + label
	}
	return res, nil
}

// IssueDispatchBlockReason is IssueClaimBlockReason for a producer about to
// START work on an issue — a fresh claim with no PR behind it — plus the
// open-PR guard (hivecommons/hive#14799): an OPEN pull request already
// linking the issue with a closing keyword refuses the dispatch
// (ClaimBlockReasonOpenPRPrefix), labels the issue `has-open-pr` and logs
// the skip. Renewals, PR-backed start signals and the release tick keep
// using IssueClaimBlockReason: the agent whose own PR links the issue holds
// its claim legitimately, and a guard that released it would un-stage the
// very PR it exists to protect. Off with scanner.skip_issues_with_open_pr:
// false; a failed listing fails open with a warning.
func (c *Client) IssueDispatchBlockReason(ctx context.Context, repo string, number int) (string, error) {
	gate, err := c.issueClaimGate(ctx, repo, number)
	if err != nil || gate.reason != "" {
		return gate.reason, err
	}
	// The same read says whether the issue is still open: a closing keyword
	// on a PR that merged since the snapshot was listed closes it, and a
	// closed issue is not work to start (hivecommons/hive#14814).
	if gate.closed {
		return ClaimBlockReasonIssueClosed, nil
	}
	// Last because it is the only step that costs a listing. The eval
	// cycle's claim ledger learns of a hand-opened PR one scan late; this
	// reads the forge now.
	return c.openPRLinkBlockReason(ctx, repo, number, gate.labels), nil
}

// issueIsPullRequest reports whether repo#number is a pull request. The
// comment relay's needs-decision park uses it to refuse PR targets (#12334):
// the needs-decision label is an issue-queue signal, and on a PR it would
// hard-suppress the PR instead. A lookup error is returned so the caller
// retries rather than parking an item it could not read.
func (c *Client) issueIsPullRequest(ctx context.Context, repo string, number int) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return false, fmt.Errorf("reading %s#%d before parking it: %w", repo, number, err)
	}
	return issue.IsPullRequest(), nil
}

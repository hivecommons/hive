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
//   - an issue parked on a person's decision (stage/045-needs-decision, the
//     agent escalation labels, the retired decision labels): a maintainer
//     holds it, so nobody starts on it until the un-park;
//   - an issue wearing a never-claim label (governor.claims.never_claim_labels,
//     default `epic`): an umbrella issue is the list of work, not the work.
//
// Lookup errors are returned so callers cannot mistake unknown labels for
// permission to claim. Human claims do not use this gate.
func (c *Client) IssueClaimBlockReason(ctx context.Context, repo string, number int) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return "", fmt.Errorf("checking issue claim labels: %w", err)
	}
	if issue.IsPullRequest() {
		return ClaimBlockReasonPullRequest, nil
	}
	labels := extractLabels(issue.Labels)
	if hasIssueNeedsHumanLabel(labels) {
		return "needs-human", nil
	}
	if c.isHeld(labels) {
		return "hold", nil
	}
	if label := c.issueClaimParkLabel(labels); label != "" {
		return ClaimBlockReasonParkedPrefix + label, nil
	}
	if label := claims.NeverClaimLabel(labels, c.neverClaimLabelSet()); label != "" {
		return ClaimBlockReasonNeverClaimPrefix + label, nil
	}
	return "", nil
}

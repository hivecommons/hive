package automerge

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// The decision gate (auto_merge.decision_gate) keeps a person in front of
// every automated merge of a credential, keystore, auth or secret change,
// and of a sentinel-flagged PR, whoever wrote it.
//
// It exists because the trusted-author lane merged a credential-path PR with
// no person deciding: the author's owner role admitted it, the sentinel
// finding was notice-only for a trusted author, the swarm's changes_requested
// is a COMMENTED review (so hasOutstandingChangesRequested never sees it),
// the review gate defaults to shadow, and auto_merge.human_merge_paths is
// empty unless an operator fills it in. Each of those is a reasonable default
// on its own; together they let a credential change land on CI alone.
//
// The gate runs in every lane that merges without a person (App self-merge,
// trusted bot, trusted author, contributor), after CI is green and before the
// current-base gate, the native queue and the merge call. The author's role
// is never the decision. A decision is bound to the exact head:
//
//   - a trusted human's APPROVED review on head (latestTrustedHumanApproval);
//   - an operator admission or a recorded `/hive decision approve` for head
//     by a trusted merger (operatorAdmission);
//   - the dashboard's head-bound Approve comment (dashboardApprovalForHead).
//
// A push moves the head and drops the decision, so the new head is held again.

// Skip reasons reported by the decision gate.
const (
	autoMergeReasonSensitivePathDecision = prstage.AutoMergeReasonSensitivePathDecision
	autoMergeReasonSensitivePathEvidence = prstage.AutoMergeReasonSensitivePathEvidence
	autoMergeReasonSentinelDecision      = prstage.AutoMergeReasonSentinelDecision
)

// decisionGateConfigKey is the config key named in the gate's log lines.
const decisionGateConfigKey = "auto_merge.decision_gate"

// DecisionGatePolicy is the live auto_merge.decision_gate policy for one
// owner/repo. The zero value is off.
type DecisionGatePolicy struct {
	// Enabled turns the gate on.
	Enabled bool
	// Paths are the globs whose change needs a decision.
	Paths []string
	// Sentinel makes the sentinel alert label need a decision in every
	// lane, trusted authors included.
	Sentinel bool
}

// SetDecisionGate installs (or, with nil, removes) the per-repo
// auto_merge.decision_gate lookup. fn is called on every evaluation with
// "owner/repo", so a config reload applies without restarting the sweep.
func (c *Engine) SetDecisionGate(fn func(repo string) DecisionGatePolicy) {
	if c == nil {
		return
	}
	c.decisionGateMu.Lock()
	defer c.decisionGateMu.Unlock()
	c.decisionGatePolicy = fn
}

func (c *Engine) currentDecisionGate(repo string) DecisionGatePolicy {
	if c == nil {
		return DecisionGatePolicy{}
	}
	c.decisionGateMu.RLock()
	fn := c.decisionGatePolicy
	c.decisionGateMu.RUnlock()
	if fn == nil {
		return DecisionGatePolicy{}
	}
	return fn(repo)
}

// decisionGate returns "" when pr's head may merge. Otherwise it returns the
// reason the head waits for a person: the sentinel alert label is on it, or
// it touches a decision path, and nobody has decided on head. When the
// policy has paths and the complete changed-file list cannot be read it
// fails closed with autoMergeReasonSensitivePathEvidence and the error.
func (c *Engine) decisionGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, head string) (string, error) {
	policy := c.currentDecisionGate(owner + "/" + repo)
	if !policy.Enabled || pr == nil {
		return "", nil
	}
	reason := ""
	var matches []string
	if policy.Sentinel && c.sentinelLabelPresent(labelNames(pr.Labels)) {
		reason = autoMergeReasonSentinelDecision
	}
	if reason == "" && len(policy.Paths) > 0 {
		files, err := ListChangedFiles(ctx, c.gh, owner, repo, pr)
		if err != nil {
			return autoMergeReasonSensitivePathEvidence, err
		}
		matches = intent.HumanMergePathMatches(files, policy.Paths)
		if len(matches) > 0 {
			reason = autoMergeReasonSensitivePathDecision
		}
	}
	if reason == "" {
		return "", nil
	}
	number := pr.GetNumber()
	if login, ok := c.humanDecisionForHead(ctx, owner, repo, number, head); ok {
		c.info("automerge decision gate cleared by a person's decision on this head", "repo", displayRepo, "pr", number, "head", head, "decided_by", login, "reason", reason, "paths", matches, "config", decisionGateConfigKey)
		return "", nil
	}
	if c.markTerminalHoldLogged(owner, repo, number, head, "decision-gate:"+reason) {
		c.info("automerge decision gate holding merge until a person decides on this head", "repo", displayRepo, "pr", number, "head", head, "reason", reason, "paths", matches, "config", decisionGateConfigKey)
	}
	return reason, nil
}

// humanDecisionForHead returns the person whose decision admits head, or
// ok=false when nobody decided on exactly this head.
func (c *Engine) humanDecisionForHead(ctx context.Context, owner, repo string, number int, head string) (string, bool) {
	if c == nil || strings.TrimSpace(head) == "" {
		return "", false
	}
	if login, ok := c.latestTrustedHumanApproval(ctx, owner, repo, number, head); ok {
		return login, true
	}
	if login := c.operatorAdmission(ctx, owner, repo, number, head); login != "" {
		return login, true
	}
	return c.dashboardApprovalForHead(ctx, owner, repo, number, head)
}

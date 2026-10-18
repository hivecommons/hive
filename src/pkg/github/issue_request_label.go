package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The standalone `label` relay operation (hivecommons/hive#9587).
//
// Labeling an existing issue or PR was the one everyday write with no relay:
// an agent could only reach it with a direct `gh issue edit --add-label`,
// outside the audited write surface. The issue-request watcher's `label` kind
// closes that gap — same file-UID authorizer, same lane allowlist, same repo
// pause and repo scope, same redacted audit entry as every other relay
// operation.
//
// It is NOT a general-purpose label API. A label is an input to hive
// automation: `lgtm` queues a merge, a hold label blocks one, the claim
// namespace records ownership, and the human-decision labels record a
// person's verdict. An agent that could add or remove those through this
// relay would be able to decide its own merges, so reserved labels are
// refused here, before any GitHub call, in both directions.

// reservedExactLabels are the control labels no agent may add or remove
// through the label relay, whatever its lane allowlist says. Each one is read
// by hive automation (or records a human's decision) rather than describing
// the item it sits on.
var reservedExactLabels = []string{
	// Human decisions and workflow gates.
	HumanAckLabel,        // approved-direction: a person endorsed the direction
	"design-approved",    // config.DefaultDesignApprovedLabel
	"needs-human",        // escalation to a person; agents may ADD it (agentMayAddLabel), never remove it
	"needs-decision",     // retired park label (hivecommons/hive#12683); never written again
	"needs-direction",    // retired park label (hivecommons/hive#12683); never written again
	"blocked",            // workflow skip marker
	AutoMergeQueuedLabel, // lgtm: queues the merge
}

// reservedLabelPrefixes are namespaces owned by the hive itself: claim
// ownership (`hive/claimed-by-<agent>`), the PR-claim bookkeeping labels and
// most verification state labels are written by hive code paths that read
// them back as fact, and the `stage/*` family the reconcilers compute.
var reservedLabelPrefixes = []string{"hive/", stagelabels.Prefix}

// agentRecordableHiveStateLabels are the narrow exceptions agents may add
// because hive explicitly asks for them as the agent's audited verdict. The
// relay still refuses removing them and refuses every other hive/* label.
var agentRecordableHiveStateLabels = []string{
	VerifiedOpenLabel,
}

// agentMayAddLabel reports whether an agent may ADD label through the relay
// even though it is reserved: the recordable hive-state verdicts, the
// hive's configured needs-decision label (`stage/045-needs-decision` by
// default, the issue parking bay), the PR-only `stage/085-needs-decision`,
// and `needs-human`. Parking an item on a maintainer decision is
// ADR-0019's escalation move (it replaced the retired `needs-direction` and
// `needs-decision` labels, hivecommons/hive#12683); handing an item to a
// person with `needs-human` is the same move for work a human must do or
// consent to. In both cases the agent gives up work; it never decides its
// own merge. Removing either label — the un-park — stays with people: a
// maintainer's `/hive approve` / `/hive decision` for the decision label, a
// person's hand for `needs-human`. The relay does not know whether the
// target is an issue or a PR, so both parking stages pass; the stage
// reconcilers then clamp the label to the item's kind (an issue handed 085
// is re-staged to 045, a PR handed 045 is re-staged to 085).
func (c *Client) agentMayAddLabel(label string) bool {
	if agentRecordableHiveStateLabel(label) {
		return true
	}
	name := strings.TrimSpace(label)
	return strings.EqualFold(name, c.needsDecisionLabel()) ||
		strings.EqualFold(name, PRNeedsDecisionStageLabel) ||
		strings.EqualFold(name, issueNeedsHumanLabel)
}

// reservedLabelForAgents reports whether label is refused to the label relay.
// autoMergeLabel is the hive's configured merge-queue label, which an
// operator may have renamed away from the default.
//
// Matching is case-insensitive on the trimmed name because GitHub label names
// are compared case-insensitively when they are applied.
func reservedLabelForAgents(label, autoMergeLabel string) bool {
	name := strings.ToLower(strings.TrimSpace(label))
	if name == "" {
		return false
	}
	for _, prefix := range reservedLabelPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, reserved := range reservedExactLabels {
		if name == strings.ToLower(strings.TrimSpace(reserved)) {
			return true
		}
	}
	if am := strings.ToLower(strings.TrimSpace(autoMergeLabel)); am != "" && name == am {
		return true
	}
	for _, hold := range HoldLabels {
		if name == strings.ToLower(strings.TrimSpace(hold)) {
			return true
		}
	}
	return false
}

func agentRecordableHiveStateLabel(label string) bool {
	name := strings.ToLower(strings.TrimSpace(label))
	for _, allowed := range agentRecordableHiveStateLabels {
		if name == strings.ToLower(strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

// normalizeLabelList trims, drops empties and de-duplicates a requested label
// list (case-insensitively), keeping the order the agent asked for.
func normalizeLabelList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			label := strings.TrimSpace(part)
			if label == "" {
				continue
			}
			key := strings.ToLower(label)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, label)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reservedLabelRefusal returns the refusal reason, and true, when a label
// request names any reserved label in either direction. It fails closed: one
// reserved label refuses the whole request rather than silently applying the
// rest, so an agent can never learn which part of a batch got through.
func (c *Client) reservedLabelRefusal(add, remove []string) (string, bool) {
	autoMerge := c.AutoMergeLabel()
	var bad []string
	seen := map[string]bool{}
	for _, label := range add {
		if c.agentMayAddLabel(label) {
			continue
		}
		if !reservedLabelForAgents(label, autoMerge) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(label))
		if seen[key] {
			continue
		}
		seen[key] = true
		bad = append(bad, strings.TrimSpace(label))
	}
	for _, label := range remove {
		if !reservedLabelForAgents(label, autoMerge) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(label))
		if seen[key] {
			continue
		}
		seen[key] = true
		bad = append(bad, strings.TrimSpace(label))
	}
	if len(bad) == 0 {
		return "", false
	}
	sort.Strings(bad)
	return "label request names hive-controlled label(s) " + strings.Join(bad, ", ") +
		": these drive hive automation or record a human decision and cannot be set by an agent. " +
		"Use the claim relay for ownership, and ask a maintainer for merge, hold and decision labels.", true
}

// reparkDecisionRefusal returns the refusal reason, and true, when a label
// request would re-park an issue on a maintainer decision that a maintainer
// has already given (hivecommons/hive#12680 / #12681, 2026-10-17: the owner
// replied `/hive decision … Agents stand down`, the sweep applied
// `approved-direction`, and a contributor agent then read that decision and
// re-parked the issue asking for one). The park is suppressed when the issue
// carries `approved-direction` AND the hive's own un-park reply
// (`<!-- hive:unpark-reply:… id=… -->`) is on the timeline with no newer,
// different `/hive decision` after it — that is, the decision is unchanged.
// A newer `/hive decision` with new text means direction is changing; the
// un-park sweep answers it, so the park is allowed through. The reply tells
// the agent what to do instead: follow the decision, or — when it reserves
// the work for people — add `needs-human` and unclaim. A lookup failure is
// returned so the caller retries rather than reading "unknown" as allowed.
func (c *Client) reparkDecisionRefusal(ctx context.Context, repo string, number int, add []string) (string, bool, error) {
	if number <= 0 || !containsDecisionParkLabel(add, c.needsDecisionLabel()) {
		return "", false, nil
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return "", false, fmt.Errorf("checking issue park labels: %w", err)
	}
	labels := extractLabels(issue.Labels)
	if !labelPresent(labels, HumanAckLabel) {
		return "", false, nil
	}
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return "", false, err
	}
	decision, at, ok := c.lastRecordedUnparkDecision(comments)
	if !ok {
		return "", false, nil
	}
	if hasNewerDifferentDecisionCommand(comments, at, decision) {
		return "", false, nil
	}
	return fmt.Sprintf("a maintainer already decided on %s#%d (recorded by Hive: %q) and `%s` is applied; "+
		"re-parking it on `%s` asks for a decision that has been given. Follow the decision. "+
		"If it reserves the work for people, add `%s` through this relay and `/unclaim` instead.",
		repo, number, decision, HumanAckLabel, c.needsDecisionLabel(), issueNeedsHumanLabel), true, nil
}

func containsDecisionParkLabel(labels []string, configured string) bool {
	for _, label := range labels {
		name := strings.TrimSpace(label)
		if name == "" || strings.EqualFold(name, issueNeedsHumanLabel) {
			continue
		}
		if strings.EqualFold(name, strings.TrimSpace(configured)) {
			return true
		}
		for _, parking := range issueParkingLabels {
			if strings.EqualFold(name, parking) && !strings.EqualFold(parking, issueNeedsHumanLabel) {
				return true
			}
		}
	}
	return false
}

// unparkDecisionLinePrefix is how the hive's accepted un-park reply records
// the decision text (renderUnparkAcceptedReply).
const unparkDecisionLinePrefix = "Decision: "

// lastRecordedUnparkDecision reads the decision text out of the newest
// un-park reply the hive itself posted.
func (c *Client) lastRecordedUnparkDecision(comments []*gh.IssueComment) (decision string, at time.Time, ok bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		cm := comments[i]
		if cm == nil || !c.markerAuthorTrusted(cm.GetUser()) || !strings.Contains(cm.GetBody(), unparkReplyMarkerPrefix) {
			continue
		}
		for _, line := range strings.Split(cm.GetBody(), "\n") {
			if idx := strings.Index(line, unparkDecisionLinePrefix); idx >= 0 {
				return strings.TrimSpace(line[idx+len(unparkDecisionLinePrefix):]), cm.GetCreatedAt().Time, true
			}
		}
	}
	return "", time.Time{}, false
}

// hasNewerDifferentDecisionCommand reports whether a `/hive decision` whose
// text differs from recorded was posted after the recorded reply.
func hasNewerDifferentDecisionCommand(comments []*gh.IssueComment, after time.Time, recorded string) bool {
	for _, cm := range comments {
		if cm == nil || !cm.GetCreatedAt().Time.After(after) {
			continue
		}
		cmd, ok := parseUnparkCommand(cm.GetBody())
		if !ok || cmd.Kind != unparkCommandDecision {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(cmd.Decision), strings.TrimSpace(recorded)) {
			return true
		}
	}
	return false
}

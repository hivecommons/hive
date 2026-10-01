package github

import (
	"sort"
	"strings"
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
	"needs-human",        // escalation to a person
	"needs-decision",     // blocked on a human decision
	"blocked",            // workflow skip marker
	AutoMergeQueuedLabel, // lgtm: queues the merge
}

// reservedLabelPrefixes are namespaces owned by the hive itself: claim
// ownership (`hive/claimed-by-<agent>`), the PR-claim bookkeeping labels and
// most verification state labels are written by hive code paths that read
// them back as fact.
var reservedLabelPrefixes = []string{"hive/"}

// agentRecordableHiveStateLabels are the narrow exceptions agents may add
// because hive explicitly asks for them as the agent's audited verdict. The
// relay still refuses removing them and refuses every other hive/* label.
var agentRecordableHiveStateLabels = []string{
	VerifiedOpenLabel,
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
		if agentRecordableHiveStateLabel(label) {
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

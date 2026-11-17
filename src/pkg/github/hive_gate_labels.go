package github

import (
	"context"
	"strings"
	"sync"
)

// Hive gate labels (hivecommons/hive#18468).
//
// `hold` means "do not merge / do not act" in most projects, and a `hold` a
// person adds still means exactly that to the hive: hands-off, never
// implemented, decomposed, relayed or merged, and never removed by the hive.
// The hive itself no longer applies `hold`. When one of its own gates needs a
// person it applies one of two self-explanatory labels instead, each of which
// blocks merges exactly like `hold` (exact-match members of the hold set, see
// HasHoldLabelWith):
//
//   - ready-for-review: the hive's work is done and the PR waits for a
//     maintainer to review it (the ACMM level gate at L3-L5).
//   - decision-needed: the hive is blocked on a human decision (the
//     reporter-trust and self-authorization gates, the hold guard's re-hold
//     after a branch moved under an approval, an owner-only delegated
//     decision).
//
// Both names are configurable through governor.labels.ready_for_review and
// governor.labels.decision_needed; SetHiveGateLabelNames installs them.
const (
	DefaultReadyForReviewLabel = "ready-for-review"
	DefaultDecisionNeededLabel = "decision-needed"

	// ReadyForReviewLabelDescription / DecisionNeededLabelDescription are the
	// GitHub label descriptions: each says who acts next.
	ReadyForReviewLabelDescription = "Hive's work is done; a maintainer reviews, then removes this label to let it merge."
	DecisionNeededLabelDescription = "Hive is blocked on a human decision; a maintainer decides (see hive's comment)."

	readyForReviewLabelColor = "0e8a16"
	decisionNeededLabelColor = "d93f0b"

	// LegacyHiveHoldLabel is the literal `hold` the hive's gates applied
	// before #18468. The hive never writes it again; MigrateHiveAppliedHolds
	// swaps the ones it can prove it applied for the gate label above.
	LegacyHiveHoldLabel = "hold"
)

var hiveGateLabelNames = struct {
	sync.RWMutex
	ready    string
	decision string
}{ready: DefaultReadyForReviewLabel, decision: DefaultDecisionNeededLabel}

// SetHiveGateLabelNames installs the configured gate label names. A blank
// (or `hold`) name keeps the default: the hive must never apply `hold`.
func SetHiveGateLabelNames(readyForReview, decisionNeeded string) {
	readyForReview = sanitizeGateLabelName(readyForReview, DefaultReadyForReviewLabel)
	decisionNeeded = sanitizeGateLabelName(decisionNeeded, DefaultDecisionNeededLabel)
	hiveGateLabelNames.Lock()
	defer hiveGateLabelNames.Unlock()
	hiveGateLabelNames.ready = readyForReview
	hiveGateLabelNames.decision = decisionNeeded
}

func sanitizeGateLabelName(name, def string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, LegacyHiveHoldLabel) {
		return def
	}
	return name
}

// ReadyForReviewLabelName is the label the ACMM level gate applies.
func ReadyForReviewLabelName() string {
	hiveGateLabelNames.RLock()
	defer hiveGateLabelNames.RUnlock()
	return hiveGateLabelNames.ready
}

// DecisionNeededLabelName is the label the hive's decision gates apply.
func DecisionNeededLabelName() string {
	hiveGateLabelNames.RLock()
	defer hiveGateLabelNames.RUnlock()
	return hiveGateLabelNames.decision
}

// HiveGateLabels returns the configured gate labels, ready-for-review first.
func HiveGateLabels() []string {
	return []string{ReadyForReviewLabelName(), DecisionNeededLabelName()}
}

// IsHiveGateLabel reports whether label is one of the configured gate labels
// or one of their defaults (exact, case-insensitive). The defaults stay
// recognised after a rename so a label applied under the old name keeps
// blocking until a person removes it.
func IsHiveGateLabel(label string) bool {
	label = strings.TrimSpace(label)
	if label == "" {
		return false
	}
	for _, gate := range append(HiveGateLabels(), DefaultReadyForReviewLabel, DefaultDecisionNeededLabel) {
		if strings.EqualFold(label, gate) {
			return true
		}
	}
	return false
}

// HasHiveGateLabel reports whether labels carry a hive gate label.
func HasHiveGateLabel(labels []string) bool {
	for _, label := range labels {
		if IsHiveGateLabel(label) {
			return true
		}
	}
	return false
}

// hiveGateLabelDefinition returns the colour and description for a gate label.
func hiveGateLabelDefinition(name string) (color, description string, ok bool) {
	switch {
	case strings.EqualFold(name, ReadyForReviewLabelName()) || strings.EqualFold(name, DefaultReadyForReviewLabel):
		return readyForReviewLabelColor, ReadyForReviewLabelDescription, true
	case strings.EqualFold(name, DecisionNeededLabelName()) || strings.EqualFold(name, DefaultDecisionNeededLabel):
		return decisionNeededLabelColor, DecisionNeededLabelDescription, true
	}
	return "", "", false
}

// EnsureHiveGateLabels creates the gate labels in repo when they are
// missing, with the description that says who acts next. It runs once per
// repository from the boot migration (MigrateHiveAppliedHolds); a label that
// still does not exist when a gate fires is auto-created by GitHub on the
// AddLabels call, without the description (an existing label is never
// rewritten: a maintainer may have chosen their own colour or wording).
func (c *Client) EnsureHiveGateLabels(ctx context.Context, repo string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	var firstErr error
	for _, label := range HiveGateLabels() {
		color, desc, _ := hiveGateLabelDefinition(label)
		if err := c.EnsureIssueLabel(ctx, repo, label, color, desc); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

package config

import "testing"

// hivecommons/hive#18468: the gate label names are configurable, and no
// configuration can make the hive apply `hold` itself.
func TestHiveGateLabelConfigNeverResolvesToHold(t *testing.T) {
	var l LabelsConfig
	if l.ReadyForReviewLabelOrDefault() != DefaultReadyForReviewLabel || l.DecisionNeededLabelOrDefault() != DefaultDecisionNeededLabel {
		t.Fatalf("defaults = %q/%q", l.ReadyForReviewLabelOrDefault(), l.DecisionNeededLabelOrDefault())
	}
	l = LabelsConfig{ReadyForReview: " hold ", DecisionNeeded: "HOLD"}
	if l.ReadyForReviewLabelOrDefault() != DefaultReadyForReviewLabel || l.DecisionNeededLabelOrDefault() != DefaultDecisionNeededLabel {
		t.Fatalf("hold override resolved to %q/%q, want the defaults", l.ReadyForReviewLabelOrDefault(), l.DecisionNeededLabelOrDefault())
	}
	l = LabelsConfig{ReadyForReview: "awaiting-review", DecisionNeeded: "awaiting-decision"}
	if l.ReadyForReviewLabelOrDefault() != "awaiting-review" || l.DecisionNeededLabelOrDefault() != "awaiting-decision" {
		t.Fatalf("overrides = %q/%q", l.ReadyForReviewLabelOrDefault(), l.DecisionNeededLabelOrDefault())
	}
}

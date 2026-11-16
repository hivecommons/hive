package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/holdguard"
)

// hivecommons/hive#18468: the hive's gate labels block a merge exactly like
// a person's `hold`, and the hold guard re-holds with decision-needed.
func TestHiveGateLabelsBlockMergeLikeHold(t *testing.T) {
	for _, label := range []string{"hold", github.DefaultReadyForReviewLabel, github.DefaultDecisionNeededLabel} {
		if got := automergeBlockLabelReason([]string{"kind/bug", label}, true); got != "label:hold" {
			t.Errorf("automergeBlockLabelReason(%q) = %q, want label:hold", label, got)
		}
	}
	if got := automergeBlockLabelReason([]string{"not-ready-for-review"}, true); got != "" {
		t.Errorf("look-alike label blocked with %q", got)
	}
}

func TestHoldGuardNeverReappliesHold(t *testing.T) {
	if holdguard.ReHoldLabel == github.LegacyHiveHoldLabel {
		t.Fatal("the hold guard re-applies hold; the hive must never apply hold itself")
	}
	if holdguard.ReHoldLabel != github.DefaultDecisionNeededLabel {
		t.Fatalf("holdguard.ReHoldLabel = %q, want %q", holdguard.ReHoldLabel, github.DefaultDecisionNeededLabel)
	}
}

package config

import "testing"

// Saving one half of review_feedback must not drop the other (#19320).
func TestReviewFeedbackSettings_HalvesKeptApart(t *testing.T) {
	c := &Config{}
	two := 3
	c.SetReviewRetrySettings(ReviewRetrySettings{MaxRetries: &two, Backoff: "2h"})
	on := true
	c.SetReviewRulesSettings(ReviewRulesSettings{Enabled: &on})
	got := c.ReviewRetrySettingsSnapshot()
	if got.MaxRetries == nil || *got.MaxRetries != 3 || got.Backoff != "2h" {
		t.Fatalf("retry lost after saving rules: %+v", got)
	}
	c.SetReviewRulesSettings(ReviewRulesSettings{})
	if got := c.ReviewRetrySettingsSnapshot(); got.MaxRetries == nil {
		t.Fatal("retry lost after clearing rules")
	}
	prev := c.SetReviewRetrySettings(ReviewRetrySettings{})
	if prev.MaxRetries == nil || *prev.MaxRetries != 3 {
		t.Fatalf("previous value = %+v", prev)
	}
	if c.ReviewFeedback != nil {
		t.Fatalf("empty block kept: %+v", c.ReviewFeedback)
	}
	// The snapshot is a copy.
	c.SetReviewRetrySettings(ReviewRetrySettings{MaxRetries: &two})
	snap := c.ReviewRetrySettingsSnapshot()
	*snap.MaxRetries = 9
	if *c.ReviewRetrySettingsSnapshot().MaxRetries != 3 {
		t.Fatal("snapshot aliases the config")
	}
}

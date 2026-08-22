package agent

import "testing"

func TestPaneShowsQuotaExhausted(t *testing.T) {
	cases := []string{
		"You have exceeded your monthly quota",
		"You've used all your Copilot Free chat requests for the month",
		`API Error: 429 {"error":{"type":"budget_exceeded","message":"Budget has been exceeded!"}}`,
		"provider spending limit reached (682 refused calls since Fri)",
		"litellm refused the request on a spending limit (429)",
		"Oh no! It looks like you've gone over your budget allowance of 1000 Bobcoins. Give me feedback using about my performance and I'll make sure you get rewarded with some extra credits!",
		"💰 1000.01/1000 (0%) | Current 💬: 0.00",
	}
	for _, line := range cases {
		if !paneShowsQuotaExhausted([]string{line}) {
			t.Fatalf("paneShowsQuotaExhausted(%q) = false, want true", line)
		}
	}
}

func TestQuotaExhaustionIsNotLoginPrompt(t *testing.T) {
	line := "Please run /login · You've used all your Copilot Free chat requests for the month"
	if !paneShowsQuotaExhausted([]string{line}) {
		t.Fatalf("quota line was not detected")
	}
	if paneShowsLoginPrompt([]string{line}) {
		t.Fatalf("quota exhaustion must not be classified as a login prompt")
	}
}

func TestPaneShowsQuotaExhaustedIgnoresOrdinaryRateLimit(t *testing.T) {
	if paneShowsQuotaExhausted([]string{"API Error: 429 rate limit exceeded, try again later"}) {
		t.Fatalf("ordinary transient rate limit must not be quota exhaustion")
	}
}

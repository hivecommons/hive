package rotation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexResetCreditsAvailableCount pins #10596: the count is read from the
// existing rateLimits payload, and null/missing/malformed stay unknown (nil)
// rather than collapsing to 0.
func TestCodexResetCreditsAvailableCount(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "codex_rate_limits_reset_credits.json"))
	if err != nil {
		t.Fatal(err)
	}
	const present = `"rateLimitResetCredits": {"availableCount": 3}`
	intp := func(n int) *int { return &n }
	tests := []struct {
		name  string
		block string
		want  *int
	}{
		{"count present", present, intp(3)},
		{"zero", `"rateLimitResetCredits": {"availableCount": 0}`, intp(0)},
		{"null count", `"rateLimitResetCredits": {"availableCount": null}`, nil},
		{"null object", `"rateLimitResetCredits": null`, nil},
		{"missing object", `"unrelated": 1`, nil},
		{"missing count", `"rateLimitResetCredits": {}`, nil},
		{"malformed string", `"rateLimitResetCredits": {"availableCount": "three"}`, nil},
		{"malformed fraction", `"rateLimitResetCredits": {"availableCount": 1.5}`, nil},
		{"malformed negative", `"rateLimitResetCredits": {"availableCount": -2}`, nil},
		{"malformed object type", `"rateLimitResetCredits": 7`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Replace(string(raw), present, tc.block, 1)
			h, err := codexHeadroom("openai", 80, json.RawMessage(payload))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (h.ResetCreditsAvailable == nil) != (tc.want == nil) || (tc.want != nil && *h.ResetCreditsAvailable != *tc.want) {
				t.Fatalf("Headroom.ResetCreditsAvailable = %v, want %v", h.ResetCreditsAvailable, tc.want)
			}
			b, err := json.Marshal(HeadroomToContributorReading(h))
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatal(err)
			}
			got, has := out["reset_credits_available"]
			if tc.want == nil {
				if has {
					t.Fatalf("unknown count was published as %v", got)
				}
			} else if !has || int(got.(float64)) != *tc.want {
				t.Fatalf("published reset_credits_available = %v, want %d", got, *tc.want)
			}
		})
	}
}

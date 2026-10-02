package dashboard

import (
	"strings"
	"testing"
)

func TestContinuousModeControlsAndBadgesRender(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-key="continuous"`,
		`data-key="continuousCooldown"`,
		`data-key="continuousBudgetPct"`,
		`Continuous</b> — re-kick when a session ends`,
		`∞ continuous`,
		`held by budget guard`,
		`Continuous mode — re-kick after session end plus cool-down`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard HTML missing %q", want)
		}
	}
}

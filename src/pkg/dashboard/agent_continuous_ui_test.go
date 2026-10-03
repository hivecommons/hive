package dashboard

import (
	"strings"
	"testing"
)

func TestContinuousModeControlsAndBadgesRender(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`Continuous in all active modes`,
		`data-action="toggleContinuousAllActiveModes"`,
		`data-key="continuousCooldown"`,
		`data-key="continuousBudgetPct"`,
		`role="radiogroup"`,
		`aria-checked="${checked ? 'true' : 'false'}"`,
		`data-tab="Cadences"`,
		`∞ continuous`,
		`held by budget guard`,
		`Cool-down and budget guard apply to every mode set to Continuous.`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard HTML missing %q", want)
		}
	}
	for _, old := range []string{
		`function setAgentContinuous(`,
		`data-action="setAgentContinuous"`,
		`gov-continuous-switch`,
		`<th>Continuous</th>`,
	} {
		if strings.Contains(html, old) {
			t.Fatalf("dashboard HTML still contains removed continuous table toggle %q", old)
		}
	}
}

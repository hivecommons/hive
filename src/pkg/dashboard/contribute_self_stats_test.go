package dashboard

import (
	"strings"
	"testing"
)

func TestContributeOperationsRendersYourContributionPanel(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`id="cc-your-contribution-card"`,
		`<h3>Your contribution</h3>`,
		`Issues worked (24h)`,
		`Issues worked (total)`,
		`PRs opened`,
		`function ccLoadYourContribution()`,
		`/api/contributors/`,
		`total_tasks_completed_with_pr`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing contribution stats marker %q", want)
		}
	}

	ops := strings.Index(body, `id="tab-ops"`)
	card := strings.Index(body, `id="cc-your-contribution-card"`)
	lb := strings.Index(body, `id="tab-leaderboard"`)
	if ops < 0 || card <= ops || (lb >= 0 && card >= lb) {
		t.Errorf("your contribution card is not inside Operations (ops=%d card=%d lb=%d)", ops, card, lb)
	}
}

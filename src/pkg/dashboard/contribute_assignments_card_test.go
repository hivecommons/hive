package dashboard

import (
	"strings"
	"testing"
)

// TestContributeYourAssignmentsCardWiring pins the Operations "Your
// assignments" card (#13454): a hidden-by-default, read-only card fed by the
// caller's own relay_* rows from GET /api/audit?contributor=<login>.
func TestContributeYourAssignmentsCardWiring(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		`id="cc-assign-card" data-ops-card="assignments" hidden>`,
		`<h3>Your assignments</h3>`,
		`id="cc-assign-body"`,
		`function ccLoadAssignments(force)`,
		`fetch('/api/audit?contributor='+encodeURIComponent(who))`,
		`indexOf('relay_')===0`,
		`if(!r.ok)return null;`,
		`function ccRenderAssignments(rows)`,
		`card.hidden=false;`,
		`try{ccLoadAssignments();}catch(e)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Your assignments card missing %q", want)
		}
	}
	card := strings.Index(body, `id="cc-assign-card"`)
	mine := strings.Index(body, `id="cc-mine-card"`)
	wall := strings.Index(body, `id="cc-wall-card"`)
	if card < 0 || mine < 0 || wall < 0 || !(mine < card && card < wall) {
		t.Fatalf("Your assignments card should sit between Your contribution and the wall: mine=%d card=%d wall=%d", mine, card, wall)
	}
}

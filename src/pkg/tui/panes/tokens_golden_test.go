package panes_test

import (
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/panes"
)

// TestTokensGolden pins the tokens pane's complete 48x12 render, byte for
// byte, per the T9 acceptance criteria.
//
// Regenerate after a DELIBERATE layout change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and read the regenerated file in the diff — a golden updated without being
// read asserts nothing.
//
// Unlike the grid golden next door this drives the pane's View directly rather
// than a bubbletea program: a Pane is not a tea.Model (its View takes the box
// the grid computed for it), and the size the criteria name — 48x12 — is a
// pane interior, not a terminal. Rendering View directly also keeps the file
// free of escape sequences, so a reviewer can read the table in the diff.
//
// The fixture is the design sketch's own fleet (src/docs/design/tui.md §3)
// plus one agent whose cost was not fetched, which is the case that actually
// ships until a cost read is wired: GET /api/tokens carries no dollar figure
// at all, so the pane has to render a fleet of mixed cost availability without
// claiming the unpriced rows were free.
func TestTokensGolden(t *testing.T) {
	msg := panes.TokensMsg{
		Agents: []panes.TokenRow{
			{Agent: "scanner", TokenCounts: panes.TokenCounts{
				Input: 1_200_000, Output: 88_100, CostUSD: 4.10, CostKnown: true}},
			{Agent: "quality", TokenCounts: panes.TokenCounts{
				Input: 410_300, Output: 31_000, CostUSD: 1.32, CostKnown: true}},
			{Agent: "reviewer", TokenCounts: panes.TokenCounts{
				Input: 96_700, Output: 12_400, CostUSD: 0.38, CostKnown: true}},
			// No cost: the agent ran on a backend the price table does not
			// cover, or the cost read has not landed. Either way the column
			// must say "unknown", not "$0.00".
			{Agent: "ux-discovery", TokenCounts: panes.TokenCounts{
				Input: 48_200, Output: 5_100}},
		},
		// Larger than the sum of the rows on purpose: AggregateSummary's
		// totals count sessions the collector could not attribute to a
		// configured agent, and the pane renders the dashboard's total rather
		// than re-deriving one that would disagree with the web UI.
		Total: panes.TokenCounts{
			Input: 1_812_400, Output: 141_900, CostUSD: 5.80, CostKnown: true},
	}

	pane, _ := panes.NewTokens().Update(msg)
	requireGolden(t, []byte(pane.View(48, 12)), filepath.Join("testdata", "tokens.golden"))
}

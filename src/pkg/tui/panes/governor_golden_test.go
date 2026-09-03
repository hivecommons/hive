package panes_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// TestGovernorGolden pins the governor pane's complete 48x12 render, byte for
// byte, per the T7 acceptance criteria.
//
// Regenerate after a deliberate layout change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// The fixture uses both halves of the real queue shape (issues and PRs), a
// scheduled next evaluation, the separately fetched evaluation interval, and
// an explicitly configured ACMM level.
func TestGovernorGolden(t *testing.T) {
	msg := panes.GovernorMsg{
		Status: client.GovernorStatus{
			GovernorState: client.GovernorState{
				Active:   true,
				Mode:     "busy",
				Issues:   5,
				PRs:      2,
				NextKick: "8/29 3:09 PM PDT",
			},
			ACMMLevel:           4,
			ACMMLevelConfigured: true,
		},
		EvalInterval: 5 * time.Minute,
	}

	pane, _ := panes.NewGovernor().Update(msg)
	requireGolden(t, []byte(pane.View(48, 12)), filepath.Join("testdata", "governor.golden"))
}

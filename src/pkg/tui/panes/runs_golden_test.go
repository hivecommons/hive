package panes_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

func TestRunsGolden(t *testing.T) {
	observedAt := time.Date(2026, time.September, 22, 20, 0, 0, 0, time.UTC)
	msg := panes.RunsMsg{
		ObservedAt: observedAt,
		Runs: []client.Run{
			{Key: "hivecommons/hive#8309", Stage: "plan_review", WaitingOn: client.RunWaitingOnHuman, WaitingSince: observedAt.Add(-15 * time.Minute).Format(time.RFC3339), PlanEpicID: "epic-8309"},
			{Key: "hivecommons/hive#8299", Stage: "implement", WaitingOn: client.RunWaitingOnAgent, StageStartedAt: observedAt.Add(-2 * time.Hour).Format(time.RFC3339)},
			{Key: "hivecommons/hive#8310", Stage: "ci", WaitingOn: client.RunWaitingOnCI, StageStartedAt: observedAt.Add(-26 * time.Hour).Format(time.RFC3339)},
		},
	}

	pane, _ := panes.NewRuns().Update(msg)
	requireGolden(t, []byte(pane.View(64, 10)), filepath.Join("testdata", "runs.golden"))
}

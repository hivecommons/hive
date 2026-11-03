package classify

import (
	"slices"
	"strings"
	"testing"
)

const clarityClearBody = "## Goal\n" +
	"The `hive status` command should print the current queue depth so an operator can see the backlog without opening the dashboard.\n\n" +
	"## Acceptance criteria\n" +
	"- [ ] `hive status` prints a `queue: N` line, N being the actionable issue count\n" +
	"- [ ] the line is covered by a unit test next to `cmd/hive/status.go`\n" +
	"- [ ] nothing else in the output changes\n"

const clarityBugBody = "Run `hive init` in an empty directory.\n\n" +
	"Expected behavior: it writes hive.yaml.\n" +
	"Actual: it crashes with a nil pointer error."

func TestScoreClarity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		wantScore     int
		wantSignals   []string
		wantQuestions int
	}{
		{
			name:        "clear issue with goal, checklist and scope",
			body:        clarityClearBody,
			wantScore:   100,
			wantSignals: []string{ClaritySignalGoal, ClaritySignalCriteria, ClaritySignalScope, ClaritySignalDetailed},
		},
		{
			name:        "bug report with expected behaviour",
			body:        clarityBugBody,
			wantScore:   ClarityGoalPoints + ClarityCriteriaPoints + ClarityScopePoints + ClarityLengthPoints/2,
			wantSignals: []string{ClaritySignalGoal, ClaritySignalCriteria, ClaritySignalScope},
		},
		{
			name:          "vague one-liner",
			body:          "make it better",
			wantScore:     0,
			wantSignals:   []string{ClaritySignalShort},
			wantQuestions: 4,
		},
		{
			name:          "goal only",
			body:          "The dashboard should load faster.",
			wantScore:     ClarityGoalPoints,
			wantSignals:   []string{ClaritySignalGoal, ClaritySignalShort},
			wantQuestions: 3,
		},
		{
			name:          "empty body",
			body:          "   ",
			wantScore:     0,
			wantSignals:   []string{ClaritySignalShort},
			wantQuestions: 4,
		},
		{
			name:          "clear issue that leaves a choice open",
			body:          clarityClearBody + "\nStorage backend: TBD.\n",
			wantScore:     100 - ClarityContradictionPenalty,
			wantSignals:   []string{ClaritySignalGoal, ClaritySignalCriteria, ClaritySignalScope, ClaritySignalDetailed, ClaritySignalContradiction},
			wantQuestions: 1,
		},
		{
			name:          "contradiction never scores below zero",
			body:          "either this or maybe that",
			wantScore:     0,
			wantSignals:   []string{ClaritySignalShort, ClaritySignalContradiction},
			wantQuestions: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreClarity(tc.body, 80)
			if got.Score != tc.wantScore {
				t.Errorf("score = %d, want %d (signals %v)", got.Score, tc.wantScore, got.Signals)
			}
			if !slices.Equal(got.Signals, tc.wantSignals) {
				t.Errorf("signals = %v, want %v", got.Signals, tc.wantSignals)
			}
			if len(got.Questions) != tc.wantQuestions {
				t.Errorf("questions = %q, want %d", got.Questions, tc.wantQuestions)
			}
		})
	}
}

func TestScoreClarityIsDeterministic(t *testing.T) {
	a, b := ScoreClarity(clarityBugBody, 80), ScoreClarity(clarityBugBody, 80)
	if a.Score != b.Score || !slices.Equal(a.Signals, b.Signals) || !slices.Equal(a.Questions, b.Questions) {
		t.Fatalf("same body scored differently: %+v vs %+v", a, b)
	}
}

func TestScoreClarityAsksForAcceptanceCriteria(t *testing.T) {
	got := ScoreClarity("The dashboard should load faster.", 80)
	if !slices.ContainsFunc(got.Questions, func(q string) bool { return strings.Contains(q, "Acceptance criteria") }) {
		t.Fatalf("questions %q do not ask for acceptance criteria", got.Questions)
	}
}

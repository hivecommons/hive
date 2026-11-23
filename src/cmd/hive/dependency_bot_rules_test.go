package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// The dashboard's Overview card keeps read-only copies of the dependency-bot
// rules (pkg/dashboard/overview_card_rules.go). Both sides are checked
// against this one truth table, so a change here that the copies miss fails
// on one side or the other.
const dependencyBotRulesFixture = "../../pkg/dashboard/testdata/dependency_bot_rules.json"

type dependencyBotRuleCase struct {
	Name      string             `json:"name"`
	PR        github.PullRequest `json:"pr"`
	RedCI     bool               `json:"red_ci"`
	Conflict  bool               `json:"conflict"`
	Candidate bool               `json:"candidate"`
}

func TestDependencyBotRulesMatchTheSharedFixture(t *testing.T) {
	data, err := os.ReadFile(dependencyBotRulesFixture)
	if err != nil {
		t.Fatal(err)
	}
	var cases []dependencyBotRuleCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty fixture")
	}
	for _, tc := range cases {
		if got := dependencyBotRedCI(tc.PR); got != tc.RedCI {
			t.Errorf("%s: dependencyBotRedCI = %v, fixture says %v", tc.Name, got, tc.RedCI)
		}
		if got := conflictMergeState(tc.PR); got != tc.Conflict {
			t.Errorf("%s: conflictMergeState = %v, fixture says %v", tc.Name, got, tc.Conflict)
		}
		if got := staleRedBotRebaseCandidate(tc.PR); got != tc.Candidate {
			t.Errorf("%s: staleRedBotRebaseCandidate = %v, fixture says %v", tc.Name, got, tc.Candidate)
		}
	}
}

// The dashboard's card copies these values (overview_card_rules.go
// cardBotConflictGrace, cardBotRedCIGrace, cardDependabotAuthor), and its
// TestCardDependencyBotConstants pins them to the same literals.
func TestDependencyBotConstantsTheCardCopies(t *testing.T) {
	if depBotConflictGrace != 24*time.Hour || depBotRedCIGrace != 4*time.Hour || depBotDependabotAuthor != "dependabot[bot]" {
		t.Fatalf("conflict grace %v, red-CI grace %v, Dependabot %q: update the card's copies too", depBotConflictGrace, depBotRedCIGrace, depBotDependabotAuthor)
	}
}

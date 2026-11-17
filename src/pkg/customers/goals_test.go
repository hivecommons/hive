package customers

import (
	"errors"
	"strings"
	"testing"
)

func goalByText(p Person, text string) (Goal, bool) {
	for _, g := range p.Goals {
		if g.Text == text {
			return g, true
		}
	}
	return Goal{}, false
}

func TestInferenceNeverOverwritesAGoalAPersonOwns(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	if _, err := s.AddGoal(p.ID, "Ship the CSV importer", "ada"); err != nil {
		t.Fatal(err)
	}

	added, err := s.SuggestGoals(p.ID, []GoalSuggestion{
		{Text: "ship  the csv IMPORTER", Evidence: []string{IssueRef("o/r", 1)}}, // same goal, reworded
		{Text: "Get SSO working for their team", Evidence: []string{IssueRef("o/r", 2), MeetingRef("m1")}},
	})
	if err != nil || added != 1 {
		t.Fatalf("SuggestGoals = %d, %v; want 1 new suggestion", added, err)
	}
	got, _ := s.Get(p.ID)
	manual, ok := goalByText(got, "Ship the CSV importer")
	if !ok || manual.State != StateConfirmed || manual.Source != SourceManual || manual.By != "ada" {
		t.Fatalf("the manual goal was touched: %+v", got.Goals)
	}
	sso, ok := goalByText(got, "Get SSO working for their team")
	if !ok || sso.State != StateSuggested || sso.Source != SourceAuto || len(sso.Evidence) != 2 || sso.By != "" {
		t.Fatalf("suggestion = %+v", sso)
	}
	if len(got.ConfirmedGoals()) != 1 {
		t.Fatalf("an inferred goal counts as confirmed: %+v", got.ConfirmedGoals())
	}

	// Confirm: the hive worded it, a person stands behind it.
	got, err = s.ConfirmGoal(p.ID, sso.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	sso, _ = goalByText(got, "Get SSO working for their team")
	if sso.State != StateConfirmed || sso.Source != SourceAuto || sso.By != "owner" || len(sso.History) != 1 {
		t.Fatalf("confirmed = %+v", sso)
	}
	if again, _ := s.ConfirmGoal(p.ID, sso.ID, "someone-else"); len(again.Goals) != 2 {
		t.Fatal("confirming twice changed the goals")
	}

	// A later inference with different wording must not replace either.
	if _, err := s.SuggestGoals(p.ID, []GoalSuggestion{{Text: "Single sign-on", Evidence: []string{IssueRef("o/r", 3)}}}); err != nil {
		t.Fatal(err)
	}
	// ... and an inference with nothing to say withdraws only its own
	// pending suggestions.
	if _, err := s.SuggestGoals(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(p.ID)
	if len(got.Goals) != 2 || len(got.ConfirmedGoals()) != 2 {
		t.Fatalf("goals after an empty inference = %+v", got.Goals)
	}
}

func TestRejectedGoalIsNotOfferedAgain(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	if _, err := s.SuggestGoals(p.ID, []GoalSuggestion{{Text: "Wants dark mode", Evidence: []string{IssueRef("o/r", 9)}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(p.ID)
	if len(got.Goals) != 1 {
		t.Fatalf("goals = %+v", got.Goals)
	}
	got, err := s.RejectGoal(p.ID, got.Goals[0].ID)
	if err != nil || len(got.Goals) != 0 || len(got.RejectedGoals) != 1 {
		t.Fatalf("RejectGoal = %+v, %v", got, err)
	}
	added, err := s.SuggestGoals(p.ID, []GoalSuggestion{{Text: "wants DARK mode", Evidence: []string{IssueRef("o/r", 10)}}})
	if err != nil || added != 0 {
		t.Fatalf("a rejected goal was offered again: %d, %v", added, err)
	}
	if got, _ := s.Get(p.ID); len(got.Goals) != 0 {
		t.Fatalf("goals = %+v", got.Goals)
	}
	if _, err := s.RejectGoal(p.ID, "g_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reject missing = %v", err)
	}
}

func TestSuggestGoalsBoundsAndRefresh(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	var many []GoalSuggestion
	for _, text := range []string{"one", "two", "TWO", "", strings.Repeat("x", MaxGoalLen+1), "three", "four"} {
		many = append(many, GoalSuggestion{Text: text, Evidence: []string{"a:1", "a:2", "a:3", "a:4", "a:5", "a:6"}})
	}
	added, err := s.SuggestGoals(p.ID, many)
	if err != nil || added != maxPendingGoals {
		t.Fatalf("SuggestGoals = %d, %v; want the pending cap %d", added, err, maxPendingGoals)
	}
	got, _ := s.Get(p.ID)
	if len(got.Goals) != maxPendingGoals {
		t.Fatalf("goals = %+v", got.Goals)
	}
	one, _ := goalByText(got, "one")
	if len(one.Evidence) != MaxEvidence {
		t.Fatalf("evidence = %d, want the cap %d", len(one.Evidence), MaxEvidence)
	}
	// The same suggestion again keeps its id and refreshes its evidence.
	added, err = s.SuggestGoals(p.ID, []GoalSuggestion{{Text: "one", Evidence: []string{"b:1"}}})
	if err != nil || added != 0 {
		t.Fatalf("refresh = %d, %v", added, err)
	}
	got, _ = s.Get(p.ID)
	if len(got.Goals) != 1 || got.Goals[0].ID != one.ID || got.Goals[0].Evidence[0] != "b:1" {
		t.Fatalf("after refresh = %+v", got.Goals)
	}
	if _, err := s.SuggestGoals("p_missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing person = %v", err)
	}
}

func TestGoalEditAndErrors(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	if _, err := s.SuggestGoals(p.ID, []GoalSuggestion{{Text: "Faster builds", Evidence: []string{IssueRef("o/r", 4)}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(p.ID)
	id := got.Goals[0].ID
	got, err := s.EditGoal(p.ID, id, "Builds under five minutes", "ada")
	if err != nil {
		t.Fatal(err)
	}
	g := got.Goals[0]
	if g.Text != "Builds under five minutes" || g.State != StateConfirmed || g.Source != SourceManual || g.By != "ada" || len(g.History) != 1 {
		t.Fatalf("edited = %+v", g)
	}
	// Once edited it is a person's goal: inference leaves it alone.
	if _, err := s.SuggestGoals(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(p.ID); len(got.Goals) != 1 {
		t.Fatal("an edited goal was withdrawn by inference")
	}

	if _, err := s.EditGoal(p.ID, "g_missing", "x", "ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edit missing = %v", err)
	}
	if _, err := s.EditGoal(p.ID, id, " ", "ada"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("edit to empty = %v", err)
	}
	if _, err := s.ConfirmGoal(p.ID, "g_missing", "ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirm missing = %v", err)
	}
	if _, err := s.AddGoal(p.ID, "", "ada"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty goal = %v", err)
	}
	if _, err := s.AddGoal(p.ID, "builds under FIVE minutes", "ada"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate goal = %v", err)
	}
	if _, err := s.AddGoal("p_missing", "x", "ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("goal on missing person = %v", err)
	}
	for i := len(got.Goals); i < MaxGoals; i++ {
		if _, err := s.AddGoal(p.ID, "goal "+string(rune('a'+i)), "ada"); err != nil {
			t.Fatalf("goal %d: %v", i, err)
		}
	}
	if _, err := s.AddGoal(p.ID, "one too many", "ada"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over the goal cap = %v", err)
	}
	if added, _ := s.SuggestGoals(p.ID, []GoalSuggestion{{Text: "no room", Evidence: []string{"a:1"}}}); added != 0 {
		t.Fatal("a suggestion was added past the goal cap")
	}
}

func TestSatisfactionIsDecidedByAPerson(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	ev := []string{IssueRef("o/r", 1), PRRef("o/r", 2)}

	// Inference offers; the standing level does not move.
	offered, err := s.SuggestSatisfaction(p.ID, LevelAtRisk, ev)
	if err != nil || !offered {
		t.Fatalf("SuggestSatisfaction = %v, %v", offered, err)
	}
	got, _ := s.Get(p.ID)
	if got.Satisfaction.Level != LevelUnknown || got.Satisfaction.Suggestion == nil || got.Satisfaction.Suggestion.Level != LevelAtRisk {
		t.Fatalf("after suggestion = %+v", got.Satisfaction)
	}
	// The same suggestion again is not new.
	if offered, _ := s.SuggestSatisfaction(p.ID, LevelAtRisk, []string{PRRef("o/r", 2), IssueRef("o/r", 1)}); offered {
		t.Fatal("the same suggestion was offered twice")
	}

	// Reject: remembered, and not offered again from the same evidence.
	got, err = s.RejectSatisfaction(p.ID)
	if err != nil || got.Satisfaction.Suggestion != nil || got.Satisfaction.Level != LevelUnknown {
		t.Fatalf("RejectSatisfaction = %+v, %v", got.Satisfaction, err)
	}
	if offered, _ := s.SuggestSatisfaction(p.ID, LevelAtRisk, ev); offered {
		t.Fatal("a rejected suggestion was offered again")
	}
	// New evidence is a new suggestion.
	ev2 := append([]string{IssueRef("o/r", 7)}, ev...)
	if offered, _ := s.SuggestSatisfaction(p.ID, LevelAtRisk, ev2); !offered {
		t.Fatal("new evidence must be able to raise the question again")
	}

	// Confirm: the level stands, marked as inferred and confirmed by a person.
	got, err = s.ConfirmSatisfaction(p.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	sat := got.Satisfaction
	if sat.Level != LevelAtRisk || sat.Source != SourceAuto || sat.By != "owner" || len(sat.Evidence) != 3 || sat.Suggestion != nil {
		t.Fatalf("confirmed = %+v", sat)
	}

	// A person sets it by hand; inference must never overwrite that.
	got, err = s.SetSatisfaction(p.ID, LevelSatisfied, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if offered, err := s.SuggestSatisfaction(p.ID, LevelNeutral, []string{IssueRef("o/r", 20)}); err != nil || !offered {
		t.Fatalf("a disagreeing inference is stored beside the manual value: %v, %v", offered, err)
	}
	got, _ = s.Get(p.ID)
	sat = got.Satisfaction
	if sat.Level != LevelSatisfied || sat.Source != SourceManual || sat.By != "ada" {
		t.Fatalf("inference overwrote a manual level: %+v", sat)
	}
	if sat.Suggestion == nil || sat.Suggestion.Level != LevelNeutral {
		t.Fatalf("the disagreeing suggestion is missing: %+v", sat)
	}
	if len(sat.History) < 2 {
		t.Fatalf("history = %+v", sat.History)
	}
	// An inference that only agrees says nothing.
	if offered, _ := s.SuggestSatisfaction(p.ID, LevelSatisfied, []string{IssueRef("o/r", 21)}); offered {
		t.Fatal("an agreeing inference was offered")
	}
	// Setting by hand answers any waiting suggestion.
	got, _ = s.SetSatisfaction(p.ID, LevelNeutral, "ada")
	if got.Satisfaction.Suggestion != nil {
		t.Fatal("a manual set left a suggestion waiting")
	}
}

func TestSatisfactionErrors(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	if _, err := s.SetSatisfaction(p.ID, "delighted", "ada"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown level = %v", err)
	}
	if _, err := s.SuggestSatisfaction(p.ID, "9/10", []string{"a:1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a numeric score must be refused: %v", err)
	}
	if offered, err := s.SuggestSatisfaction(p.ID, LevelSatisfied, nil); err != nil || offered {
		t.Fatalf("a suggestion without evidence = %v, %v", offered, err)
	}
	if offered, err := s.SuggestSatisfaction(p.ID, LevelUnknown, []string{"a:1"}); err != nil || offered {
		t.Fatalf("suggesting unknown = %v, %v", offered, err)
	}
	if _, err := s.SuggestSatisfaction("p_missing", LevelNeutral, []string{"a:1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing person = %v", err)
	}
	if _, err := s.ConfirmSatisfaction(p.ID, "ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirm with nothing waiting = %v", err)
	}
	if _, err := s.RejectSatisfaction(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reject with nothing waiting = %v", err)
	}
	if _, err := s.SetSatisfaction("p_missing", LevelNeutral, "ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set on missing person = %v", err)
	}
}

func TestPruneEvidenceFollowsRetention(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	meeting, issue := MeetingRef("m1"), IssueRef("o/r", 1)
	if _, err := s.SuggestGoals(p.ID, []GoalSuggestion{
		{Text: "from the meeting only", Evidence: []string{meeting}},
		{Text: "from both", Evidence: []string{meeting, issue}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(p.ID)
	both, _ := goalByText(got, "from both")
	if _, err := s.ConfirmGoal(p.ID, both.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SuggestSatisfaction(p.ID, LevelNeutral, []string{meeting}); err != nil {
		t.Fatal(err)
	}

	// The meeting is purged by its own retention.
	dropped, err := s.PruneEvidence(func(ref string) bool { return ref != meeting })
	if err != nil || dropped != 3 {
		t.Fatalf("PruneEvidence = %d, %v; want 3 references dropped", dropped, err)
	}
	got, _ = s.Get(p.ID)
	if _, ok := goalByText(got, "from the meeting only"); ok {
		t.Fatal("a suggestion outlived all of its evidence")
	}
	both, ok := goalByText(got, "from both")
	if !ok || len(both.Evidence) != 1 || both.Evidence[0] != issue {
		t.Fatalf("confirmed goal after prune = %+v", both)
	}
	if got.Satisfaction.Suggestion != nil {
		t.Fatal("a satisfaction suggestion outlived its evidence")
	}
	raw := mustRead(t, s.Path())
	if strings.Contains(raw, meeting) {
		t.Fatal("people.json still points at the purged meeting")
	}
	if dropped, _ := s.PruneEvidence(func(string) bool { return true }); dropped != 0 {
		t.Fatalf("second prune dropped %d", dropped)
	}
}

func TestGoalFingerprint(t *testing.T) {
	if GoalFingerprint("Ship  the Importer") != GoalFingerprint(" ship the importer ") {
		t.Fatal("spacing and case must not change a goal's fingerprint")
	}
	if GoalFingerprint("a") == GoalFingerprint("b") {
		t.Fatal("different goals share a fingerprint")
	}
	if len(GoalFingerprint("a")) != fingerprintLen {
		t.Fatal("fingerprint length")
	}
}

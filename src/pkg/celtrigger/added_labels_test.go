package celtrigger

import "testing"

// event.added_labels (hivecommons/hive#18538) lets a rule fire on the label
// a pr.labeled event added, not on every label the PR already carries.
func TestMatch_AddedLabels(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "review-on-label", Expr: `event.kind == "pr.labeled" && "hive-review" in event.added_labels`, Agent: "reviewer"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	added := NormalizedEvent{Kind: KindPRLabeled, Labels: []string{"bug", "hive-review"}, AddedLabels: []string{"hive-review"}}
	if names := ruleNames(eng.Match(added)); len(names) != 1 {
		t.Fatalf("added opt-in label must match, got %v", names)
	}
	other := NormalizedEvent{Kind: KindPRLabeled, Labels: []string{"bug", "hive-review"}, AddedLabels: []string{"bug"}}
	if names := ruleNames(eng.Match(other)); len(names) != 0 {
		t.Fatalf("a different added label must not match, got %v", names)
	}
	opened := NormalizedEvent{Kind: KindPROpened, Labels: []string{"hive-review"}}
	if names := ruleNames(eng.Match(opened)); len(names) != 0 {
		t.Fatalf("pr.opened must not match a pr.labeled rule, got %v", names)
	}
}

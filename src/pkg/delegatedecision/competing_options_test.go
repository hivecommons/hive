package delegatedecision

import (
	"strings"
	"testing"
)

// projectbluefin/utah#920's three delivery options, as the dashboard reads
// them from the body.
var utah920Options = []Option{
	{Label: "1", Summary: "Carry a small set in the image: `gdb-headless`, `elfutils`, `sysprof-cli`."},
	{Label: "2", Summary: "A `ujust` recipe that installs the set on demand."},
	{Label: "3", Summary: "A `-dx`/debug flavor only, leaving the default image alone."},
}

func utah920Input() Input {
	return Input{Kind: KindIssue, Number: 920, Repo: "projectbluefin/utah",
		Title:  "Ship (or make one command away) gdb, sysprof-cli and elfutils for diagnosing shell hangs",
		Labels: []string{"kind/feature", LabelIssueNeedsDecision}, Options: utah920Options}
}

func TestCompetingOptionsWithoutARecommendationAreNotDecided(t *testing.T) {
	d := ApplyRails(Decide(utah920Input()), Rails{})
	if d.Verdict != VerdictRefused || d.Rule != RuleCompetingOptions || !d.KeepsPark {
		t.Fatalf("decision = %+v, want a refusal under %s", d, RuleCompetingOptions)
	}
	if len(d.Actions) != 0 {
		t.Fatalf("a refusal writes nothing, got actions %+v", d.Actions)
	}
	if want := "the issue asks a person to choose between 3 options (1, 2, 3) and recommends none"; !strings.HasPrefix(d.Rationale, want) {
		t.Errorf("rationale %q, want it to start %q", d.Rationale, want)
	}
	if d.Option != nil {
		t.Errorf("no option is chosen, got %+v", d.Option)
	}
}

func TestCompetingOptionsGoWithTheRecommendedOne(t *testing.T) {
	in := utah920Input()
	in.RecommendedOption = "2"
	d := ApplyRails(Decide(in), Rails{})
	if d.Verdict != VerdictApprove || d.Rule != RuleRecommendedOption {
		t.Fatalf("decision = %+v, want approve under %s", d, RuleRecommendedOption)
	}
	if d.Option == nil || d.Option.Label != "2" {
		t.Fatalf("option = %+v, want 2", d.Option)
	}
	want := "go with option 2: A `ujust` recipe that installs the set on demand."
	if d.Text != want {
		t.Errorf("text %q, want %q", d.Text, want)
	}
	// The recorded `/hive decision` carries the option, so the claiming
	// agent builds that one and not one it picks itself.
	var recorded string
	for _, a := range d.Actions {
		if a.Kind == ActionCommentDecision {
			recorded = a.Text
		}
	}
	if recorded != want {
		t.Errorf("recorded decision %q, want %q", recorded, want)
	}
	if body := CommentBody(d, "h1", "writer"); !strings.Contains(body, "option 2: A `ujust` recipe") {
		t.Errorf("decision comment does not name the option:\n%s", body)
	}
}

func TestCompetingOptionsOnlyApplyToARealChoice(t *testing.T) {
	in := utah920Input()
	in.Options = in.Options[:1]
	if d := Decide(in); d.Rule != "default-approve" {
		t.Errorf("one option is not a choice: rule %q, want default-approve", d.Rule)
	}
	in.Options = nil
	if d := Decide(in); d.Rule != "default-approve" {
		t.Errorf("no options: rule %q, want default-approve", d.Rule)
	}
	// A recommendation that names none of the options is no recommendation.
	in = utah920Input()
	in.RecommendedOption = "7"
	if d := Decide(in); d.Rule != RuleCompetingOptions {
		t.Errorf("unoffered recommendation: rule %q, want %s", d.Rule, RuleCompetingOptions)
	}
	// The LLM decider only ever replaces the reversible default; a choice
	// between options never reaches it.
	if IsReversibleDefault(Decide(utah920Input())) {
		t.Error("a competing-options refusal must not be handed to the LLM decider")
	}
}

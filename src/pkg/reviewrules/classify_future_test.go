package reviewrules

import (
	"strings"
	"testing"
)

// The comment a maintainer left on the techdocs analysis PR, verbatim
// (hivecommons/hive#19135).
const maintainersInline = "The project's `.project/maintainers.yaml` is the source of truth for project ownership and maintainership. Please capture this in the assessment knowledge base; it isn't yet documented in the assessment criteria.\n\n" +
	"That roster lists five maintainers. Docs `OWNERS` names only one maintainer, while `CODEOWNERS` also names someone who isn't on the roster. Reframe the recommendation around reconciling these files with the official roster, rather than simply adding a second approver.\n\n" +
	"https://github.com/hivecommons/.project/blob/5eba57f/maintainers.yaml#L15-L25"

// The same maintainer's later PR comment: it quotes the hive's reply and asks
// a question about the knowledge base. Neither part is the maintainer's rule.
const quotesAndAsks = "> On the knowledge-base request: I attempted it. The rule I would record: maintainers.yaml is the source of truth for ownership.\n\n" +
	"@operator, is there a way to, from these review conversations, have the hive agent add to the knowledge-base? Some of the assessment is more like an unwritten constitution than a codified one, so being able to add to the corpus may be helpful"

func TestClassify_FutureLookingPhrasings(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		kind    Kind
		signals []string
	}{
		{"in future", "We should consider this in future.", KindRule, []string{SignalGoingForward}},
		{"in the future", "In the future, check the roster first.", KindRule, []string{SignalGoingForward}},
		{"future assessments", "Future assessments should check the roster file.", KindRule, []string{SignalGoingForward}},
		{"in future assessments", "In future assessments, read the roster.", KindRule, []string{SignalGoingForward}},
		{"moving forward", "Moving forward, cite the roster.", KindRule, []string{SignalGoingForward}},
		{"for the future", "Worth keeping for the future.", KindRule, []string{SignalGoingForward}},
		{"add this to the knowledge base", "Please add this to the knowledge base.", KindRule, []string{SignalKnowledgeBase}},
		{"worth considering alone is not a default signal", "Worth considering a different heading.", KindCorrection, nil},
		{"a question is not a rule", "Should this always be the case?", KindCorrection, nil},
		{"quoted text is not the reviewer's rule", "> the lockfile is the source of truth\n\nThanks, fixed.", KindCorrection, nil},
		{"code is not the reviewer's rule", "```\nsource of truth\n```\nlooks fine", KindCorrection, nil},
		{"real inline comment", maintainersInline, KindRule, []string{SignalSourceOfTruth, SignalCaptureRequest, SignalCriteria, SignalKnowledgeBase}},
		{"quote plus question", quotesAndAsks, KindCorrection, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.body)
			if got.Kind != tc.kind || strings.Join(got.Signals, ",") != strings.Join(tc.signals, ",") {
				t.Fatalf("got %q %v, want %q %v", got.Kind, got.Signals, tc.kind, tc.signals)
			}
		})
	}
}

func TestExtractRuleText_QuotesQuestionsAndShortReferences(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"real inline comment", maintainersInline,
			"The project's `.project/maintainers.yaml` is the source of truth for project ownership and maintainership; it isn't yet documented in the assessment criteria"},
		{"a short sentence that refers back keeps the comment", "The roster check is missing here. We should consider this in future.",
			"The roster check is missing here. We should consider this in future."},
		{"quoted lines never enter the rule", "> the hive said X is authoritative\nGoing forward, the roster file is the source of truth for ownership.",
			"Going forward, the roster file is the source of truth for ownership."},
		{"questions never enter the rule", "Is the lockfile authoritative? The roster file must always be cited for ownership claims.",
			"The roster file must always be cited for ownership claims."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractRuleText(tc.body); got != tc.want {
				t.Fatalf("text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifier_ExtraPhrasesAndDisabledSignals(t *testing.T) {
	c := NewClassifier([]string{"worth considering", "  house   rule ", "x"}, map[string]bool{SignalAuthoritative: true})
	if got := c.Classify("Worth considering a different heading."); got.Kind != KindRule || strings.Join(got.Signals, ",") != SignalOperatorPhrase {
		t.Fatalf("extra phrase: %+v", got)
	}
	if got := c.Classify("A HOUSE\nRULE: cite commits"); got.Kind != KindCorrection {
		// a newline ends a sentence, so the phrase does not span it
		t.Fatalf("phrase split across sentences: %+v", got)
	}
	if got := c.Classify("This is a house rule: cite commits."); got.Kind != KindRule {
		t.Fatalf("normalised phrase: %+v", got)
	}
	if got := c.Classify("That registry is authoritative for names."); got.Kind != KindCorrection {
		t.Fatalf("disabled signal still matched: %+v", got)
	}
	if got := c.Classify("worth consideringly"); got.Kind != KindCorrection {
		t.Fatalf("phrase must match on word boundaries: %+v", got)
	}
	if got := c.ExtractRuleText("Nice. Worth considering for every assessment that cites a roster."); got != "Worth considering for every assessment that cites a roster." {
		t.Fatalf("extract with extra phrase: %q", got)
	}
	if got := NewClassifier([]string{"(a+)+$"}, nil).Classify("(a+)+$ is literal"); got.Kind != KindRule {
		t.Fatalf("a phrase is quoted, never compiled as a pattern: %+v", got)
	}
}

func TestValidatePhraseAndSignals(t *testing.T) {
	for p, want := range map[string]bool{"ok phrase": true, "ab": false, strings.Repeat("a", MaxPhraseRunes+1): false, "bad\x01": false} {
		if got := ValidatePhrase(p) == ""; got != want {
			t.Errorf("ValidatePhrase(%q) ok = %v, want %v", p, got, want)
		}
	}
	if len(Signals()) != len(ruleSignals) {
		t.Fatalf("Signals() lists %d, table has %d", len(Signals()), len(ruleSignals))
	}
	for _, s := range Signals() {
		if !KnownSignal(s.Name) || s.Examples == "" {
			t.Errorf("signal %+v", s)
		}
	}
	if KnownSignal("nope") || KnownSignal(SignalOperatorPhrase) {
		t.Fatal("unknown names are not signals")
	}
}

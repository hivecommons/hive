package reviewrules

import (
	"strings"
	"testing"
)

const maintainersComment = ".project/maintainers.yaml is the source of truth for ownership and maintainership; capture this in the assessment knowledge base"

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		kind    Kind
		signals []string
	}{
		{"maintainer fixture", maintainersComment, KindRule, []string{SignalSourceOfTruth, SignalCaptureRequest, SignalCriteria, SignalKnowledgeBase}},
		{"source of truth, hyphenated", "The CODEOWNERS file is the single source-of-truth here.", KindRule, []string{SignalSourceOfTruth}},
		{"authoritative", "That registry is authoritative for names.", KindRule, []string{SignalAuthoritative}},
		{"should always", "Analyses should always cite the scorecard.", KindRule, []string{SignalAlways}},
		{"must always", "You must always link the commit.", KindRule, []string{SignalAlways}},
		{"must never", "The score must never exceed the evidence.", KindRule, []string{SignalNever}},
		{"going forward", "Going forward, use the maintainers file.", KindRule, []string{SignalGoingForward}},
		{"from now on", "From now on check the license first.", KindRule, []string{SignalGoingForward}},
		{"next time", "Next time pin the link to a commit.", KindRule, []string{SignalGoingForward}},
		{"for future assessments", "Remember this for future assessments.", KindRule, []string{SignalGoingForward, SignalCaptureRequest}},
		{"document this", "Please document this somewhere.", KindRule, []string{SignalCaptureRequest}},
		{"assessment criteria", "This is missing from the assessment criteria.", KindRule, []string{SignalCriteria}},
		{"knowledge base", "Add that to the knowledgebase.", KindRule, []string{SignalKnowledgeBase}},
		{"one-off correction", "The score is too high for Agent readiness.", KindCorrection, nil},
		{"typo", "typo in line 4", KindCorrection, nil},
		{"always without a modal", "He is always late with reviews.", KindCorrection, nil},
		{"empty", "", KindCorrection, nil},
		{"upper case", "SOURCE OF TRUTH is the lockfile", KindRule, []string{SignalSourceOfTruth}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.body)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q (signals %v)", got.Kind, tc.kind, got.Signals)
			}
			if strings.Join(got.Signals, ",") != strings.Join(tc.signals, ",") {
				t.Fatalf("signals = %v, want %v", got.Signals, tc.signals)
			}
		})
	}
}

func TestExtractRuleText(t *testing.T) {
	long := strings.Repeat("always cite ", 100) + "this should always hold"
	tests := []struct {
		name string
		body string
		want string
	}{
		{"fixture drops the capture request clause", maintainersComment, ".project/maintainers.yaml is the source of truth for ownership and maintainership"},
		{"keeps only signal sentences", "The score is high. The maintainers file is the source of truth. Thanks!", "The maintainers file is the source of truth"},
		{"multi-line", "First line.\nGoing forward, cite the commit.\nBye", "Going forward, cite the commit"},
		{"only a capture request keeps it", "Please capture this.", "Please capture this."},
		{"no signal returns the body", "just a remark", "just a remark"},
		{"control characters and comment delimiters", "source of truth <!-- x --> is\tthe lockfile\x00", "source of truth  x  is the lockfile"},
		{"empty clause list falls back", "capture this; record it", "capture this; record it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractRuleText(tc.body)
			if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(tc.want), " ") {
				t.Fatalf("text = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		got := ExtractRuleText(long)
		if r := []rune(got); len(r) != MaxRuleRunes+len("...") || !strings.HasSuffix(got, "...") {
			t.Fatalf("len = %d, want %d with ellipsis", len(r), MaxRuleRunes+3)
		}
	})
}

func TestForeignHomeHint(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{"Please add this in your repo's docs", true},
		{"put it into the repository", true},
		{"This belongs in your criteria file", true},
		{"update your documentation", true},
		{"capture this in the assessment knowledge base", false},
		{"the project has maintainers", false},
	}
	for _, tc := range tests {
		if got := ForeignHomeHint(tc.body); got != tc.want {
			t.Errorf("ForeignHomeHint(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestDefuseMentions(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ask @nate-double-u now", "ask `@nate-double-u` now"},
		{"@alice first", "`@alice` first"},
		{"already `@bob` quoted", "already `@bob` quoted"},
		{"mail a@b.example", "mail a@b.example"},
		{"no mention", "no mention"},
	}
	for _, tc := range tests {
		if got := DefuseMentions(tc.in); got != tc.want {
			t.Errorf("DefuseMentions(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

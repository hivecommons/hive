package reviewrules

import (
	"strings"
	"testing"
)

const goodURL = "https://github.com/cncf/techdocs/pull/424#discussion_r1001"

func block(json string) string { return "Fixed.\n<!-- hive:review-rules " + json + " -->\n" }

func TestParseBlocks(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     int
		wantErrs []string
	}{
		{"no block", "just a reply", 0, nil},
		{"valid agent scope default", block(`{"rules":[{"text":"Use the maintainers file.","evidence_url":"` + goodURL + `"}]}`), 1, nil},
		{"valid workflow", block(`{"rules":[{"text":"t","scope":"workflow","applies_to":"cncf-techdocs-assessment","evidence_url":"` + goodURL + `"}]}`), 1, nil},
		{"valid repo", block(`{"rules":[{"text":"t","scope":"Repo","applies_to":"cncf/techdocs","evidence_url":"` + goodURL + `"}]}`), 1, nil},
		{"valid global", block(`{"rules":[{"text":"t","scope":"global","evidence_url":"` + goodURL + `"}]}`), 1, nil},
		{"malformed json", block(`{"rules":[`), 0, nil},
		{"unknown field", block(`{"rules":[{"text":"t","evidence_url":"` + goodURL + `","cmd":"rm -rf /"}]}`), 0, []string{"valid JSON"}},
		{"empty text", block(`{"rules":[{"text":"  ","evidence_url":"` + goodURL + `"}]}`), 0, []string{"text is empty"}},
		{"long text", block(`{"rules":[{"text":"` + strings.Repeat("x", MaxRuleRunes+1) + `","evidence_url":"` + goodURL + `"}]}`), 0, []string{"over 600"}},
		{"evidence not github", block(`{"rules":[{"text":"t","evidence_url":"https://evil.example/pull/1"}]}`), 0, []string{"evidence_url"}},
		{"evidence missing", block(`{"rules":[{"text":"t"}]}`), 0, []string{"evidence_url"}},
		{"evidence http", block(`{"rules":[{"text":"t","evidence_url":"http://github.com/a/b/pull/1"}]}`), 0, []string{"evidence_url"}},
		{"bad scope", block(`{"rules":[{"text":"t","scope":"everything","evidence_url":"` + goodURL + `"}]}`), 0, []string{"scope must be"}},
		{"workflow without name", block(`{"rules":[{"text":"t","scope":"workflow","evidence_url":"` + goodURL + `"}]}`), 0, []string{"needs applies_to"}},
		{"repo without slug", block(`{"rules":[{"text":"t","scope":"repo","applies_to":"nope","evidence_url":"` + goodURL + `"}]}`), 0, []string{"owner/name"}},
		{"applies_to bad characters", block(`{"rules":[{"text":"t","applies_to":"a b; rm","evidence_url":"` + goodURL + `"}]}`), 0, []string{"applies_to"}},
		{"one bad one good", block(`{"rules":[{"text":"","evidence_url":"` + goodURL + `"},{"text":"ok","evidence_url":"` + goodURL + `"}]}`), 1, []string{"rule 1"}},
		{"two blocks", block(`{"rules":[{"text":"a","evidence_url":"`+goodURL+`"}]}`) + block(`{"rules":[{"text":"b","evidence_url":"`+goodURL+`"}]}`), 2, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules, errs := ParseBlocks(tc.body)
			if len(rules) != tc.want {
				t.Fatalf("rules = %d, want %d (errs %v)", len(rules), tc.want, errs)
			}
			for _, w := range tc.wantErrs {
				found := false
				for _, e := range errs {
					found = found || strings.Contains(e.Error(), w)
				}
				if !found {
					t.Fatalf("errors %v do not mention %q", errs, w)
				}
			}
		})
	}
}

func TestParseBlocksCapsTheCount(t *testing.T) {
	var items []string
	for i := 0; i < MaxBlockRules+3; i++ {
		items = append(items, `{"text":"r","evidence_url":"`+goodURL+`"}`)
	}
	rules, errs := ParseBlocks(block(`{"rules":[` + strings.Join(items, ",") + `]}`))
	if len(rules) != MaxBlockRules || len(errs) != 1 || !strings.Contains(errs[0].Error(), "more than") {
		t.Fatalf("rules=%d errs=%v, want %d rules and one cap error", len(rules), errs, MaxBlockRules)
	}
}

func TestValidateBlockRuleNormalises(t *testing.T) {
	got, err := ValidateBlockRule(BlockRule{Text: " a\n b <!-- c --> ", Scope: " AGENT ", EvidenceURL: " " + goodURL + " ", AppliesTo: " quality "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "a b c" || got.Scope != "agent" || got.AppliesTo != "quality" || got.EvidenceURL != goodURL {
		t.Fatalf("normalised = %+v", got)
	}
}

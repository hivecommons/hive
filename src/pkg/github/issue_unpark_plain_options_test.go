package github

import (
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// The ci-maintainer park on Danathar/goodreads-mcp#432: options as plain "- A:"
// list items in a --needs-decision comment, so the comment carries the
// owner-step marker. It is a choice, not an owner step.
const plainListOptionsPark = "**What I need from you:** Pick A, B or C below.\n\n" +
	"## Options\n\n" +
	"- A: Keep the rule. Review the flagged PRs after the fact, then stop the Hive app merging Tier 2 paths (require a human merge).\n" +
	"- B: Change the rule in docs/risk-tiers.md so the Hive app may merge these paths, and update the audit to match.\n" +
	"- C: Accept the flagged PRs as reviewed and leave things as they are.\n\n" +
	"Reply `/hive decision A|B|C`."

func TestUnparkOptionLetterReadsPlainListLetters(t *testing.T) {
	cases := map[string]string{
		"- A: Keep the rule":       "A",
		"* B. Change the rule":     "B",
		"+ C) Accept":              "C",
		"## D — heading option":    "D",
		"- E:":                     "E",
		"  - F - spaced dash":      "F",
		"A: yes, it does":          "",
		"A. Smith wrote this":      "",
		"- E.g. a sentence":        "",
		"- A/B test the change":    "",
		"- Apple: fruit":           "",
		"- G: out of range":        "",
		"-A: no space after mark":  "",
		"- a: lower case is prose": "",
	}
	for line, want := range cases {
		if got := unparkOptionLetter(line); got != want {
			t.Errorf("unparkOptionLetter(%q) = %q, want %q", line, got, want)
		}
	}
	summaries := map[string]string{
		"- A: Keep the rule.":   "Keep the rule.",
		"* B. Change the rule":  "Change the rule",
		"## D — heading option": "heading option",
		"- E:":                  "",
	}
	for line, want := range summaries {
		if got := unparkOptionSummary(line); got != want {
			t.Errorf("unparkOptionSummary(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestUnparkNoticeReadsPlainListOptionsOnAMarkedPark(t *testing.T) {
	c := menuTestClient()
	park := ghComment("hive[bot]", ownerStepCommentBody(plainListOptionsPark, true))
	m := c.unparkMenuFor("Scheduled run failing: Agent audit trail", menuTestHiveUser, []*gh.IssueComment{park})
	if m.ownerStep {
		t.Fatal("a marked park that lists options is a choice, not an owner step")
	}
	if strings.Join(m.options, "") != "ABC" {
		t.Fatalf("options = %v, want A B C", m.options)
	}
	notice := renderUnparkNotice(m)
	for _, want := range []string{"`/hive approve`: go with option A: Keep the rule.", "`/hive decision B`: go with option B: Change the rule", "`/hive decision C`"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "/hive done") {
		t.Fatalf("a choice must not offer /hive done:\n%s", notice)
	}
}

// The shape the 045 policy rule asks for: `Option X:` lines and a
// `Recommended` line, including the owner-confirms form for governance
// questions. `/hive approve` accepts the named option.
func TestUnparkNoticeReadsThePolicyRecommendationLine(t *testing.T) {
	c := menuTestClient()
	for _, line := range []string{
		"Recommended: B, because it keeps the audit honest.",
		"Recommended (owner confirms): B, because A slows every merge.",
	} {
		body := ownerStepCommentBody("Option A: keep the rule\nOption B: change the rule\nOption C: accept\n\n"+line, true)
		m := c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", body)})
		bullets := m.bullets()
		if m.ownerStep || m.recommended != "B" || !strings.Contains(bullets, "`/hive approve`: go with the recommendation (option B): change the rule") {
			t.Fatalf("%q: recommended = %q, ownerStep = %v:\n%s", line, m.recommended, m.ownerStep, bullets)
		}
		if decision, ok := m.approveDecision(); !ok || !strings.Contains(decision, "option B") {
			t.Fatalf("%q: approve decision = %q, %v", line, decision, ok)
		}
	}
}

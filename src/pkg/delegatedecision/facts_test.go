package delegatedecision

import (
	"strings"
	"testing"
)

func factMap(fs []Fact) map[string]Fact {
	m := map[string]Fact{}
	for _, f := range fs {
		m[f.Label] = f
	}
	return m
}

func TestFactsGreenContributorPR(t *testing.T) {
	in := Input{Kind: KindPR, Number: 8, Author: "k", TrustedAuthor: true, CIGreen: true, ContributorOnline: true, AuthorTooling: AuthorToolingClanker, Small: true, HeadReviewVerdict: "approve"}
	m := factMap(in.Facts())
	want := map[string]Fact{
		"Author":      {"Author", "@k, trusted (write access)", FactInfo},
		"Contributor": {"Contributor", "the author's clanker is connected", FactInfo},
		"CI":          {"CI", "green", FactOK},
		"Conflicts":   {"Conflicts", "none", FactOK},
		"Sign-off":    {"Sign-off", "signed off", FactOK},
		"Review":      {"Review", "the hive's review on this head: approve", FactOK},
		"Sentinel":    {"Sentinel", "no alert", FactOK},
		"Change":      {"Change", "small (size XS or S)", FactInfo},
	}
	for label, f := range want {
		if m[label] != f {
			t.Errorf("%s = %+v, want %+v", label, m[label], f)
		}
	}
	for _, f := range in.Facts() {
		if f.State == FactBlocks {
			t.Errorf("a green, signed-off, conflict-free head lists a blocker: %+v", f)
		}
	}
}

func TestFactsBlockedPRNamesEveryBlocker(t *testing.T) {
	in := Input{Kind: KindPR, Number: 15219, Author: "Karibhou", AuthorTooling: AuthorToolingClanker,
		Conflicting: true, DCOMissing: true, SentinelAlert: true, SentinelReason: "changes 1 sensitive path(s)",
		ChangesRequestedBy: []string{"maint"}, WorkflowPath: true, Question: "Should this land before the release?",
		Competing: []CompetingPR{{Number: 15219, Current: true}, {Number: 15240}}}
	m := factMap(in.Facts())
	blocks := map[string]string{
		"CI":             "not green",
		"Conflicts":      "conflicts with the base branch",
		"Sign-off":       "DCO sign-off missing",
		"Review":         "the changes @maint requested on GitHub still stand",
		"Sentinel":       "changes 1 sensitive path(s)",
		"Workflow files": "edits .github/workflows; the hive cannot push them here",
	}
	for label, value := range blocks {
		if f := m[label]; f.Value != value || f.State != FactBlocks {
			t.Errorf("%s = %+v, want %q holding it back", label, f, value)
		}
	}
	if f := m["Contributor"]; f.Value != "the author's clanker hasn't been connected since the nudge" {
		t.Errorf("Contributor = %+v", f)
	}
	if f := m["Author"]; f.Value != "@Karibhou, not trusted" {
		t.Errorf("Author = %+v", f)
	}
	if f := m["Other PRs for the same work"]; f.Value != "#15240" {
		t.Errorf("competing = %+v, want only the other PR", f)
	}
	if f := m["Question for a person"]; f.Value != in.Question {
		t.Errorf("question = %+v", f)
	}
}

func TestFactsPendingCIIsNotABlocker(t *testing.T) {
	m := factMap(Input{Kind: KindPR, CIPending: true}.Facts())
	if f := m["CI"]; f.State != FactInfo || !strings.Contains(f.Value, "still running") {
		t.Fatalf("CI = %+v, want still running, not a blocker", f)
	}
}

func TestFactsIssueListsNoHeadFacts(t *testing.T) {
	in := Input{Kind: KindIssue, Number: 42, Author: "reporter", DuplicateOf: 7, Overlaps: []int{9, 11}, RepoPaused: true}
	m := factMap(in.Facts())
	for _, label := range []string{"CI", "Conflicts", "Sign-off", "Review", "Contributor", "Change"} {
		if _, ok := m[label]; ok {
			t.Errorf("an issue lists the PR fact %q", label)
		}
	}
	if m["Duplicate"].Value != "duplicates #7" || m["Overlaps"].Value != "#9, #11" || m["Repository"].State != FactInfo {
		t.Fatalf("issue facts = %+v", in.Facts())
	}
}

func TestFactsHiveAndPersonPRsHaveNoContributorLine(t *testing.T) {
	for _, in := range []Input{{Kind: KindPR, HiveAuthored: true}, {Kind: KindPR, HumanAuthor: true, Author: "dev"}} {
		if _, ok := factMap(in.Facts())["Contributor"]; ok {
			t.Errorf("%+v lists a contributor reachability line", in)
		}
	}
}

func TestLadderWhyNotNamesTheOptions(t *testing.T) {
	in := Input{Kind: KindPR, Number: 15219, Author: "k", AuthorTooling: AuthorToolingClanker, Conflicting: true}
	d, ok := decideLadder(in, "#15219")
	if !ok || d.Ladder == nil {
		t.Fatalf("ladder did not decide: %+v", d)
	}
	got := d.Ladder.WhyNot()
	if len(got) != len(d.Ladder.RuledOut) {
		t.Fatalf("WhyNot = %+v for %v", got, d.Ladder.RuledOut)
	}
	byOption := map[string]string{}
	for _, w := range got {
		byOption[w.Option] = w.Because
	}
	if byOption["Close as already fixed"] != "no merged PR has closed this PR's issue" {
		t.Errorf("already-fixed = %q", byOption["Close as already fixed"])
	}
	if b := byOption["Approve as it is"]; !strings.Contains(b, "conflicting") || !strings.Contains(b, "CI not green") {
		t.Errorf("mergeable-as-is = %q, want the blockers", b)
	}
	if (*Ladder)(nil).WhyNot() != nil {
		t.Error("a nil ladder has no why-not")
	}
	unknown := (&Ladder{RuledOut: []string{"some-new-rung: because"}}).WhyNot()
	if unknown[0].Option != "some new rung" || unknown[0].Because != "because" {
		t.Errorf("unknown rung = %+v", unknown)
	}
}

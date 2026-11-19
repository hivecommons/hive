package delegatedecision

import (
	"slices"
	"strings"
	"testing"
)

func addedLabels(d Decision) []string {
	var out []string
	for _, a := range d.Actions {
		if a.Kind == ActionLabelsAdd {
			out = append(out, a.Labels...)
		}
	}
	return out
}

// hivecommons/hive#19229: a governor follow-up asking for a person's
// security read was default-approved ("an agent may claim it") and landed at
// 010 where no agent could act. A human-only ask keeps its park.
func TestHumanOnlyIssueKeepsPark(t *testing.T) {
	in := Input{Kind: KindIssue, Number: 19229,
		Title:     "[governor] follow-up: A reviewer handed this to a person: security requires human: Not deeply reviewed",
		HumanOnly: HumanOnlySecurityReview}
	d := Run(in)
	if d.Rule != RuleHumanOnly || !d.KeepsPark || d.Verdict != VerdictRecommendOnly {
		t.Fatalf("decision = %+v, want %s keeping the park", d, RuleHumanOnly)
	}
	if !slices.Contains(addedLabels(d), LabelNeedsOwner) {
		t.Fatalf("labels added = %v, want needs-owner", addedLabels(d))
	}
	for _, a := range d.Actions {
		if a.Kind == ActionCommentDecision || slices.Contains(a.Labels, LabelTriageAccepted) {
			t.Fatalf("a human-only ask must not be approved for an agent: %+v", d.Actions)
		}
	}
	if !strings.Contains(d.Text, HumanOnlySecurityReview) {
		t.Fatalf("text %q must say why", d.Text)
	}
	if e, ok := Explain(d.Rule); !ok || e.Kind != GlossaryRule {
		t.Fatalf("rule %q has no glossary entry", d.Rule)
	}
}

func TestHumanOnlyIssueControlDefaultApproves(t *testing.T) {
	d := Run(Input{Kind: KindIssue, Number: 19230, Title: "follow-up: also cover the docs page"})
	if d.Rule != "default-approve" {
		t.Fatalf("control: an ordinary follow-up is %q, want default-approve", d.Rule)
	}
}

func TestHumanOnlyPRSecurityRoutedKeepsPark(t *testing.T) {
	in := Input{Kind: KindPR, Number: 19227, Title: "🐛 fix: proxy egress policy", CIGreen: true, HiveAuthored: true, TrustedAuthor: true,
		ReviewerRequiresHuman: true, SecurityRequiresPerson: true, Small: true}
	d := Run(in)
	if d.Rule != RuleHumanOnly || !d.KeepsPark {
		t.Fatalf("decision = %+v, want %s", d, RuleHumanOnly)
	}
	for _, a := range d.Actions {
		if a.Kind == ActionApproveReview {
			t.Fatalf("a security hand-off must not be released: %+v", d.Actions)
		}
	}
	in.SecurityRequiresPerson = false
	if d := Run(in); d.Rule == RuleHumanOnly {
		t.Fatalf("control: without the security hand-off the PR is not human-only: %+v", d)
	}
}

func TestHumanOnlyPRAdoptedKeepsItsOwnRule(t *testing.T) {
	in := Input{Kind: KindPR, Number: 15722, Title: "fix: thing", CIGreen: true, AdoptedByHive: true,
		ReviewerRequiresHuman: true, SecurityFinding: true, SecurityRequiresPerson: true}
	if d := Run(in); d.Rule != RuleAdoptedSecurityFinding {
		t.Fatalf("adopted PR rule = %q, want %q", d.Rule, RuleAdoptedSecurityFinding)
	}
}

// hivecommons/hive#19226: rule owner-only fired on a code-review PR because
// its title named credentials ("set or rotate the secret named in the
// title"). A PR title is its topic; only a grant-shaped title or a question
// that names a secret is an owner ask.
func TestOwnerOnlyPRTitleTopicIsNotAnAsk(t *testing.T) {
	for _, title := range []string{
		"🐛 fix: consistent redirect policy and credential stripping for http clients",
		"fix(proxy): redact tokens in egress logs",
		"Credential helper refactor",
	} {
		in := Input{Kind: KindPR, Number: 19226, Title: title, CIGreen: true}
		if OwnerOnly(in) {
			t.Errorf("PR %q is a code change, not an owner-only ask", title)
		}
		if d := Run(in); d.Rule == "owner-only" {
			t.Errorf("PR %q decided owner-only: %+v", title, d)
		}
	}
	for _, in := range []Input{
		{Kind: KindPR, Number: 50, Title: "Add org membership for bot"},
		{Kind: KindPR, Number: 50, Title: "✨ feat: rotate the release PAT"},
		{Kind: KindPR, Number: 50, Title: "fix: release job", Question: "This needs a new repository secret REGISTRY_TOKEN; can the owner add it?"},
		{Kind: KindPR, Number: 50, Title: "fix: release job", Reasons: []string{"needs a PAT with packages:write"}},
	} {
		if !OwnerOnly(in) {
			t.Errorf("%+v asks the owner for a grant and must stay owner-only", in)
		}
	}
}

func TestOwnerOnlyIssueTitleStillMatches(t *testing.T) {
	if !OwnerOnly(Input{Kind: KindIssue, Title: "Workflow fails: GITHUB_TOKEN lacks packages:write"}) {
		t.Fatal("issue titles keep the topic match: an issue is the ask")
	}
}

package delegatedecision

import (
	"strings"
	"testing"
)

func kinds(d Decision) []ActionKind {
	out := make([]ActionKind, 0, len(d.Actions))
	for _, a := range d.Actions {
		out = append(out, a.Kind)
	}
	return out
}

func sameKinds(a, b []ActionKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCapabilityGapSharedInference(t *testing.T) {
	for _, report := range []string{
		"The App cannot push changes to workflow files.",
		"The App cannot push workflows.",
		"The App cannot edit the workflow.",
		"The App cannot push to the upstream repository.",
		"The App cannot update the protected branch.",
		"The App cannot set the repository secret.",
		"The GitHub App lacks the `issues: write` permission.",
		"App lacks the `administration` permission to edit rulesets",
		"The installation lacks the required permissions; needs a human.",
		"The App token is missing the `checks: write` permission.",
		"The installation has no permission to manage checks.",
		"The App is without the required permission.",
		"The App cannot obtain the required permission.",
		"The App can't obtain the required permission.",
	} {
		t.Run(report, func(t *testing.T) {
			for _, in := range []Input{
				{Kind: KindIssue, Title: report},
				{Kind: KindIssue, Question: report},
				{Kind: KindIssue, Reasons: []string{report}},
				{Kind: KindIssue, Body: report},
				{Kind: KindIssue, Body: strings.Repeat("context\n", 11) + report},
			} {
				d := Run(in)
				if !CapabilityGap(in) || d.Rule != RuleCapabilityGap || d.Verdict != VerdictRecommendOnly || !d.KeepsPark || !sameKinds(kinds(d), []ActionKind{ActionRecommendOnly}) {
					t.Fatalf("capability report must keep the park: input=%+v decision=%+v", in, d)
				}
			}
		})
	}
	for _, in := range []Input{
		{Kind: KindIssue, Title: "The contributor must sign the CLA."},
		{Kind: KindIssue, Title: "Which workflow retry strategy should we use?"},
		{Kind: KindIssue, Title: "Which permissions should the GitHub App request?"},
		{Kind: KindIssue, Title: "The GitHub App has the required permissions."},
		{Kind: KindIssue, Body: strings.Repeat("context\n", 12) + "The GitHub App lacks the `issues: write` permission."},
		{Kind: KindPR, Body: "The GitHub App lacks the `issues: write` permission."},
		{Kind: KindIssue, Body: strings.Repeat("context\n", 12) + "The App cannot push to the repository."},
		{Kind: KindPR, Body: "The App cannot push to the repository."},
	} {
		if CapabilityGap(in) {
			t.Errorf("unexpected capability gap: %+v", in)
		}
	}
}

func TestDecideIssueRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        Input
		verdict   Verdict
		rule      string
		kinds     []ActionKind
		keepsPark bool
		target    int
	}{
		{
			name:    "hive policy change recommends only",
			in:      Input{Kind: KindIssue, Number: 10, Title: "Change hive.yaml branch protection defaults"},
			verdict: VerdictRecommendOnly, rule: "hive-policy", kinds: []ActionKind{ActionRecommendOnly}, keepsPark: true,
		},
		{
			name:    "governance label recommends only",
			in:      Input{Kind: KindIssue, Number: 10, Title: "Reorganize contributor ladder", Labels: []string{"Governance"}},
			verdict: VerdictRecommendOnly, rule: "hive-policy", keepsPark: true,
		},
		{
			name:    "capability gap keeps the park instead of looping",
			in:      Input{Kind: KindIssue, Number: 14976, Title: "Retry wrappers", Reasons: []string{"workflow files which the App token cannot push (no `workflows` permission); needs a human"}},
			verdict: VerdictRecommendOnly, rule: "capability-gap", kinds: []ActionKind{ActionRecommendOnly}, keepsPark: true,
		},
		{
			name:    "secrets item is owner-only",
			in:      Input{Kind: KindIssue, Number: 11, Title: "Workflow fails: GITHUB_TOKEN lacks packages:write"},
			verdict: VerdictOwnerOnly, rule: "owner-only", kinds: []ActionKind{ActionLabelsAdd, ActionRecommendOnly}, keepsPark: true,
		},
		{
			name:    "secrets in park reason is owner-only",
			in:      Input{Kind: KindIssue, Number: 11, Title: "Release job red", Reasons: []string{"needs a PAT with packages:write"}},
			verdict: VerdictOwnerOnly, rule: "owner-only", keepsPark: true,
		},
		{
			name:    "secrets in body is owner-only",
			in:      Input{Kind: KindIssue, Number: 11, Title: "Release job red", Body: "The release job needs a new secret for the registry."},
			verdict: VerdictOwnerOnly, rule: "owner-only", keepsPark: true,
		},
		{
			name:    "explicit duplicate closes newer",
			in:      Input{Kind: KindIssue, Number: 20, Title: "Dashboard flickers", DuplicateOf: 12},
			verdict: VerdictCloseDuplicate, rule: "dedupe", kinds: []ActionKind{ActionCloseDuplicate}, target: 12,
		},
		{
			name:    "duplicate from reason text",
			in:      Input{Kind: KindIssue, Number: 20, Title: "Dashboard flickers", Reasons: []string{"reviewer: duplicate of #15?"}},
			verdict: VerdictCloseDuplicate, rule: "dedupe", target: 15,
		},
		{
			name:    "duplicate pointing at a newer item does not close",
			in:      Input{Kind: KindIssue, Number: 20, Title: "Dashboard flickers", DuplicateOf: 25},
			verdict: VerdictApprove, rule: "default-approve",
		},
		{
			name:    "review backlog never auto-closes",
			in:      Input{Kind: KindIssue, Number: 21, Title: "Review backlog: PR closed unmerged", BlockedOnPRClosed: true},
			verdict: VerdictApprove, rule: "review-backlog", kinds: []ActionKind{ActionCommentDecision, ActionLabelsAdd},
		},
		{
			name:    "review backlog from text",
			in:      Input{Kind: KindIssue, Number: 21, Title: "Add tests", Question: "the review backlog item is blocked on its PR which closed unmerged; close it?"},
			verdict: VerdictApprove, rule: "review-backlog",
		},
		{
			name:    "flaky test quarantines",
			in:      Input{Kind: KindIssue, Number: 22, Title: "TestWebhook is flaky on macOS"},
			verdict: VerdictQuarantineFlaky, rule: "flaky-test", kinds: []ActionKind{ActionLabelsAdd, ActionCommentDecision, ActionLabelsAdd},
		},
		{
			name:    "flaky label quarantines",
			in:      Input{Kind: KindIssue, Number: 22, Title: "TestWebhook red", Labels: []string{"flaky"}},
			verdict: VerdictQuarantineFlaky, rule: "flaky-test",
		},
		{
			name:    "ci hygiene gate approves as ratchet",
			in:      Input{Kind: KindIssue, Number: 23, Title: "Add actionlint to CI"},
			verdict: VerdictAcceptRatchet, rule: "ci-ratchet",
		},
		{
			name:    "ratchet from body",
			in:      Input{Kind: KindIssue, Number: 23, Title: "CI tightening", Body: "Introduce a link-checker allowlist and fail on new breaks."},
			verdict: VerdictAcceptRatchet, rule: "ci-ratchet",
		},
		{
			name:    "design proposal with acceptance criteria accepted",
			in:      Input{Kind: KindIssue, Number: 24, Title: "Proposal: tracker filters", Labels: []string{"kind/proposal"}, Body: "## Acceptance criteria\n- filters persist"},
			verdict: VerdictAcceptProposal, rule: "design-proposal",
		},
		{
			name:    "proposal without acceptance criteria falls to default",
			in:      Input{Kind: KindIssue, Number: 24, Title: "Proposal: tracker filters", Labels: []string{"proposal"}, Body: "some thoughts"},
			verdict: VerdictApprove, rule: "default-approve",
		},
		{
			name:    "default approve cross-links overlaps sorted",
			in:      Input{Kind: KindIssue, Number: 30, Title: "Something", Overlaps: []int{9, 3}},
			verdict: VerdictApprove, rule: "default-approve", kinds: []ActionKind{ActionCommentDecision, ActionLabelsAdd, ActionLink, ActionLink}, target: 3,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Decide(tc.in)
			if d.Verdict != tc.verdict || d.Rule != tc.rule {
				t.Fatalf("verdict/rule = %s/%s, want %s/%s", d.Verdict, d.Rule, tc.verdict, tc.rule)
			}
			if d.KeepsPark != tc.keepsPark {
				t.Fatalf("KeepsPark = %v, want %v", d.KeepsPark, tc.keepsPark)
			}
			if tc.kinds != nil && !sameKinds(kinds(d), tc.kinds) {
				t.Fatalf("actions = %v, want %v", kinds(d), tc.kinds)
			}
			if tc.target != 0 {
				found := false
				for _, a := range d.Actions {
					if a.Target == tc.target {
						found = true
					}
				}
				if !found {
					t.Fatalf("no action targets #%d: %+v", tc.target, d.Actions)
				}
			}
			if d.Text == "" || d.Rationale == "" {
				t.Fatalf("empty text/rationale: %+v", d)
			}
		})
	}
}

func TestDecidePRRules(t *testing.T) {
	t.Parallel()
	green := Input{Kind: KindPR, Number: 50, Title: "Fix typo", CIGreen: true, HiveAuthored: true, TrustedAuthor: true, ContributorOnline: true}
	cases := []struct {
		name      string
		in        Input
		verdict   Verdict
		rule      string
		kinds     []ActionKind
		keepsPark bool
		textHas   string
	}{
		{
			name:    "hive policy PR recommends only",
			in:      Input{Kind: KindPR, Number: 50, Title: "Tighten required checks in hive.yaml", CIGreen: true},
			verdict: VerdictRecommendOnly, rule: "hive-policy", keepsPark: true,
		},
		{
			name:    "owner-only PR",
			in:      Input{Kind: KindPR, Number: 50, Title: "Add org membership for bot", CIGreen: true},
			verdict: VerdictOwnerOnly, rule: "owner-only", keepsPark: true, textHas: "organization membership",
		},
		{
			name:    "sentinel trusted approves with reviewed-intended",
			in:      Input{Kind: KindPR, Number: 51, Title: "Bump CI action pin", SentinelAlert: true, SentinelReason: "workflow file changed", TrustedAuthor: true, CIGreen: true},
			verdict: VerdictApprove, rule: "sentinel-trusted", kinds: []ActionKind{ActionCommentDecision, ActionApproveReview}, textHas: "reviewed, intended: workflow file changed",
		},
		{
			name:    "sentinel trusted without reason",
			in:      Input{Kind: KindPR, Number: 51, Title: "Bump", SentinelAlert: true, TrustedAuthor: true},
			verdict: VerdictApprove, rule: "sentinel-trusted", textHas: "sensitive path",
		},
		{
			name:    "sentinel untrusted recommends only",
			in:      Input{Kind: KindPR, Number: 51, Title: "Bump", SentinelAlert: true},
			verdict: VerdictRecommendOnly, rule: "sentinel-untrusted", keepsPark: true,
		},
		{
			name:    "hive PR stays parked pending contributor adequacy review",
			in:      Input{Kind: KindPR, Number: 52, Title: "Add filter", HiveAuthored: true, CIGreen: true, Competing: []CompetingPR{{Number: 49, Mergeable: true, Current: true}}},
			verdict: VerdictRecommendOnly, rule: "prefer-contributor", kinds: []ActionKind{ActionRecommendOnly}, keepsPark: true, textHas: "compare #49 with #52",
		},
		{
			name:    "green contributor PR beats hive duplicate",
			in:      Input{Kind: KindPR, Number: 52, Title: "Add filter", CIGreen: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 53, HiveAuthored: true}}},
			verdict: VerdictApprove, rule: "prefer-contributor", kinds: []ActionKind{ActionCommentDecision, ActionApproveReview},
		},
		{
			name:    "stale contributor PR superseded by hive with credit",
			in:      Input{Kind: KindPR, Number: 52, Title: "Add filter", Conflicting: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 53, HiveAuthored: true}}},
			verdict: VerdictCloseSuperseded, rule: "prefer-contributor", textHas: "with credit",
		},
		{
			name:    "competing hive duplicate does not release a PR missing DCO",
			in:      Input{Kind: KindPR, Number: 52, Title: "Add filter", CIGreen: true, DCOMissing: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 53, HiveAuthored: true}}},
			verdict: VerdictDirection, rule: "fix-then-merge", kinds: []ActionKind{ActionCommentDecision}, textHas: "add the DCO sign-off",
		},
		{
			name:    "competing hive duplicate does not bypass the reporter-trust gate",
			in:      Input{Kind: KindPR, Number: 52, Title: "Add filter", CIGreen: true, ReporterTrustHold: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 53, HiveAuthored: true}}},
			verdict: VerdictApprove, rule: "reporter-trust",
		},
		{
			name:    "two hive PRs competing fall through",
			in:      Input{Kind: KindPR, Number: 52, Title: "Add filter", HiveAuthored: true, CIGreen: true, Competing: []CompetingPR{{Number: 53, HiveAuthored: true}}},
			verdict: VerdictApprove, rule: "default-release",
		},
		{
			name:    "online contributor ignores stale adoption caption",
			in:      Input{Kind: KindPR, Number: 54, Title: "Contributor fix", AdoptionStalled: true, ContributorOnline: true, CIGreen: true},
			verdict: VerdictApprove, rule: "default-release", kinds: []ActionKind{ActionCommentDecision, ActionApproveReview},
		},
		{
			name:    "adoption stalled adopts in supersede mode",
			in:      Input{Kind: KindPR, Number: 54, Title: "Contributor fix", AdoptionStalled: true, CIGreen: true},
			verdict: VerdictAdoptSupersede, rule: "contributor-offline", kinds: []ActionKind{ActionCommentDecision, ActionAdoptSupersede},
		},
		{
			name:    "offline contributor with red CI adopts",
			in:      Input{Kind: KindPR, Number: 54, Title: "Contributor fix", DCOMissing: true, CIGreen: true},
			verdict: VerdictAdoptSupersede, rule: "contributor-offline",
		},
		{
			// hivecommons/hive#15188: the App cannot publish .github/workflows
			// edits, so adopt-supersede would be refused by the lane; the
			// policy must say so instead of promising an adoption.
			name:    "offline contributor on a workflow path is a capability gap",
			in:      Input{Kind: KindPR, Number: 54, Title: "CI fix", AdoptionStalled: true, WorkflowPath: true, CIGreen: true},
			verdict: VerdictRecommendOnly, rule: RuleCapabilityGap, kinds: []ActionKind{ActionRecommendOnly}, keepsPark: true,
		},
		{
			name:    "reporter trust hold cleared when PR sound",
			in:      Input{Kind: KindPR, Number: 55, Title: "Fix", ReporterTrustHold: true, CIGreen: true, ContributorOnline: true},
			verdict: VerdictApprove, rule: "reporter-trust", kinds: []ActionKind{ActionCommentDecision, ActionApproveReview, ActionLabelsAdd},
		},
		{
			name:    "conflicting head gets direction",
			in:      Input{Kind: KindPR, Number: 56, Title: "Fix", HiveAuthored: true, Conflicting: true, DCOMissing: true},
			verdict: VerdictDirection, rule: "fix-then-merge", kinds: []ActionKind{ActionCommentDecision}, textHas: "rebase onto the base branch, add the DCO sign-off, make CI green",
		},
		{
			name:    "small green PR approved",
			in:      func() Input { i := green; i.Labels = []string{"size/XS"}; return i }(),
			verdict: VerdictApprove, rule: "small-low-risk",
		},
		{
			name:    "low-risk paths approved",
			in:      func() Input { i := green; i.LowRiskPaths = true; return i }(),
			verdict: VerdictApprove, rule: "small-low-risk",
		},
		{
			name:    "green PR default release",
			in:      green,
			verdict: VerdictApprove, rule: "default-release", kinds: []ActionKind{ActionCommentDecision, ActionApproveReview},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Decide(tc.in)
			if d.Verdict != tc.verdict || d.Rule != tc.rule {
				t.Fatalf("verdict/rule = %s/%s, want %s/%s", d.Verdict, d.Rule, tc.verdict, tc.rule)
			}
			if d.KeepsPark != tc.keepsPark {
				t.Fatalf("KeepsPark = %v, want %v", d.KeepsPark, tc.keepsPark)
			}
			if tc.kinds != nil && !sameKinds(kinds(d), tc.kinds) {
				t.Fatalf("actions = %v, want %v", kinds(d), tc.kinds)
			}
			if tc.textHas != "" && !strings.Contains(d.Text, tc.textHas) {
				t.Fatalf("text %q lacks %q", d.Text, tc.textHas)
			}
		})
	}
}

// A clean third-party PR referencing the issue must not close the hive PR,
// even when the operator is allowed to run the delegated decision.
func TestCompetingPRCannotAutomaticallyCloseHivePR(t *testing.T) {
	t.Parallel()
	for _, trusted := range []bool{false, true} {
		d := Run(Input{
			Kind: KindPR, Number: 52, Title: "Add filter", HiveAuthored: true,
			CIGreen: true, TrustedAuthor: trusted,
			Competing: []CompetingPR{{Number: 49, Mergeable: true, Current: true}},
		})
		if d.Verdict != VerdictRecommendOnly || !d.KeepsPark {
			t.Fatalf("clean competitor must preserve the park: %+v", d)
		}
		if !sameKinds(kinds(d), []ActionKind{ActionRecommendOnly}) {
			t.Fatalf("clean competitor must not mutate either PR: %+v", d.Actions)
		}
		body := CommentBody(d, "test-hive", "operator")
		if strings.Contains(body, "/hive decision") || !strings.Contains(body, "keep #52 open") {
			t.Fatalf("recommendation must not release the parked PR: %s", body)
		}
	}
}

func TestOwnerAction(t *testing.T) {
	t.Parallel()
	cases := []struct{ title, want string }{
		{"Grant org membership to bot", "organization membership"},
		{"PAT scope missing repo", "rotate or re-scope"},
		{"Set REGISTRY secret", "set or rotate the secret"},
	}
	for _, tc := range cases {
		if got := ownerAction(Input{Title: tc.title}); !strings.Contains(got, tc.want) {
			t.Errorf("ownerAction(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

func TestApplyRails(t *testing.T) {
	t.Parallel()
	base := Decision{Verdict: VerdictApprove, Rule: "x", Text: "approve #1", Rationale: "r",
		Actions: []Action{{Kind: ActionCommentDecision, Text: "approve #1"}, {Kind: ActionApproveReview}}}

	t.Run("held item refused", func(t *testing.T) {
		t.Parallel()
		d := ApplyRails(base, Rails{Held: true})
		if d.Verdict != VerdictRefused || !d.KeepsPark || len(d.Actions) != 0 || len(d.Refusals) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("allowed actions pass", func(t *testing.T) {
		t.Parallel()
		d := ApplyRails(base, Rails{})
		if !sameKinds(kinds(d), kinds(base)) || len(d.Refusals) != 0 || d.Verdict != VerdictApprove {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("disallowed kind dropped", func(t *testing.T) {
		t.Parallel()
		in := base
		in.Actions = append([]Action{{Kind: ActionKind("merge")}}, in.Actions...)
		d := ApplyRails(in, Rails{})
		if len(d.Actions) != 2 || len(d.Refusals) != 1 || !strings.Contains(d.Refusals[0], "merge") {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("removing hold refused", func(t *testing.T) {
		t.Parallel()
		in := base
		in.Actions = []Action{{Kind: ActionLabelsRemove, Labels: []string{"stage/085-needs-decision", "hold"}}}
		d := ApplyRails(in, Rails{})
		if d.Verdict != VerdictRecommendOnly || !d.KeepsPark || len(d.Refusals) != 1 {
			t.Fatalf("got %+v", d)
		}
		if d.Actions[0].Kind != ActionRecommendOnly || !strings.Contains(d.Actions[0].Text, "hive recommends: approve #1") {
			t.Fatalf("got %+v", d.Actions)
		}
	})
	t.Run("narrowed allowed set downgrades to recommendation", func(t *testing.T) {
		t.Parallel()
		d := ApplyRails(base, Rails{AllowedActions: []ActionKind{ActionRecommendOnly}})
		if d.Verdict != VerdictRecommendOnly || len(d.Refusals) != 2 {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestRunReadsHoldLabel(t *testing.T) {
	t.Parallel()
	d := Run(Input{Kind: KindIssue, Number: 1, Title: "x", Labels: []string{"Hold"}})
	if d.Verdict != VerdictRefused {
		t.Fatalf("got %s", d.Verdict)
	}
	d = Run(Input{Kind: KindIssue, Number: 1, Title: "x"})
	if d.Verdict != VerdictApprove {
		t.Fatalf("got %s", d.Verdict)
	}
}

func TestMarkerAndComment(t *testing.T) {
	t.Parallel()
	m := Marker(" hive-a ")
	if m != "<!-- hive:delegated-decision by=hive-a policy=owner-delegate -->" {
		t.Fatalf("marker %q", m)
	}
	if Marker("") != "<!-- hive:delegated-decision by=unknown policy=owner-delegate -->" {
		t.Fatalf("empty marker %q", Marker(""))
	}
	if !IsDelegatedComment("x\n"+m) || IsDelegatedComment("plain") {
		t.Fatal("IsDelegatedComment")
	}

	approve := Decision{Verdict: VerdictApprove, Rule: "default-release", Text: "approve #5", Rationale: "r"}
	body := CommentBody(approve, "hive-a", "andy")
	for _, want := range []string{"/hive decision approve #5", "Rationale: r.", "@andy's request", "rule `default-release`", m} {
		if !strings.Contains(body, want) {
			t.Errorf("approve body lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "/hive decision ") {
		t.Errorf("decision line must be first: %q", body)
	}

	rec := Decision{Verdict: VerdictRecommendOnly, Rule: "hive-policy", Text: "hive recommends: x; owner to confirm", Rationale: "r", KeepsPark: true}
	body = CommentBody(rec, "hive-a", "")
	if strings.Contains(body, "/hive decision") || !strings.Contains(body, "`stage/085-needs-decision` stays") || !strings.Contains(body, "Decided by the hive as") {
		t.Errorf("recommend body:\n%s", body)
	}

	body = CommentBody(Decision{Verdict: VerdictRefused, Rule: "rail-hold", Rationale: "held"}, "h", "")
	if !strings.Contains(body, "did not decide this item: held.") {
		t.Errorf("refused body:\n%s", body)
	}
}

func TestPolicyTextCoversRules(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"Be decisive", "Duplicate of #N", "needs-owner", "ratchets", "supersede mode", "never merge a PR", "owner to confirm", "keep the hive PR open and parked until adequacy is confirmed", "except for the safety rails", "is not code work", "paid for by evidence"} {
		if !strings.Contains(PolicyText, want) {
			t.Errorf("PolicyText lacks %q", want)
		}
	}
	if len(AllowedActions) != 9 {
		t.Fatalf("allowed set size %d", len(AllowedActions))
	}
}

func TestIsSmall(t *testing.T) {
	t.Parallel()
	if !(Input{Small: true}).IsSmall() || !(Input{Labels: []string{"size/S"}}).IsSmall() || (Input{Labels: []string{"size/L"}}).IsSmall() {
		t.Fatal("IsSmall")
	}
}

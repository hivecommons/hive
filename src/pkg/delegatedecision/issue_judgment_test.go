package delegatedecision

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// TestDecideIssueLikeTheOwner pins the five real parked issues the owner
// judged by hand against what the delegate now decides, and logs the rule,
// verdict and comment each produces (the PR body quotes that log).
func TestDecideIssueLikeTheOwner(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      Input
		rule    string
		verdict Verdict
		text    []string
		notText []string
		actions []ActionKind
	}{
		{
			name: "hive#15021 selfreport agent_login_required → operator action, not code work",
			in:   fixtureHive15021, rule: "selfreport-operator-action", verdict: VerdictOperatorAction,
			text:    []string{"@clubanderson", "re-authenticate the agent provider", "hosted-ibm-alchemy-logg-ptoo"},
			actions: []ActionKind{ActionLabelsAdd, ActionRecommendOnly},
		},
		{
			name: "hive#15329 RFC on hive-filed proposals (human author) → recommend only, with a real recommendation",
			in:   fixtureHive15329, rule: "hive-policy", verdict: VerdictRecommendOnly,
			text:    []string{"hive recommends: approve `", "should not be actionable", "default-on", "owner to confirm"},
			notText: []string{"no special rule"},
			actions: []ActionKind{ActionRecommendOnly},
		},
		{
			name: "hivecommons.github.io#296 gate proposal without incident → not now",
			in:   fixtureSite296, rule: "proposal-not-now", verdict: VerdictNotNow,
			text:    []string{"not now", "no incident or recurrence justifies the cost", "re-propose"},
			actions: []ActionKind{ActionCloseNotNow},
		},
		{
			name: "hive#15335 gate config with incident evidence → approve",
			in:   fixtureHive15335, rule: "proposal-approve-evidence", verdict: VerdictApprove,
			text:    []string{"approve as proposed", "incident #15322"},
			actions: []ActionKind{ActionCommentDecision, ActionLabelsAdd},
		},
		{
			name: "rationguard#418 gate proposal citing a three-of-a-class recurrence → approve",
			in:   fixtureRationguard418, rule: "proposal-approve-evidence", verdict: VerdictApprove,
			text:    []string{"approve as proposed", "#415, #417"},
			actions: []ActionKind{ActionCommentDecision, ActionLabelsAdd},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Number = 1
			in.Labels = append([]string{LabelIssueNeedsDecision}, in.Labels...)
			d := Run(in)
			if d.Rule != tc.rule || d.Verdict != tc.verdict {
				t.Fatalf("rule=%q verdict=%q; want %q %q (%+v)", d.Rule, d.Verdict, tc.rule, tc.verdict, d)
			}
			for _, want := range tc.text {
				if !strings.Contains(d.Text, want) {
					t.Errorf("text %q lacks %q", d.Text, want)
				}
			}
			for _, bad := range tc.notText {
				if strings.Contains(d.Text, bad) {
					t.Errorf("text %q must not say %q", d.Text, bad)
				}
			}
			kinds := make([]ActionKind, 0, len(d.Actions))
			for _, a := range d.Actions {
				kinds = append(kinds, a.Kind)
			}
			if len(kinds) != len(tc.actions) {
				t.Fatalf("actions = %v; want %v", kinds, tc.actions)
			}
			for i := range kinds {
				if kinds[i] != tc.actions[i] {
					t.Fatalf("actions = %v; want %v", kinds, tc.actions)
				}
			}
			body := CommentBodyWith(d, "hive-a", "clubanderson", CommentOptions{Kind: KindIssue})
			t.Logf("rule=%s verdict=%s\ntext: %s\n--- comment ---\n%s", d.Rule, d.Verdict, d.Text, body)
		})
	}
}

// An operator-action decision never carries the approval that makes an
// issue actionable, never posts a `/hive decision` line the unpark sweep
// would consume, and names the issue-side park label.
func TestOperatorActionKeepsIssueParked(t *testing.T) {
	t.Parallel()
	in := fixtureHive15021
	in.Number = 15021
	d := Run(in)
	if !d.KeepsPark {
		t.Fatalf("operator action must keep the park: %+v", d)
	}
	for _, a := range d.Actions {
		if a.Kind == ActionCommentDecision || a.Kind == ActionApproveReview {
			t.Fatalf("operator action must not record an approval: %+v", d.Actions)
		}
	}
	body := CommentBodyWith(d, "h", "", CommentOptions{Kind: KindIssue})
	if strings.Contains(body, "/hive decision") || !strings.Contains(body, "`"+LabelIssueNeedsDecision+"` stays") {
		t.Fatalf("comment:\n%s", body)
	}
	eff := ExpectedEffect(d, KindIssue)
	if eff.Waits || !strings.HasPrefix(eff.IfNot, "Waiting on the spoke owner: ") || !strings.Contains(eff.IfNot, "@clubanderson") {
		t.Fatalf("effect = %+v", eff)
	}
}

func TestParseSelfReport(t *testing.T) {
	t.Parallel()
	sr, ok := ParseSelfReport(fixtureHive15021.Title, fixtureHive15021.Body)
	if !ok || sr.Class != "agent_login_required" || sr.Hive != "hosted-ibm-alchemy-logg-ptoo" || sr.Spoke != "hosted-ibm-alchemy-logg-ptoo" {
		t.Fatalf("%+v %v", sr, ok)
	}
	if len(sr.Owners) != 1 || sr.Owners[0] != "clubanderson" || !strings.HasPrefix(sr.SpokeURL, "https://hosted-ibm-alchemy-logg-ptoo.") {
		t.Fatalf("%+v", sr)
	}
	// Title-only form, no marker: the class comes from the prefix.
	sr, ok = ParseSelfReport("[selfreport] data_disk_pressure: disk 97% full", "no table here")
	if !ok || sr.Class != "data_disk_pressure" || sr.Spoke != "" {
		t.Fatalf("%+v %v", sr, ok)
	}
	if _, ok := ParseSelfReport("[quality] something about login required", "class `agent_login_required`"); ok {
		t.Fatal("a body that only mentions a class is not a self-report")
	}
}

// Code-defect classes stay on the implementer path: a panic self-report is
// still decided by the old rules, not handed to an operator.
func TestSelfreportCodeDefectClassStaysOnOldPath(t *testing.T) {
	t.Parallel()
	in := fixtureHive15021
	in.Number = 2
	in.Title = "[selfreport] panic_recovered: index out of range"
	in.Body = strings.Replace(in.Body, "class `agent_login_required`", "class `panic_recovered`", 1)
	d := Run(in)
	if d.Verdict == VerdictOperatorAction || d.Rule == "selfreport-operator-action" {
		t.Fatalf("panic self-report must not be an operator action: %+v", d)
	}
	if _, ok := selfreportOpsClass("unclassified_error_repeated"); ok {
		t.Fatal("unclassified_error_repeated is a code defect")
	}
}

// The ops-class table mirrors pkg/fleetreport's constants by value so a
// renamed class fails here rather than silently falling to default-approve.
func TestSelfreportOpsClassesMatchFleetReport(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, c := range []string{
		fleetreport.ClassAgentLoginRequired, fleetreport.ClassInferenceAuthFailed, fleetreport.ClassDataDiskPressure,
		fleetreport.ClassGitHubAppTokenMintFailed, fleetreport.ClassGitHubAppNotInstalled, fleetreport.ClassGitHubAppRepoNotCovered,
		fleetreport.ClassProxyConnectDialFailed, fleetreport.ClassProxyTLSHandshakeFailed, fleetreport.ClassCruxRuntimeUnavailable,
		fleetreport.ClassModelDiscoveryDegraded, fleetreport.ClassInferenceUpstreamError, fleetreport.ClassInferenceCallerUnknown,
		fleetreport.ClassLabelProvisioningFailed, fleetreport.ClassSharedCIRerunForbidden,
	} {
		known[c] = true
	}
	for class := range selfreportOpsRemedies {
		if !known[class] {
			t.Errorf("ops class %q is not a fleetreport class", class)
		}
	}
	for class := range known {
		if _, ok := selfreportOpsClass(class); !ok {
			t.Errorf("fleetreport class %q missing from the ops table", class)
		}
	}
}

func TestProposalEvidence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, title, body string
		kind              string
	}{
		{"incident ref", "[quality] Gate config", "v5 broke (incident #15322)", EvidenceIncident},
		{"breakage word", "[scanner] gate: x", "this regression shipped twice", EvidenceIncident},
		{"fixed in twice", "[scanner] gate", "1. #1 (fixed in #2). 2. #3 (fixed in #4).", EvidenceRecurrence},
		{"fixing PRs list", "[scanner] gate", "Fixing PRs: #415, #417.", EvidenceRecurrence},
		{"one fix only", "[scanner] gate", "#1 (fixed in #2) — nothing else", ""},
		{"third time but no fixes", fixtureSite296.Title, fixtureSite296.Body, ""},
		{"bare refs are not recurrence", "[quality] gate", "see #248, #257, #295 for prior discussion", ""},
	}
	for _, tc := range cases {
		kind, detail, ok := ProposalEvidence(tc.title, tc.body)
		if (tc.kind != "") != ok || kind != tc.kind {
			t.Errorf("%s: kind=%q ok=%v detail=%q; want %q", tc.name, kind, ok, detail, tc.kind)
		}
	}
}

func TestIsProposal(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"[quality] Gate config: x", "[scanner] something", "[architect] plan", "[strategist] plan", "[outreach] post", "Add a gate proposal for y", "📝 RFC: foo"} {
		if !IsProposal(title, nil) {
			t.Errorf("%q should be a proposal", title)
		}
	}
	for _, title := range []string{"fix: nil deref in parser", "[selfreport] agent_login_required", "rfc822 date parsing is wrong", "bug: performance regression"} {
		if IsProposal(title, nil) {
			t.Errorf("%q should not be a proposal", title)
		}
	}
	if !IsProposal("anything", []string{"kind/design"}) || !IsProposal("anything", []string{"kind/proposal"}) {
		t.Error("design/proposal labels mark a proposal")
	}
}

// A person's proposal without evidence is never declined by the hive; it
// keeps the reversible default. A bot's is declined for now.
func TestProposalNotNowOnlyForHiveFiled(t *testing.T) {
	t.Parallel()
	human := fixtureSite296
	human.Number = 3
	human.Author = "hanthor"
	if d := Run(human); d.Rule != "default-approve" || d.Verdict != VerdictApprove {
		t.Fatalf("human proposal: %+v", d)
	}
	bot := fixtureSite296
	bot.Number = 4
	bot.Author = "someone-else[bot]"
	if d := Run(bot); d.Rule != "proposal-not-now" {
		t.Fatalf("bot proposal: %+v", d)
	}
	viaFlag := fixtureSite296
	viaFlag.Number = 5
	viaFlag.Author = ""
	viaFlag.HiveAuthored = true
	if d := Run(viaFlag); d.Rule != "proposal-not-now" {
		t.Fatalf("hive-authored proposal: %+v", d)
	}
	// Hive's own policy stays recommend-only even when hive-filed and
	// evidence-free.
	policy := fixtureSite296
	policy.Number = 6
	policy.Labels = append(policy.Labels, labelGovernance)
	if d := Run(policy); d.Rule != "hive-policy" {
		t.Fatalf("hive-policy proposal: %+v", d)
	}
}

// The hive-policy recommendation on an issue is a sentence the owner can
// act on, not "no special rule applies".
func TestHivePolicyRecommendationIsReal(t *testing.T) {
	t.Parallel()
	in := fixtureHive15329
	in.Number = 15329
	d := Run(in)
	body := CommentBodyWith(d, "h", "", CommentOptions{Kind: KindIssue})
	for _, want := range []string{"hive recommends: approve `📝 RFC: admission-side #5117", "should not be actionable", "default-on", "`stage/045-needs-decision` stays"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	// A PR-side policy item keeps the generic wording.
	if got := hivePolicyRecommendation(Input{Kind: KindPR, Title: "x"}); !strings.Contains(got, "accept the proposal as written") {
		t.Errorf("PR recommendation = %q", got)
	}
}

func TestGlossaryCoversIssueJudgment(t *testing.T) {
	t.Parallel()
	for _, slug := range []string{"selfreport-operator-action", "proposal-approve-evidence", "proposal-not-now", string(VerdictOperatorAction), string(VerdictNotNow), string(ActionCloseNotNow)} {
		e, ok := Explain(slug)
		if !ok {
			t.Errorf("no glossary entry for %q", slug)
			continue
		}
		if n := len(strings.Fields(e.Plain)); n > 20 {
			t.Errorf("%q plain words are %d words; keep them ≤ 20 for the dashboard", slug, n)
		}
	}
	if got := ActionPlainOn(Decision{Verdict: VerdictNotNow}, KindIssue); !strings.Contains(got, "declined for now") {
		t.Errorf("not-now plain = %q", got)
	}
}

// hivecommons/hive#15754: a hive advisory is never a person's decision.
// Items open → refused, no writes, no park wording; all resolved (or no
// checklist) → close as completed. The rails must let the close through.
func TestAdvisoryDecision(t *testing.T) {
	t.Parallel()
	base := Input{Repo: "o/r", Number: 15734, Kind: KindIssue, Title: "triage: duplicate and overlapping open issues/PRs to consolidate",
		Body: "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\n- [x] #1 — close\n- [ ] #2 — close\n", Labels: []string{"agent/dedupe"}, HiveAuthored: true}
	for _, tc := range []struct {
		name        string
		adv         AdvisoryChecklist
		wantVerdict Verdict
		wantRule    string
		wantActions int
		wantPark    bool
	}{
		{"items open", AdvisoryChecklist{Resolved: 1, Total: 3}, VerdictRefused, RuleAdvisoryInProgress, 0, true},
		{"all resolved", AdvisoryChecklist{Resolved: 3, Total: 3}, VerdictClose, RuleAdvisoryResolved, 1, false},
		{"no checklist", AdvisoryChecklist{}, VerdictClose, RuleAdvisoryResolved, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.Advisory = &tc.adv
			d := Run(in)
			if d.Verdict != tc.wantVerdict || d.Rule != tc.wantRule || len(d.Actions) != tc.wantActions || d.KeepsPark != tc.wantPark {
				t.Fatalf("%s: %+v", tc.name, d)
			}
			if tc.wantActions == 1 && d.Actions[0].Kind != ActionCloseCompleted {
				t.Fatalf("%s: action %+v", tc.name, d.Actions[0])
			}
			if tc.adv.Total > 0 && tc.adv.Resolved < tc.adv.Total && !strings.Contains(d.Rationale, "1/3 resolved") {
				t.Fatalf("%s: rationale %q", tc.name, d.Rationale)
			}
			body := CommentBodyWith(d, "h", "", CommentOptions{Kind: KindIssue})
			if tc.wantVerdict == VerdictClose && (!strings.Contains(body, "Closed as completed") || strings.Contains(body, "/hive decision")) {
				t.Fatalf("%s: body\n%s", tc.name, body)
			}
		})
	}
	// A dedupe checklist with a known duplicate target is still an
	// advisory, not a duplicate to close.
	in := base
	in.Advisory = &AdvisoryChecklist{Resolved: 0, Total: 2}
	in.DuplicateOf = 1
	if d := Run(in); d.Rule != RuleAdvisoryInProgress {
		t.Fatalf("advisory outranks dedupe: %+v", d)
	}
}

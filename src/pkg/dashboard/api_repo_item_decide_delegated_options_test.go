package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// projectbluefin/utah#920: three mutually exclusive delivery options, parked
// at 045. "Let hive decide" offered "Approve the direction of #920 and let
// the hive pick it up", which recorded no choice. It must name the option it
// goes with, or leave the choice to a person.
const delegatedOptionsBody = "## Problem\n\nNo debugger ships.\n\n## Proposal\n\nPick one:\n\n" +
	"1. **Carry a small set in the image:** `gdb-headless`, `elfutils`, `sysprof-cli`.\n" +
	"2. **A `ujust` recipe** that installs the set on demand.\n" +
	"3. **A `-dx`/debug flavor** only, leaving the default image alone.\n"

func TestDelegatedPlanWordingNamesTheOptionOrLeavesTheChoice(t *testing.T) {
	options := []delegatedecision.Option{{Label: "1", Summary: "image"}, {Label: "2", Summary: "A ujust recipe"}, {Label: "3", Summary: "-dx flavor"}}
	in := delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 920, Options: options}

	summary, next := delegatedPlanWording(in, delegatedecision.Decision{Verdict: delegatedecision.VerdictRefused, Rule: delegatedecision.RuleCompetingOptions, KeepsPark: true})
	if want := "Leave the choice to you: #920 asks a person to choose between 3 options (1, 2, 3) and recommends none."; summary != want {
		t.Errorf("refusal summary %q, want %q", summary, want)
	}
	if strings.Join(next, "|") != "Nothing changes; #920 stays waiting for you.|Use Give direction to name the option the hive should build." {
		t.Errorf("refusal consequences %q", next)
	}

	chosen := options[1]
	summary, next = delegatedPlanWording(in, delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: delegatedecision.RuleRecommendedOption, Option: &chosen})
	if want := "Go with option 2: A ujust recipe — the option #920 recommends — and let the hive build it."; summary != want {
		t.Errorf("approve summary %q, want %q", summary, want)
	}
	if strings.Join(next, "|") != "Hive records option 2 as the decision and removes the needs-decision hold.|An agent claims #920 on the next cycle and builds option 2." {
		t.Errorf("approve consequences %q", next)
	}
	for _, s := range append([]string{summary}, next...) {
		if strings.Contains(s, "the direction") {
			t.Errorf("an issue with options must never read as a bare direction: %q", s)
		}
	}

	// A reporter-authored option names itself by label alone in the recorded
	// decision, but the preview still shows the owner what they approve: the
	// issue's own words, quoted, on one line.
	reporter := delegatedecision.Option{Label: "2", Summary: "A ujust\n  recipe.", Untrusted: true}
	summary, next = delegatedPlanWording(in, delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: delegatedecision.RuleRecommendedOption, Option: &reporter})
	if want := "Go with option 2 (the issue's words: “A ujust recipe”) — the option #920 recommends — and let the hive build it."; summary != want {
		t.Errorf("untrusted approve summary %q, want %q", summary, want)
	}
	if reporter.Name() != "option 2" {
		t.Errorf("untrusted Name() = %q, want the label alone", reporter.Name())
	}
	if strings.Join(next, "|") != "Hive records option 2 as the decision and removes the needs-decision hold.|An agent claims #920 on the next cycle and builds option 2." {
		t.Errorf("untrusted approve consequences %q", next)
	}
	empty := delegatedecision.Option{Label: "2", Untrusted: true}
	if got, _ := delegatedPlanWording(in, delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: delegatedecision.RuleRecommendedOption, Option: &empty}); !strings.HasPrefix(got, "Go with option 2 — ") {
		t.Errorf("empty untrusted summary must add no quote: %q", got)
	}
}

func TestDelegatedDecisionPlanForAnIssueWithOptions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantWould delegatedecision.Verdict
		wantRule  string
		wantSum   string
		wantWhy   string
	}{
		{"no recommendation", delegatedOptionsBody, delegatedecision.VerdictRefused, delegatedecision.RuleCompetingOptions,
			"Leave the choice to you: #7 asks a person to choose between 3 options (1, 2, 3) and recommends none.",
			"The issue asks a person to choose between 3 options (1, 2, 3) and recommends none."},
		{"recommends option 2", delegatedOptionsBody + "\nRecommendation: option 2, it keeps the image small.\n", delegatedecision.VerdictApprove, delegatedecision.RuleRecommendedOption,
			"Go with option 2: A `ujust` recipe that installs the set on demand — the option #7 recommends — and let the hive build it.",
			"The issue offers 3 options and recommends option 2."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{issueTitle: "Ship debugging tools", issueBody: tc.body, issueLabels: []string{"kind/feature", "stage/045-needs-decision"}}
			srv := newDelegatedServer(t, mock)
			w := postDelegatedPlan(t, srv, 7, "writer", `{"type":"issue"}`, map[string]string{"X-Hive-Role": "merger"})
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var plan delegatedDecisionPlan
			if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.Would != string(tc.wantWould) || plan.Rule != tc.wantRule || plan.Summary != tc.wantSum || plan.Why != tc.wantWhy || plan.Writes {
				t.Fatalf("plan would=%q rule=%q summary=%q why=%q writes=%v", plan.Would, plan.Rule, plan.Summary, plan.Why, plan.Writes)
			}
			hasDirection := false
			for _, alt := range plan.Alternatives {
				hasDirection = hasDirection || (alt.Action == decisionActionDirection && alt.Enabled)
			}
			if !hasDirection {
				t.Errorf("Give direction must stay offered: %+v", plan.Alternatives)
			}
			if len(mock.comments) != 0 || len(mock.edits) != 0 {
				t.Fatalf("a plan may not write: %v %v", mock.comments, mock.edits)
			}
		})
	}
}

// Running the decision: with no recommendation nothing is written; with one,
// the recorded `/hive decision` names the option the claiming agent builds.
func TestDelegatedDecisionIssueWithOptionsRecordsTheChoice(t *testing.T) {
	mock := &repoItemDecisionMock{issueBody: delegatedOptionsBody, issueLabels: []string{"stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	job := decodeDelegatedJob(t, postDelegated(t, srv, 7, "writer", `{"type":"issue"}`, nil))
	if job.Decision == nil || job.Decision.Rule != delegatedecision.RuleCompetingOptions {
		t.Fatalf("job = %+v", job)
	}
	if len(mock.comments) != 0 || len(mock.added) != 0 || len(mock.removed) != 0 {
		t.Fatalf("a refusal must write nothing: comments=%v added=%v removed=%v", mock.comments, mock.added, mock.removed)
	}

	mock = &repoItemDecisionMock{issueBody: delegatedOptionsBody + "\nI recommend option 2.\n", issueLabels: []string{"stage/045-needs-decision"}}
	srv = newDelegatedServer(t, mock)
	job = decodeDelegatedJob(t, postDelegated(t, srv, 7, "writer", `{"type":"issue"}`, nil))
	if job.Decision == nil || job.Decision.Rule != delegatedecision.RuleRecommendedOption {
		t.Fatalf("job = %+v", job)
	}
	if len(mock.comments) == 0 || !strings.HasPrefix(mock.comments[0], "/hive decision go with option 2: A `ujust` recipe that installs the set on demand.") {
		t.Fatalf("recorded decision does not name option 2: %v", mock.comments)
	}
}

package github

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Owner decision hivecommons/hive#13298: triage-needed is not a stage for
// hive agents and not for trusted humans. A triage bot (Prow) puts
// needs-triage on EVERY new issue, so the label alone held maintainer- and
// hive-filed issues at 015 and in the contributor skip list. The label now
// counts only for an untrusted (or unknown) author.

// TestIssueStageFor_NeedsTriageByAuthorTrust pins the 010/015 split on the
// precomputed AuthorTrusted. The reporter_triage bucket is unchanged: only
// untrusted reporters land there, and it derives 015 whoever is named.
func TestIssueStageFor_NeedsTriageByAuthorTrust(t *testing.T) {
	cases := []struct {
		name string
		in   IssueStageInput
		want stagelabels.Stage
		why  string
	}{
		{"trusted human (MEMBER) + needs-triage is 010", IssueStageInput{AuthorKind: authorkind.Human, AuthorTrusted: true, Labels: []string{triageAwaitingLabel, "kind/bug"}}, stagelabels.Actionable, ""},
		{"hive-filed + needs-triage is 010", IssueStageInput{AuthorKind: authorkind.Hive, AuthorTrusted: true, Labels: []string{triageAwaitingLabel}}, stagelabels.Actionable, ""},
		{"trusted + Needs-Triage (case) is 010", IssueStageInput{AuthorKind: authorkind.Human, AuthorTrusted: true, Labels: []string{"Needs-Triage"}}, stagelabels.Actionable, ""},
		{"untrusted human + needs-triage is 015", IssueStageInput{AuthorKind: authorkind.Human, Labels: []string{triageAwaitingLabel}}, stagelabels.TriageNeeded, "carries needs-triage"},
		{"unknown author + needs-triage is 015", IssueStageInput{Labels: []string{triageAwaitingLabel}}, stagelabels.TriageNeeded, "carries needs-triage"},
		{"reporter_triage bucket + label is 015 (label named first)", IssueStageInput{AuthorKind: authorkind.Human, Bucket: workBucketReporterTriage, Labels: []string{triageAwaitingLabel}}, stagelabels.TriageNeeded, "carries needs-triage"},
		{"reporter_triage bucket is 015", IssueStageInput{AuthorKind: authorkind.Human, Bucket: workBucketReporterTriage}, stagelabels.TriageNeeded, "reporter-trust gate"},
		{"reporter_triage bucket is 015 even if marked trusted", IssueStageInput{AuthorKind: authorkind.Human, AuthorTrusted: true, Bucket: workBucketReporterTriage}, stagelabels.TriageNeeded, "reporter-trust gate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, covered, reason := IssueStageFor(tc.in, nil)
			if got != tc.want || covered {
				t.Fatalf("IssueStageFor = %s/%v %q, want %s", got, covered, reason, tc.want)
			}
			if got == stagelabels.TriageNeeded && !strings.Contains(reason, tc.why) {
				t.Fatalf("reason %q must name %q", reason, tc.why)
			}
			if got == stagelabels.Actionable && strings.Contains(reason, "triage") {
				t.Fatalf("010 reason %q must not mention triage", reason)
			}
		})
	}
	// The verdict rides through the engine's Facts, so an out-of-cycle
	// re-derivation (issueInputFrom) agrees with the sweep.
	in := IssueStageInput{Repo: "o/r", Number: 1, AuthorKind: authorkind.Human, AuthorTrusted: true, Labels: []string{triageAwaitingLabel}}
	f := issueFactsFrom(in, nil)
	if !f.Issue.AuthorTrusted || f.Issue.TriageAwaiting != "" || f.Issue.TriageAwaitingLabel != triageAwaitingLabel {
		t.Fatalf("facts = %+v, want trusted, not awaiting, label still named", f.Issue)
	}
	if back := issueInputFrom(f); !back.AuthorTrusted {
		t.Fatalf("issueInputFrom dropped AuthorTrusted: %+v", back)
	}
	if d := IssueDeriver().Derive(f); d.Stage != stagelabels.Actionable {
		t.Fatalf("deriver stage = %s, want 010", d.Stage.Label())
	}
}

// prowTriageIssues are a repository's open issues as Prow leaves them:
// every one carries needs-triage, whoever filed it.
func prowTriageIssues() []wireIssue {
	prow := []wireLabel{{Name: "kind/bug"}, {Name: "needs-triage"}}
	return []wireIssue{
		{Number: 11, Title: "member filed", User: wireUser{"maintainer"}, AuthorAssociation: "MEMBER", Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 12, Title: "hive filed", User: wireUser{"kubestellar-hive[bot]"}, AuthorAssociation: "NONE", Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 13, Title: "stranger filed", User: wireUser{"stranger"}, AuthorAssociation: "NONE", Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 14, Title: "contributor filed", User: wireUser{"contrib"}, AuthorAssociation: "CONTRIBUTOR", Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 15, Title: "first-time contributor filed", User: wireUser{"newbie"}, AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 16, Title: "trusted login filed", User: wireUser{"external-maintainer"}, AuthorAssociation: "NONE", Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 17, Title: "author unknown", User: wireUser{""}, Labels: prow, CreatedAt: hoursAgo(5)},
		{Number: 18, Title: "stranger filed, accepted", User: wireUser{"stranger3"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "needs-triage"}, {Name: "triage/accepted"}}, CreatedAt: hoursAgo(5)},
		// A third-party App (not this hive's) copying a user's text: its
		// CONTRIBUTOR association never counts, Prow's label still holds it.
		{Number: 19, Title: "third-party app filed", User: wireUser{"github-actions[bot]"}, AuthorAssociation: "CONTRIBUTOR", Labels: prow, CreatedAt: hoursAgo(5)},
		// A bot the operator listed in trusted_logins (Prow's own, say).
		{Number: 20, Title: "trusted bot filed", User: wireUser{"triage-robot[bot]"}, AuthorAssociation: "NONE", Labels: prow, CreatedAt: hoursAgo(5)},
	}
}

// prowTriageAppBot is the hive App's bot login in prowTriageIssues (#12).
const prowTriageAppBot = "kubestellar-hive[bot]"

func enumerateProwTriage(t *testing.T, f config.IssueFilterConfig) *ActionableResult {
	t.Helper()
	org, repo := "testorg", "testrepo"
	server := httptest.NewServer(buildMux(t, org, repo, prowTriageIssues(), nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin(prowTriageAppBot)
	c.SetIssueFilter(f)
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return result
}

// enumeratedStage derives the stage the issue sweep would write for number.
func enumeratedStage(t *testing.T, result *ActionableResult, number int) (stagelabels.Stage, IssueStageInput) {
	t.Helper()
	for _, in := range IssueStageInputs(result) {
		if in.Number == number {
			stage, _, _ := IssueStageFor(in, nil)
			return stage, in
		}
	}
	t.Fatalf("issue #%d not enumerated", number)
	return stagelabels.None, IssueStageInput{}
}

func enumeratedActionable(result *ActionableResult, number int) (Issue, bool) {
	for _, is := range result.Issues.Items {
		if is.Number == number {
			return is, true
		}
	}
	return Issue{}, false
}

// TestEnumerateActionable_ProwNeedsTriageTrust is the end-to-end pin: the
// enumeration's AuthorTrusted, the stage it derives, and the contribute skip
// list's verdict on the same envelope, with the reporter-trust gate off
// and on. The trust predicate is the gate's own (ReporterTrustConfig
// .Trusted over the effective associations), applied either way.
func TestEnumerateActionable_ProwNeedsTriageTrust(t *testing.T) {
	trustedLogin := []string{"external-maintainer", "triage-robot[bot]"}
	on := true
	off := config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{TrustedLogins: trustedLogin}}
	gateOn := config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{Enabled: &on, TrustedLogins: trustedLogin}}
	hub := config.HubConfig{}

	type want struct {
		trusted    bool
		stage      stagelabels.Stage
		actionable bool
	}
	runs := []struct {
		name   string
		filter config.IssueFilterConfig
		want   map[int]want
	}{
		{"gate off", off, map[int]want{
			11: {true, stagelabels.Actionable, true},
			12: {true, stagelabels.Actionable, true},
			13: {false, stagelabels.TriageNeeded, true},
			14: {false, stagelabels.TriageNeeded, true},
			15: {false, stagelabels.TriageNeeded, true},
			16: {true, stagelabels.Actionable, true},
			17: {false, stagelabels.TriageNeeded, true},
			18: {false, stagelabels.TriageNeeded, true},
			19: {false, stagelabels.TriageNeeded, true},
			20: {true, stagelabels.Actionable, true},
		}},
		// Gate on: an untrusted reporter's issue without triage/accepted
		// never reaches the actionable set (the gate's own hold, the
		// reporter_triage bucket, 015); with triage/accepted it is admitted
		// but Prow's needs-triage still derives 015 and skips it.
		{"gate on", gateOn, map[int]want{
			11: {true, stagelabels.Actionable, true},
			12: {true, stagelabels.Actionable, true},
			13: {false, stagelabels.TriageNeeded, false},
			14: {false, stagelabels.TriageNeeded, false},
			15: {false, stagelabels.TriageNeeded, false},
			16: {true, stagelabels.Actionable, true},
			17: {false, stagelabels.TriageNeeded, true},
			18: {false, stagelabels.TriageNeeded, true},
			// The gate judges only human reporters; a bot's issue is
			// admitted, and only the trust predicate releases it.
			19: {false, stagelabels.TriageNeeded, true},
			20: {true, stagelabels.Actionable, true},
		}},
	}
	for _, run := range runs {
		t.Run(run.name, func(t *testing.T) {
			result := enumerateProwTriage(t, run.filter)
			for number, w := range run.want {
				stage, in := enumeratedStage(t, result, number)
				if stage != w.stage {
					t.Errorf("#%d stage = %s, want %s (input %+v)", number, stage.Label(), w.stage.Label(), in)
				}
				if in.AuthorTrusted != w.trusted {
					t.Errorf("#%d AuthorTrusted = %v, want %v", number, in.AuthorTrusted, w.trusted)
				}
				is, ok := enumeratedActionable(result, number)
				if ok != w.actionable {
					t.Errorf("#%d actionable = %v, want %v", number, ok, w.actionable)
					continue
				}
				if !ok {
					if in.Bucket != workBucketReporterTriage {
						t.Errorf("#%d bucket = %q, want %s (the gate's hold)", number, in.Bucket, workBucketReporterTriage)
					}
					continue
				}
				label, skipped := hub.MatchContributeSkipLabelFor(is.Labels, is.AuthorTrusted)
				if skipped == w.trusted || (skipped && label != "needs-triage") {
					t.Errorf("#%d skip = (%q,%v), want skipped=%v on needs-triage", number, label, skipped, !w.trusted)
				}
			}
		})
	}

	// A custom trusted-association set that leaves MEMBER out makes the
	// member's issue untrusted again: 015 and skipped.
	custom := off
	custom.ReporterTrust.TrustedAssociations = []string{config.AuthorAssociationOwner}
	result := enumerateProwTriage(t, custom)
	if stage, in := enumeratedStage(t, result, 11); stage != stagelabels.TriageNeeded || in.AuthorTrusted {
		t.Fatalf("MEMBER outside trusted_associations: stage %s trusted %v, want 015 untrusted", stage.Label(), in.AuthorTrusted)
	}
	if stage, in := enumeratedStage(t, result, 12); stage != stagelabels.Actionable || !in.AuthorTrusted {
		t.Fatalf("hive-filed issue is trusted whatever the association set: stage %s trusted %v", stage.Label(), in.AuthorTrusted)
	}
}

// The stage sink's fresh read (enrichStageFacts) decides trust the same way,
// so an out-of-cycle fact on a trusted author's needs-triage issue derives
// 010 instead of the 015 the next sweep would take back.
func TestEnrichStageFacts_AuthorTrusted(t *testing.T) {
	cases := []struct {
		name    string
		login   string
		filter  IssueAdmitter
		trusted bool
	}{
		{"hive App author", "hive-app[bot]", nil, true},
		{"third-party bot author", "github-actions[bot]", nil, false},
		{"bot in trusted_logins", "triage-robot[bot]", config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{TrustedLogins: []string{"triage-robot[bot]"}}}, true},
		{"human without association", "dev", nil, false},
		{"human in trusted_logins", "dev", config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{TrustedLogins: []string{"dev"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStageFactsServer(t, triageAwaitingLabel)
			srv.login = tc.login
			c := testClient(t, srv.srv.URL)
			c.SetAppBotLogin("hive-app[bot]")
			if tc.filter != nil {
				c.SetIssueFilter(tc.filter)
			}
			f, err := c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: c.IssueStageRef("o/r", 7), Issue: &stageengine.IssueFacts{Bucket: stageengine.BucketActionable}})
			if err != nil {
				t.Fatal(err)
			}
			if f.Issue.AuthorTrusted != tc.trusted {
				t.Fatalf("AuthorTrusted = %v, want %v", f.Issue.AuthorTrusted, tc.trusted)
			}
			awaiting := f.Issue.TriageAwaiting != ""
			if awaiting == tc.trusted {
				t.Fatalf("TriageAwaiting = %q with trusted=%v", f.Issue.TriageAwaiting, tc.trusted)
			}
		})
	}
}

// issueAuthorTrusted's unknown-author rule: no login is not trusted, the
// opposite of the display rule that reads an unknown author as human.
func TestIssueAuthorTrustedUnknownIsUntrusted(t *testing.T) {
	var c *Client
	if c.issueAuthorTrusted(nil) {
		t.Fatal("nil issue must not be trusted")
	}
	if !(&Client{}).reporterTrustTrusts("someone", config.AuthorAssociationMember) {
		t.Fatal("a client without a filter falls back to the default trusted associations")
	}
	if (&Client{}).reporterTrustTrusts("someone", config.AuthorAssociationContributor) {
		t.Fatal("CONTRIBUTOR is not in the default trusted set")
	}
}

// issueAuthorTrusted trusts this hive's own accounts and, for everyone else,
// only the reporter-trust predicate; another App or bot only by an explicit
// trusted_logins entry, never by its author_association.
func TestIssueAuthorTrustedBotsAndHiveAccounts(t *testing.T) {
	issue := func(login, typ, association string) *gh.Issue {
		u := &gh.User{Login: gh.Ptr(login)}
		if typ != "" {
			u.Type = gh.Ptr(typ)
		}
		return &gh.Issue{User: u, AuthorAssociation: gh.Ptr(association)}
	}
	c := &Client{}
	c.SetAppBotLogin("acme-hive[bot]")
	// An operator who trusts CONTRIBUTOR and MEMBER for humans.
	c.SetIssueFilter(config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{
		TrustedAssociations: []string{config.AuthorAssociationContributor, config.AuthorAssociationMember},
		TrustedLogins:       []string{"prow-robot[bot]"},
	}})
	cases := []struct {
		name  string
		issue *gh.Issue
		want  bool
	}{
		{"this hive's App", issue("acme-hive[bot]", "Bot", "NONE"), true},
		{"third-party App as CONTRIBUTOR", issue("github-actions[bot]", "Bot", "CONTRIBUTOR"), false},
		{"third-party App as MEMBER", issue("other-app[bot]", "Bot", "MEMBER"), false},
		{"Type=Bot without [bot] suffix as CONTRIBUTOR", issue("someci", "Bot", "CONTRIBUTOR"), false},
		{"bot in trusted_logins", issue("prow-robot[bot]", "Bot", "NONE"), true},
		{"human CONTRIBUTOR trusted by the operator's set", issue("alice", "User", "CONTRIBUTOR"), true},
		{"human NONE", issue("mallory", "User", "NONE"), false},
		{"no login", issue("", "", "MEMBER"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.issueAuthorTrusted(tc.issue); got != tc.want {
				t.Fatalf("issueAuthorTrusted = %v, want %v", got, tc.want)
			}
		})
	}
}

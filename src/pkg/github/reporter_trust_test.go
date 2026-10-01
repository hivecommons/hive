package github

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

// ---------- admission: enumeration honours the reporter gate ----------

func reporterTrustIssues() []wireIssue {
	return []wireIssue{
		{Number: 1, Title: "maintainer asks, no label", User: wireUser{"maintainer"}, AuthorAssociation: "MEMBER",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(4)},
		{Number: 2, Title: "stranger asks, no label", User: wireUser{"stranger"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(3)},
		{Number: 3, Title: "stranger asks, triaged", User: wireUser{"stranger2"}, AuthorAssociation: "FIRST_TIMER",
			Labels: []wireLabel{{Name: "triage/accepted"}}, CreatedAt: hoursAgo(2)},
		{Number: 4, Title: "hive filed it", User: wireUser{"kubestellar-hive[bot]"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(1)},
		{Number: 5, Title: "trusted login, no association", User: wireUser{"external-maintainer"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(1)},
	}
}

func enumerateWithReporterTrust(t *testing.T, f config.IssueFilterConfig) *ActionableResult {
	t.Helper()
	org, repo := "testorg", "testrepo"
	mux := buildMux(t, org, repo, reporterTrustIssues(), nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(f)
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return result
}

func enabledReporterTrust() config.IssueFilterConfig {
	on := true
	return config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{
		Enabled:       &on,
		TrustedLogins: []string{"external-maintainer"},
	}}
}

// TestEnumerateActionable_ReporterTrust pins the gate at THE choice point:
// a maintainer's unlabelled issue is actionable, a stranger's is not until
// triaged, and a stranger's triaged issue is. The bot-filed issue is not a
// "reporter" and is left to #5117 on the PR side.
func TestEnumerateActionable_ReporterTrust(t *testing.T) {
	result := enumerateWithReporterTrust(t, enabledReporterTrust())
	nums := actionableNumbers(result)
	if !nums[1] {
		t.Error("positive control failed: the MEMBER's unlabelled issue must be actionable")
	}
	if nums[2] {
		t.Error("the stranger's untriaged issue entered the actionable set — the reporter gate is not enforced")
	}
	if !nums[3] {
		t.Error("the stranger's issue carrying triage/accepted must be actionable")
	}
	if !nums[4] {
		t.Error("a hive/bot-filed issue is not judged by the reporter gate; #5117 owns it")
	}
	if !nums[5] {
		t.Error("an explicitly trusted login must be admitted whatever its association")
	}
	var triage int
	for _, r := range result.WorkBreakdownByRepo {
		triage += r.Issues.ReporterTriage
	}
	if triage != 1 {
		t.Errorf("reporter_triage = %d, want 1 (issue #2 awaiting triage)", triage)
	}
}

// TestEnumerateActionable_ReporterTrustOffUnchanged is the regression pin for
// every existing hive: with the block absent, all five issues are actionable
// and nothing is counted as awaiting triage.
func TestEnumerateActionable_ReporterTrustOffUnchanged(t *testing.T) {
	result := enumerateWithReporterTrust(t, config.IssueFilterConfig{})
	if got := result.Issues.Count; got != 5 {
		t.Errorf("Issues.Count = %d, want 5 — absent reporter_trust must change nothing", got)
	}
	for _, r := range result.WorkBreakdownByRepo {
		if r.Issues.ReporterTriage != 0 {
			t.Errorf("reporter_triage = %d with the gate off", r.Issues.ReporterTriage)
		}
	}
}

// TestEnumerateActionable_ReporterTrustComposesWithRequireLabels: the
// ordinary allow-list still applies to trusted reporters afterwards, so
// "everyone needs an approval label" remains expressible exactly as before.
func TestEnumerateActionable_ReporterTrustComposesWithRequireLabels(t *testing.T) {
	f := enabledReporterTrust()
	f.RequireLabels = []string{"triage/accepted"}
	result := enumerateWithReporterTrust(t, f)
	nums := actionableNumbers(result)
	if nums[1] {
		t.Error("the MEMBER's issue passed the reporter gate but lacks the required label; require_labels must still refuse it")
	}
	if !nums[3] {
		t.Error("the triaged stranger's issue satisfies both gates and must be actionable")
	}
}

// ---------- the PR-side evaluator ----------

func reporterTrustTestClient(t *testing.T, srv *selfAuthServer, holdActive bool) *Client {
	t.Helper()
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin("kubestellar-hive[bot]")
	c.SetReporterTrustHoldEnabled(func(string) bool { return holdActive })
	trust := enabledReporterTrust().ReporterTrust
	c.SetReporterTrusted(trust.Trusted)
	return c
}

func TestEvaluateReporterTrust(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	cases := []struct {
		name     string
		issues   map[int]*selfAuthIssue
		body     string
		declared []int
		wantHeld bool
		wantWhy  string
	}{{
		name:     "a stranger asked: hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}},
		body:     "Closes #581",
		wantHeld: true, wantWhy: "not a trusted reporter",
	}, {
		name:     "a maintainer asked: no hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "maintainer", Association: "MEMBER"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "an explicitly trusted login with no association: no hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "external-maintainer"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "association missing from the payload fails toward the hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "somebody"}},
		body:     "Closes #581",
		wantHeld: true, wantWhy: "association unknown",
	}, {
		name:     "the hive filed it: not this gate's business",
		issues:   map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot", Association: "NONE"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name: "one stranger among maintainers still holds",
		issues: map[int]*selfAuthIssue{
			581: {Author: "maintainer", Association: "OWNER"},
			590: {Author: "stranger", Association: "FIRST_TIME_CONTRIBUTOR"},
		},
		body:     "Closes #581\nRefs #590",
		wantHeld: true,
	}, {
		name:     "the request's declared issue list counts",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}},
		body:     "No references in the prose.",
		declared: []int{581},
		wantHeld: true,
	}, {
		name:     "no rationale cited decides nothing",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}},
		body:     "Just a change.",
		wantHeld: false,
	}, {
		name:     "an unreadable issue decides nothing",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE", Status: 500}},
		body:     "Closes #581",
		wantHeld: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: tc.issues}
			c := reporterTrustTestClient(t, srv, true)
			got := c.EvaluateReporterTrust(context.Background(), "o/r", "t", tc.body, tc.declared)
			if got.Held != tc.wantHeld {
				t.Fatalf("Held = %v, want %v (reason %q)", got.Held, tc.wantHeld, got.Reason)
			}
			if tc.wantHeld && tc.wantWhy != "" && !strings.Contains(got.Reason, tc.wantWhy) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

// ---------- the watcher applies it at every level ----------

func runReporterTrustWatcher(t *testing.T, c *Client) (reqPath string) {
	t.Helper()
	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "barbie-theme", Base: "main",
		Title: "make the default dashboard theme Barbie", Body: "Closes #581", Agent: "quality",
	})
	if err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	return reqPath
}

func TestPRRequestWatcher_HoldsUntrustedReporterPRAtL6(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	c := reporterTrustTestClient(t, srv, true)
	// L6: no level hold. The reporter gate is the only thing between a
	// stranger's request and an unattended merge.
	c.prHoldLabel = func(string) bool { return false }

	reqPath := runReporterTrustWatcher(t, c)

	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold]", applied)
	}
	comments := srv.postedComments()
	if len(comments) != 1 {
		t.Fatalf("posted %d comments, want 1 explaining the hold", len(comments))
	}
	for _, want := range []string{ReporterTrustNoticeMarker, "#581", "@stranger", "NONE", "9665"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("hold explanation does not mention %q:\n%s", want, comments[0])
		}
	}
	raw, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var resp PRResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if !resp.OK || !resp.ReporterTrustHeld {
		t.Errorf("result = %+v, want OK with reporter_trust_held so the agent knows why", resp)
	}
	if resp.SelfAuthorized {
		t.Error("a human-filed issue is not a #5117 hold; the two gates must not be confused")
	}
}

func TestPRRequestWatcher_TrustedReporterPRNotHeld(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "maintainer", Association: "COLLABORATOR"}}}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false }
	runReporterTrustWatcher(t, c)
	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v, want none for a collaborator's request", applied)
	}
	if n := len(srv.postedComments()); n != 0 {
		t.Fatalf("posted %d comments, want none", n)
	}
}

func TestPRRequestWatcher_ReporterTrustOffDoesNothing(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	c := reporterTrustTestClient(t, srv, false)
	c.prHoldLabel = func(string) bool { return false }
	runReporterTrustWatcher(t, c)
	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v with the gate off, want none", applied)
	}
}

// At a hold-gated level the label was going on anyway; what matters is that
// the reporter-trust notice ALSO lands, because it is what stops the level
// hold release from lifting the label on promotion to L6.
func TestPRRequestWatcher_HoldGatedLevelStillPostsReporterTrustNotice(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return true }
	runReporterTrustWatcher(t, c)
	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold]", applied)
	}
	comments := srv.postedComments()
	var level, reporter bool
	for _, body := range comments {
		if _, ok := levelHoldAgentFromNotice(body); ok {
			level = true
		}
		if IsReporterTrustHoldNotice(body) {
			reporter = true
		}
	}
	if !level || !reporter {
		t.Fatalf("want both a level notice and a reporter-trust notice, got level=%v reporter=%v in %d comments", level, reporter, len(comments))
	}
}

// ---------- promotion to L6 must not release a reporter-trust hold ----------

func reporterHeldPR(number int) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Ptr(number),
		Title:  gh.Ptr("make the default dashboard theme Barbie"),
		Body:   gh.Ptr("Closes #581"),
		Labels: []*gh.Label{{Name: gh.Ptr("hold")}},
	}
}

func TestReleaseLevelHold_ReporterTrustNoticeBlocksRelease(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	// Seed the PR's comment thread with a level notice AND the reporter notice,
	// as the watcher leaves it at L5; the fake serves s.comments for any
	// number it has no issue for, attributed to the App bot.
	srv.comments = []string{levelHoldNotice("quality"), reporterTrustNotice(ReporterTrust{Held: true, Issue: 581, Repo: "o/r", Reporter: "stranger", Association: "NONE"})}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false } // promoted to L6: level no longer requires the hold

	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", reporterHeldPR(583))
	if err != nil {
		t.Fatalf("releaseLevelHoldIfEligible: %v", err)
	}
	if released || reason != "hold" {
		t.Fatalf("released=%v reason=%q; a reporter-trust hold is a human's to lift", released, reason)
	}
}

func TestReleaseLevelHold_ReEvaluatesAndPostsMissingReporterNotice(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	srv.comments = []string{levelHoldNotice("quality")} // the reporter notice never landed
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false }

	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", reporterHeldPR(583))
	if err != nil {
		t.Fatalf("releaseLevelHoldIfEligible: %v", err)
	}
	if released || reason != "hold" {
		t.Fatalf("released=%v reason=%q; the hold must survive", released, reason)
	}
}

func TestReleaseLevelHold_TrustedReporterStillReleases(t *testing.T) {
	// Positive control: with a maintainer's rationale, the reporter gate does
	// not interfere and the level hold reaches its ordinary release checks.
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "maintainer", Association: "OWNER"}}}
	srv.comments = []string{levelHoldNotice("quality")}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false }

	_, reason, _ := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", reporterHeldPR(583))
	if reason != "hold" {
		t.Fatalf("reason = %q; a trusted reporter's PR must not be held by the reporter gate", reason)
	}
}

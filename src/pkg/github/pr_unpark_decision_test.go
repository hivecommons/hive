package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
)

// Reproduces hivecommons/hive#12968, the live case behind #13041: a
// hive-authored PR parked at stage/085-needs-decision on a swarm
// requires_human verdict for e653b6d, and a maintainer's `/hive decision`
// with a three-point code change.
const (
	liveDecisionPR   = 12968
	liveDecisionHead = "e653b6d0ffee0ffee0ffee0ffee0ffee0ffee0ff"
	liveDecisionText = "keep the change but (1) gate it behind the flag, (2) drop the retry loop, (3) add the regression test"
)

func parkedPR(number int, author string, labels []string, comments ...unparkWireComment) unparkWireIssue {
	var wire []wireLabel
	for _, l := range labels {
		wire = append(wire, wireLabel{Name: l})
	}
	return unparkWireIssue{
		Number:      number,
		Title:       "fix: tunnel drain",
		Body:        "Fixes #1",
		User:        wireUser{Login: author},
		Labels:      wire,
		Comments:    comments,
		PullRequest: &unparkWirePRLink{URL: "https://example.test/pulls/1"},
		HeadSHA:     liveDecisionHead,
	}
}

func hiveParkedPR(comments ...unparkWireComment) unparkWireIssue {
	return parkedPR(liveDecisionPR, "hive[bot]", []string{PRNeedsDecisionStageLabel, "author/hive"}, comments...)
}

func trustedMergerSet(logins ...string) func(string) bool {
	set := map[string]bool{}
	for _, l := range logins {
		set[strings.ToLower(l)] = true
	}
	return func(login string) bool { return set[strings.ToLower(strings.TrimSpace(login))] }
}

func newUnparkPRClient(t *testing.T, issues []unparkWireIssue, permissions map[string]string) (*Client, *unparkServer) {
	t.Helper()
	server, rec := newUnparkServer(t, "org", "repo", issues, permissions)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")
	c.SetStageFactSink(&stageenginetest.Recorder{})
	return c, rec
}

// prDecisionFacts returns the PRDecided facts the client submitted.
func prDecisionFacts(t *testing.T, c *Client) []stageengine.PRDecisionFact {
	t.Helper()
	rec, ok := c.StageFacts().(*stageenginetest.Recorder)
	if !ok {
		t.Fatalf("stage sink is %T, want the test recorder", c.StageFacts())
	}
	var out []stageengine.PRDecisionFact
	for _, f := range rec.All() {
		if f.Method == "PRDecided" && f.Decision != nil {
			out = append(out, *f.Decision)
		}
	}
	return out
}

func TestSweepIssueUnparkCommandsDecisionOnHivePRRoutesFixKick(t *testing.T) {
	issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", "/hive decision "+liveDecisionText))}
	c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
	store := review.NewHumanChangeRequests("")
	var audited []PullRequestDecisionEvent

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		HumanChangeRequests: store,
		TrustedMerger:       trustedMergerSet("clubanderson"),
		PRAudit:             func(e PullRequestDecisionEvent) { audited = append(audited, e) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 1 || len(result.PRDecisions) != 1 || result.Replies != 0 {
		t.Fatalf("result = %+v, want one PR decision", result)
	}
	ev := result.PRDecisions[0]
	if ev.Number != liveDecisionPR || ev.Actor != "clubanderson" || ev.HeadSHA != liveDecisionHead || ev.Decision != liveDecisionText {
		t.Fatalf("event = %+v", ev)
	}
	if len(audited) != 1 || audited[0] != ev {
		t.Fatalf("audit = %+v", audited)
	}
	req, ok := store.For("org/repo", liveDecisionPR, liveDecisionHead)
	if !ok || req.Body != liveDecisionText || req.Source != review.HumanChangeRequestSourceDecision || req.SourceID != 4242 || req.Actor != "clubanderson" || req.URL != "https://example.test/c/4242" {
		t.Fatalf("recorded request = %+v, %v", req, ok)
	}
	if len(rec.posted[liveDecisionPR]) != 1 {
		t.Fatalf("posted = %v, want one reply", rec.posted[liveDecisionPR])
	}
	reply := rec.posted[liveDecisionPR][0]
	for _, want := range []string{unparkReplyMarkerPrefix + "4242 -->", "Decision recorded by @clubanderson", "head `e653b6d`", "changes requested", "pushes a new head"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
	if len(rec.added[liveDecisionPR]) != 0 || len(rec.removed[liveDecisionPR]) != 0 {
		t.Fatalf("labels touched on a PR outside the stage engine: added %v removed %v", rec.added[liveDecisionPR], rec.removed[liveDecisionPR])
	}
	// The stage moves in the same breath as the reply, through the engine.
	facts := prDecisionFacts(t, c)
	if len(facts) != 1 || facts[0] != (stageengine.PRDecisionFact{Head: liveDecisionHead, By: "clubanderson", Instruction: true, SelfAuthored: true}) {
		t.Fatalf("stage facts = %+v, want one PRDecided with the instruction", facts)
	}

	// The planner kicks the fix lane from the record, with the text verbatim.
	pr := review.PullRequest{Repo: "org/repo", Number: liveDecisionPR, Author: "hive[bot]", HeadSHA: liveDecisionHead, Labels: []string{PRNeedsDecisionStageLabel, "author/hive"}}
	pr.HumanChangeRequest = review.HumanChangeRequestFor(store, pr.Repo, pr.Number, pr.HeadSHA)
	plan := review.PlanDispatch([]review.PullRequest{pr}, review.Artifact{}, review.DispatchState{}, review.DispatchOptions{
		RequireApproval: true, FanOut: true, ProjectOrg: "org",
		Agents: []review.AgentCapability{{Name: review.DefaultFixerAgent, Enabled: true, UsesKick: true, Role: "reviewer"}},
	})
	if len(plan.FixKicks) != 1 || !strings.Contains(plan.FixKicks[0].Message, "> "+liveDecisionText) {
		t.Fatalf("fix kicks = %+v, want one kick quoting the decision", plan.FixKicks)
	}

	// Second sweep: the reply marker makes the comment answered; nothing repeats.
	issues[0].Comments = append(issues[0].Comments, unparkWireComment{ID: 9001, Body: reply, User: wireUser{Login: "hive[bot]"}, CreatedAt: unparkNow(), UpdatedAt: unparkNow()})
	c2, rec2 := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
	result2, err := c2.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")})
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(result2.PRDecisions) != 0 || len(rec2.posted[liveDecisionPR]) != 0 || store.Len() != 1 || len(prDecisionFacts(t, c2)) != 0 {
		t.Fatalf("second sweep repeated: %+v posted=%v store=%d", result2, rec2.posted[liveDecisionPR], store.Len())
	}
}

// hivecommons/hive#14679, the live case on #14628: `/hive decision approve`
// followed by numbered instructions is a change request carrying the whole
// text — recorded verbatim, stage 070 submitted, fix kick quoting every
// line — while a bare `/hive decision approve` is an approval: recorded as
// such, no fix kick, the lanes release requires_human from it.
func TestSweepIssueUnparkCommandsDecisionApproveModes(t *testing.T) {
	const instructions = "/hive decision approve\n\nBefore merging:\n\n1. Change `Closes #14605` to `Refs #14605`.\n2. Keep #14605 open.\n3. Merge on green."
	cases := []struct {
		name         string
		body         string
		wantMode     string
		wantKind     string
		wantFixKick  bool
		wantReply    []string
		wantInstruct bool
	}{
		{
			name: "approve with instructions → changes_requested", body: instructions,
			wantMode: "changes_requested", wantKind: review.HumanChangeRequestKindChanges, wantFixKick: true, wantInstruct: true,
			wantReply: []string{"Decision recorded by @clubanderson", "changes requested", prDecisionHeadMarkerLine(liveDecisionHead, prDecisionChangesRequested)},
		},
		{
			name: "bare approve → approve", body: "/hive decision approve",
			wantMode: "approve", wantKind: review.HumanChangeRequestKindApprove,
			wantReply: []string{"Approval recorded by @clubanderson", "merge path", prDecisionHeadMarkerLine(liveDecisionHead, prDecisionApprove)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", tc.body))}
			c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
			store := review.NewHumanChangeRequests("")
			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(result.PRDecisions) != 1 || result.PRDecisions[0].Mode != tc.wantMode {
				t.Fatalf("decisions = %+v, want one in mode %s", result.PRDecisions, tc.wantMode)
			}
			req, ok := store.For("org/repo", liveDecisionPR, liveDecisionHead)
			if !ok || req.Kind != tc.wantKind || req.Actor != "clubanderson" {
				t.Fatalf("recorded = %+v, %v", req, ok)
			}
			if tc.wantInstruct {
				for _, want := range []string{"1. Change `Closes #14605` to `Refs #14605`.", "2. Keep #14605 open.", "3. Merge on green."} {
					if !strings.Contains(req.Body, want) || !strings.Contains(result.PRDecisions[0].Decision, want) {
						t.Fatalf("instruction %q lost: body=%q", want, req.Body)
					}
				}
			}
			if len(rec.posted[liveDecisionPR]) != 1 {
				t.Fatalf("posted = %v", rec.posted[liveDecisionPR])
			}
			for _, want := range tc.wantReply {
				if !strings.Contains(rec.posted[liveDecisionPR][0], want) {
					t.Fatalf("reply missing %q:\n%s", want, rec.posted[liveDecisionPR][0])
				}
			}
			facts := prDecisionFacts(t, c)
			if len(facts) != 1 || facts[0].Instruction != tc.wantInstruct || facts[0].Head != liveDecisionHead || !facts[0].SelfAuthored {
				t.Fatalf("stage facts = %+v", facts)
			}
			pr := review.PullRequest{Repo: "org/repo", Number: liveDecisionPR, Author: "hive[bot]", HeadSHA: liveDecisionHead, Labels: []string{PRNeedsDecisionStageLabel, "author/hive"}}
			pr.HumanChangeRequest = review.HumanChangeRequestFor(store, pr.Repo, pr.Number, pr.HeadSHA)
			plan := review.PlanDispatch([]review.PullRequest{pr}, review.Artifact{}, review.DispatchState{}, review.DispatchOptions{
				RequireApproval: true, FanOut: true, ProjectOrg: "org",
				Agents: []review.AgentCapability{{Name: review.DefaultFixerAgent, Enabled: true, UsesKick: true, Role: "reviewer"}},
			})
			if got := len(plan.FixKicks) == 1; got != tc.wantFixKick {
				t.Fatalf("fix kicks = %+v, want kick=%v", plan.FixKicks, tc.wantFixKick)
			}
			if tc.wantFixKick && !strings.Contains(plan.FixKicks[0].Message, "3. Merge on green.") {
				t.Fatalf("fix kick does not quote the instructions:\n%s", plan.FixKicks[0].Message)
			}
		})
	}
}

// hivecommons/hive#14679: the decision on #14628 was acknowledged by one pod
// and the stage never moved before the next pod came up. An answered
// decision (reply marker present) whose record is missing for the live
// head is rebuilt from the thread — no second reply, the same stage fact —
// and one answered for an older head is left alone.
func TestSweepIssueUnparkCommandsReconcilesAnsweredDecisionAfterRestart(t *testing.T) {
	const body = "/hive decision approve\n\n1. Change `Closes #14605` to `Refs #14605`.\n2. Keep #14605 open."
	replyFor := func(head string) unparkWireComment {
		return unparkWireComment{ID: 9001, Body: unparkReplyMarkerPrefix + "4242 -->\n" + renderPRDecisionRecordedReply("clubanderson", head), User: wireUser{Login: "hive[bot]"}, CreatedAt: unparkNow(), UpdatedAt: unparkNow()}
	}
	t.Run("answered for the live head → re-recorded, no reply", func(t *testing.T) {
		issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", body), replyFor(liveDecisionHead))}
		c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
		store := review.NewHumanChangeRequests("")
		result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")})
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if len(rec.posted[liveDecisionPR]) != 0 || len(result.PRDecisions) != 0 {
			t.Fatalf("reconciliation replied again: posted=%v decisions=%+v", rec.posted[liveDecisionPR], result.PRDecisions)
		}
		req, ok := store.For("org/repo", liveDecisionPR, liveDecisionHead)
		if !ok || req.SourceID != 4242 || req.Kind != review.HumanChangeRequestKindChanges || !strings.Contains(req.Body, "2. Keep #14605 open.") {
			t.Fatalf("re-recorded = %+v, %v", req, ok)
		}
		facts := prDecisionFacts(t, c)
		if len(facts) != 1 || !facts[0].Instruction || facts[0].Head != liveDecisionHead {
			t.Fatalf("stage facts = %+v", facts)
		}
		// Idempotent: the record now stands, so the next sweep is a no-op.
		c2, rec2 := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
		if _, err := c2.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")}); err != nil {
			t.Fatal(err)
		}
		if len(rec2.posted[liveDecisionPR]) != 0 || len(prDecisionFacts(t, c2)) != 0 || store.Len() != 1 {
			t.Fatalf("second reconciliation acted: posted=%v store=%d", rec2.posted[liveDecisionPR], store.Len())
		}
	})
	t.Run("record kept only the first line → repaired with the whole text", func(t *testing.T) {
		issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", body), replyFor(liveDecisionHead))}
		c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
		store := review.NewHumanChangeRequests("")
		if err := store.Record(review.HumanChangeRequest{Repo: "org/repo", Number: liveDecisionPR, HeadSHA: liveDecisionHead, Source: review.HumanChangeRequestSourceDecision, SourceID: 4242, Actor: "clubanderson", Body: "approve"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")}); err != nil {
			t.Fatal(err)
		}
		req, ok := store.For("org/repo", liveDecisionPR, liveDecisionHead)
		if !ok || store.Len() != 1 || !strings.Contains(req.Body, "1. Change `Closes #14605` to `Refs #14605`.") || len(rec.posted[liveDecisionPR]) != 0 {
			t.Fatalf("repaired = %+v (len %d), posted=%v", req, store.Len(), rec.posted[liveDecisionPR])
		}
	})
	t.Run("pre-marker reply matched by short head", func(t *testing.T) {
		old := replyFor(liveDecisionHead)
		old.Body = unparkReplyMarkerPrefix + "4242 -->\nDecision recorded by @clubanderson for head `" + liveDecisionHead[:7] + "`. The hive is treating it as a *changes requested* review."
		issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", body), old)}
		c, _ := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
		store := review.NewHumanChangeRequests("")
		if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")}); err != nil {
			t.Fatal(err)
		}
		if _, ok := store.For("org/repo", liveDecisionPR, liveDecisionHead); !ok || len(prDecisionFacts(t, c)) != 1 {
			t.Fatalf("pre-marker reply not reconciled: store=%d", store.Len())
		}
	})
	t.Run("answered for an older head → ignored", func(t *testing.T) {
		issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", body), replyFor("0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0"))}
		c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
		store := review.NewHumanChangeRequests("")
		if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")}); err != nil {
			t.Fatal(err)
		}
		if store.Len() != 0 || len(rec.posted[liveDecisionPR]) != 0 || len(prDecisionFacts(t, c)) != 0 {
			t.Fatalf("stale decision acted on: store=%d posted=%v", store.Len(), rec.posted[liveDecisionPR])
		}
	})
}

func TestParsePRDecisionHeadMarker(t *testing.T) {
	head := liveDecisionHead
	cases := []struct {
		reply    string
		wantMode prDecisionMode
		wantOK   bool
	}{
		{renderPRDecisionRecordedReply("ann", head), prDecisionChangesRequested, true},
		{renderPRDecisionApprovedReply("ann", head), prDecisionApprove, true},
		{renderPRDecisionRecordedReply("ann", "0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0"), "", false},
		{"Decision recorded by @ann for head `" + head[:7] + "`.", prDecisionChangesRequested, true},
		{"Decision recorded by @ann for head `0ld0ld0`.", "", false},
		{renderPRHelpReply(true, head), "", false},
	}
	for _, tc := range cases {
		mode, ok := parsePRDecisionHeadMarker(tc.reply, head)
		if ok != tc.wantOK || mode != tc.wantMode {
			t.Errorf("parsePRDecisionHeadMarker(%q) = %s, %v; want %s, %v", tc.reply, mode, ok, tc.wantMode, tc.wantOK)
		}
	}
}

func TestSweepIssueUnparkCommandsDecisionOnHumanPRExplainsReviews(t *testing.T) {
	issues := []unparkWireIssue{parkedPR(31, "alice", []string{PRNeedsDecisionStageLabel}, humanComment(50, "maintainer", "/hive decision ship it with the flag"))}
	c, rec := newUnparkPRClient(t, issues, map[string]string{"maintainer": "write"})
	store := review.NewHumanChangeRequests("")

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("maintainer")})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.PRDecisions) != 0 || result.Replies != 1 || store.Len() != 0 {
		t.Fatalf("result = %+v store=%d, want one explanatory reply and no record", result, store.Len())
	}
	reply := strings.Join(rec.posted[31], "\n")
	for _, want := range []string{"did not open this PR", "**Approve** head `e653b6d`", "**changes requested** review", maintainerCommandsDocURL} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
	if len(rec.added[31]) != 0 || len(rec.removed[31]) != 0 {
		t.Fatalf("labels touched on a PR: added %v removed %v", rec.added[31], rec.removed[31])
	}
}

func TestSweepIssueUnparkCommandsPRIgnoresUntrustedAndUnparked(t *testing.T) {
	tests := []struct {
		name        string
		issue       unparkWireIssue
		permissions map[string]string
		trusted     func(string) bool
		wantSeen    int
	}{
		{
			name:        "commenter without repo write is ignored",
			issue:       hiveParkedPR(humanComment(1, "mallory", "/hive decision rm -rf")),
			permissions: map[string]string{"mallory": "read"},
			trusted:     trustedMergerSet("mallory"),
			wantSeen:    1,
		},
		{
			name:        "commenter with write but outside the merger trust set is ignored",
			issue:       hiveParkedPR(humanComment(1, "dave", "/hive decision do it")),
			permissions: map[string]string{"dave": "write"},
			trusted:     trustedMergerSet("clubanderson"),
			wantSeen:    1,
		},
		{
			name:        "no merger trust configured means no PR decision",
			issue:       hiveParkedPR(humanComment(1, "clubanderson", "/hive decision do it")),
			permissions: map[string]string{"clubanderson": "admin"},
			wantSeen:    1,
		},
		{
			name:        "bot commenter is ignored",
			issue:       hiveParkedPR(humanComment(1, "hive[bot]", "/hive decision do it")),
			permissions: map[string]string{"hive[bot]": "admin"},
			trusted:     trustedMergerSet("hive[bot]"),
			wantSeen:    1,
		},
		{
			name:        "prose on a PR gets no reply",
			issue:       hiveParkedPR(humanComment(1, "clubanderson", "yes, go ahead")),
			permissions: map[string]string{"clubanderson": "admin"},
			trusted:     trustedMergerSet("clubanderson"),
			wantSeen:    1,
		},
		{
			name:        "PR not at 085 is not read",
			issue:       parkedPR(5, "hive[bot]", []string{"stage/070-fixing"}, humanComment(1, "clubanderson", "/hive decision do it")),
			permissions: map[string]string{"clubanderson": "admin"},
			trusted:     trustedMergerSet("clubanderson"),
		},
		{
			name: "edited command is not a new instruction",
			issue: hiveParkedPR(unparkWireComment{ID: 1, Body: "/hive decision do it", User: wireUser{Login: "clubanderson"},
				CreatedAt: unparkNow(), UpdatedAt: unparkSoon(), HTMLURL: "https://example.test/c/1"}),
			permissions: map[string]string{"clubanderson": "admin"},
			trusted:     trustedMergerSet("clubanderson"),
			wantSeen:    1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := newUnparkPRClient(t, []unparkWireIssue{tt.issue}, tt.permissions)
			store := review.NewHumanChangeRequests("")
			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: tt.trusted})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if result.Seen != tt.wantSeen || len(result.PRDecisions) != 0 || result.Replies != 0 || store.Len() != 0 {
				t.Fatalf("result = %+v store=%d", result, store.Len())
			}
			if n := tt.issue.Number; len(rec.posted[n]) != 0 {
				t.Fatalf("posted = %v, want silence", rec.posted[n])
			}
		})
	}
}

func TestSweepIssueUnparkCommandsPRApproveAndHelpPointAtReviews(t *testing.T) {
	tests := []struct {
		name    string
		issue   unparkWireIssue
		want    []string
		notWant string
	}{
		{
			name:  "approve on a PR points at the Approve review",
			issue: hiveParkedPR(humanComment(1, "clubanderson", "/hive approve")),
			want:  []string{"**Approve** head `e653b6d`", "`/hive approve` is for parked issues"},
		},
		{
			name:  "help on a hive PR lists both levers",
			issue: hiveParkedPR(humanComment(1, "clubanderson", "/hive help")),
			want:  []string{PRNeedsDecisionStageLabel, "**Approve** head `e653b6d`", "`/hive decision <what to change>`", "the hive pushes the fix"},
		},
		{
			name:    "help on a human PR does not promise a push",
			issue:   parkedPR(8, "alice", []string{PRNeedsDecisionStageLabel}, humanComment(1, "clubanderson", "/hive help")),
			want:    []string{"does not push to a PR it did not open"},
			notWant: "/hive decision <what to change>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := newUnparkPRClient(t, []unparkWireIssue{tt.issue}, map[string]string{"clubanderson": "admin"})
			store := review.NewHumanChangeRequests("")
			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if result.Replies != 1 || len(result.PRDecisions) != 0 || store.Len() != 0 {
				t.Fatalf("result = %+v store=%d", result, store.Len())
			}
			reply := strings.Join(rec.posted[tt.issue.Number], "\n")
			for _, want := range tt.want {
				if !strings.Contains(reply, want) {
					t.Fatalf("reply missing %q:\n%s", want, reply)
				}
			}
			if tt.notWant != "" && strings.Contains(reply, tt.notWant) {
				t.Fatalf("reply should not contain %q:\n%s", tt.notWant, reply)
			}
		})
	}
}

func TestSweepIssueUnparkCommandsDecisionOnHivePRWithoutStoreIsSkippedLoudly(t *testing.T) {
	issues := []unparkWireIssue{hiveParkedPR(humanComment(4242, "clubanderson", "/hive decision "+liveDecisionText))}
	c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{TrustedMerger: trustedMergerSet("clubanderson")})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// No store: the sweep must not claim the fix is coming.
	if result.Skipped != 1 || len(result.PRDecisions) != 0 || len(rec.posted[liveDecisionPR]) != 0 {
		t.Fatalf("result = %+v posted=%v", result, rec.posted[liveDecisionPR])
	}
}

// hivecommons/hive#14822 regression: a `/hive decision …` the hive itself
// posted as the owner's delegate (the delegated-decision marker) is never
// read as a maintainer's change request, even when it lands under a human
// trusted merger's login (the dashboard posts through the operator's
// token). Reading it as one made the delegate's decision block the PR it
// meant to advance (`contributor-lane:changes-requested` every sweep).
func TestSweepIssueUnparkCommandsIgnoresHiveSelfDecision(t *testing.T) {
	body := delegatedecision.CommentBody(delegatedecision.Decision{
		Verdict: delegatedecision.VerdictAdoptSupersede, Rule: "contributor-offline",
		Text: "supersede now: adopt #12968 in supersede mode", Rationale: "offline",
	}, "hive-a", "clubanderson")
	if !strings.HasPrefix(body, "/hive decision ") {
		t.Fatalf("fixture must look like a maintainer decision: %q", body)
	}
	issues := []unparkWireIssue{hiveParkedPR(humanComment(4300, "clubanderson", body))}
	c, rec := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
	store := review.NewHumanChangeRequests("")
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		HumanChangeRequests: store,
		TrustedMerger:       trustedMergerSet("clubanderson"),
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.PRDecisions) != 0 || store.Len() != 0 || len(rec.posted[liveDecisionPR]) != 0 || len(prDecisionFacts(t, c)) != 0 {
		t.Fatalf("hive's own decision was read as a change request: %+v store=%d posted=%v", result, store.Len(), rec.posted[liveDecisionPR])
	}

	// The reconcile path (reply present, store empty after a restart) must
	// not rebuild a change request from it either.
	reply := unparkReplyMarkerPrefix + "4300 -->\n" + renderPRDecisionRecordedReply("clubanderson", liveDecisionHead)
	issues[0].Comments = append(issues[0].Comments, unparkWireComment{ID: 9300, Body: reply, User: wireUser{Login: "hive[bot]"}, CreatedAt: unparkNow(), UpdatedAt: unparkNow()})
	c2, _ := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
	store2 := review.NewHumanChangeRequests("")
	if _, err := c2.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store2, TrustedMerger: trustedMergerSet("clubanderson")}); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if store2.Len() != 0 {
		t.Fatalf("reconcile rebuilt a change request from the hive's own decision: %+v", store2)
	}
	// A real maintainer decision on the same thread is still honoured.
	issues[0].Comments = append(issues[0].Comments, humanComment(4301, "clubanderson", "/hive decision "+liveDecisionText))
	c3, _ := newUnparkPRClient(t, issues, map[string]string{"clubanderson": "admin"})
	store3 := review.NewHumanChangeRequests("")
	result3, err := c3.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store3, TrustedMerger: trustedMergerSet("clubanderson")})
	if err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	if len(result3.PRDecisions) != 1 || result3.PRDecisions[0].Decision != liveDecisionText {
		t.Fatalf("real maintainer decision not recorded: %+v", result3)
	}
}

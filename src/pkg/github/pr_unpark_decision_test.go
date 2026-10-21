package github

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
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
	return c, rec
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
		t.Fatalf("labels touched on a PR: added %v removed %v", rec.added[liveDecisionPR], rec.removed[liveDecisionPR])
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
	if len(result2.PRDecisions) != 0 || len(rec2.posted[liveDecisionPR]) != 0 || store.Len() != 1 {
		t.Fatalf("second sweep repeated: %+v posted=%v store=%d", result2, rec2.posted[liveDecisionPR], store.Len())
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
			name:    "approve on a PR explains the release it records",
			issue:   hiveParkedPR(humanComment(1, "clubanderson", "/hive approve")),
			want:    []string{"`/hive approve` reply posted after the swarm's `requires_human` verdict releases that hold on head `e653b6d`", "GitHub **Approve** review", "does not bypass CI, soak, `hold`-class labels", PRNeedsDecisionStageLabel, "No `needs-human` label blocks this PR."},
			notWant: "is for parked issues",
		},
		{
			name:  "help on a hive PR lists both levers",
			issue: hiveParkedPR(humanComment(1, "clubanderson", "/hive help")),
			want:  []string{PRNeedsDecisionStageLabel, "**Approve** head `e653b6d`", "reply `/hive approve` after the swarm's `requires_human` verdict", "`/hive decision <what to change>`", "the hive pushes the fix"},
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

// hivecommons/hive#13309, seen on Danathar/akmods#158: the supersession
// sweep put needs-human on a hive-authored PR, the adjudicator ruled
// DE-ESCALATE and handed off (reviewer-handoff), and no /hive command could
// lift the hive's own label. A trusted merger's `/hive approve` now lifts a
// needs-human Hive added and applies the hand-off transition; a person's
// needs-human, or one the timeline cannot attribute, stays untouched.
func TestSweepIssueUnparkCommandsPRApproveReleasesHiveNeedsHuman(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	tests := []struct {
		name        string
		author      string
		labels      []string
		timeline    []unparkWireTimelineEvent
		wantAdded   []string
		wantRemoved []string
		want        []string
	}{
		{
			name:        "hive-added needs-human lifts with the hand-off transition",
			author:      "hive[bot]",
			labels:      []string{PRNeedsDecisionStageLabel, "author/hive", "needs-human", "reviewer-handoff"},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, "hive[bot]", "needs-human", base)},
			wantAdded:   []string{"reviewer-passed"},
			wantRemoved: []string{"reviewer-handoff", "needs-human"},
			want:        []string{"`needs-human` cleared: Hive added it", "`reviewer-passed` applied", "`reviewer-handoff` and `needs-human` removed", "No `needs-human` label still blocks this PR", "green CI and soak"},
		},
		{
			name:        "reviewer-passed already present is not re-added",
			author:      "hive[bot]",
			labels:      []string{PRNeedsDecisionStageLabel, "author/hive", "needs-human", "reviewer-passed"},
			timeline:    []unparkWireTimelineEvent{labeledAt(1, "hive[bot]", "needs-human", base)},
			wantRemoved: []string{"needs-human"},
			want:        []string{"`needs-human` cleared", "`needs-human` removed"},
		},
		{
			name:     "a person's needs-human stays",
			author:   "hive[bot]",
			labels:   []string{PRNeedsDecisionStageLabel, "author/hive", "needs-human", "reviewer-handoff"},
			timeline: []unparkWireTimelineEvent{labeledAt(1, "hive[bot]", "needs-human", base), labeledAt(2, "owner", "needs-human", base.Add(time.Minute))},
			want:     []string{"`needs-human` still blocks this PR (added by @owner)", "never lifts a person's hold"},
		},
		{
			name:   "an unattributed needs-human stays",
			author: "hive[bot]",
			labels: []string{PRNeedsDecisionStageLabel, "author/hive", "needs-human"},
			want:   []string{"`needs-human` still blocks this PR (Hive could not read who added it)"},
		},
		{
			name:     "a PR the hive did not open keeps needs-human",
			author:   "alice",
			labels:   []string{PRNeedsDecisionStageLabel, "needs-human"},
			timeline: []unparkWireTimelineEvent{labeledAt(1, "hive[bot]", "needs-human", base)},
			want:     []string{"`needs-human` still blocks this PR: `/hive approve` lifts it only on a PR the hive opened"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := parkedPR(158, tt.author, tt.labels, humanComment(1, "clubanderson", "/hive approve"))
			c, rec := newUnparkPRClient(t, []unparkWireIssue{issue}, map[string]string{"clubanderson": "admin"})
			rec.timelines = map[int][]unparkWireTimelineEvent{158: tt.timeline}
			store := review.NewHumanChangeRequests("")
			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: store, TrustedMerger: trustedMergerSet("clubanderson")})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if result.Replies != 1 || len(result.PRDecisions) != 0 || store.Len() != 0 {
				t.Fatalf("result = %+v store=%d", result, store.Len())
			}
			if strings.Join(rec.added[158], ",") != strings.Join(tt.wantAdded, ",") {
				t.Fatalf("added = %v, want %v", rec.added[158], tt.wantAdded)
			}
			if strings.Join(rec.removed[158], ",") != strings.Join(tt.wantRemoved, ",") {
				t.Fatalf("removed = %v, want %v", rec.removed[158], tt.wantRemoved)
			}
			reply := strings.Join(rec.posted[158], "\n")
			for _, want := range append(tt.want, "releases that hold on head `e653b6d`") {
				if !strings.Contains(reply, want) {
					t.Fatalf("reply missing %q:\n%s", want, reply)
				}
			}
		})
	}
}

// An untrusted commenter's `/hive approve` never touches a PR's labels, even
// a needs-human Hive added.
func TestSweepIssueUnparkCommandsPRApproveUntrustedLeavesNeedsHuman(t *testing.T) {
	issue := parkedPR(158, "hive[bot]", []string{PRNeedsDecisionStageLabel, "author/hive", "needs-human"}, humanComment(1, "stranger", "/hive approve"))
	c, rec := newUnparkPRClient(t, []unparkWireIssue{issue}, map[string]string{"stranger": "write"})
	rec.timelines = map[int][]unparkWireTimelineEvent{158: {labeledAt(1, "hive[bot]", "needs-human", time.Now().Add(-time.Hour))}}
	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{HumanChangeRequests: review.NewHumanChangeRequests(""), TrustedMerger: trustedMergerSet("clubanderson")}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(rec.added[158]) != 0 || len(rec.removed[158]) != 0 || len(rec.posted[158]) != 0 {
		t.Fatalf("untrusted approve acted: added %v removed %v posted %v", rec.added[158], rec.removed[158], rec.posted[158])
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

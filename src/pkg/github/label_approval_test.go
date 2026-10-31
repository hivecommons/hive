package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Label-driven approval (hivecommons/hive#11313). Every behaviour here is
// gated on project.repo_policies[].label_driven plus an approval policy; the
// "not opted in" tests pin that a repo without either sees no change.

const (
	laHive     = "hive[bot]"
	laTriager  = "triager"
	laWriter   = "writer"
	laReader   = "reader"
	laOrgOnly  = "orgonly"
	laProwBot  = "github-actions[bot]"
	laOtherBot = "lifecycle[bot]"
)

var laBase = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func laPolicy() LabelApprovalPolicy {
	return LabelApprovalPolicy{
		Labels:          []string{"triage/accepted"},
		TrustOrgMembers: true,
		TrustedBots:     []string{laProwBot},
	}
}

func laPolicyFn(p LabelApprovalPolicy, ok bool) func(string) (LabelApprovalPolicy, bool) {
	return func(string) (LabelApprovalPolicy, bool) { return p, ok }
}

// laServer builds the unpark fake with #11313's extra endpoints populated.
func laServer(t *testing.T, issues []unparkWireIssue) (*Client, *unparkServer) {
	t.Helper()
	server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{laWriter: "write", laTriager: "read", laReader: "read", laOrgOnly: "read"})
	rec.roles = map[string]string{laTriager: "triage", laWriter: "write", laReader: "read", laOrgOnly: "read"}
	rec.orgMembers = map[string]bool{laOrgOnly: true}
	rec.timelines = map[int][]unparkWireTimelineEvent{}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(laHive)
	return c, rec
}

func laIssue(number int, labels ...string) unparkWireIssue {
	issue := parkedIssue(number)
	issue.Labels = nil
	for _, l := range labels {
		issue.Labels = append(issue.Labels, wireLabel{Name: l})
	}
	return issue
}

func laSweep(t *testing.T, c *Client, labelDriven bool, policy func(string) (LabelApprovalPolicy, bool)) *IssueUnparkSweepResult {
	t.Helper()
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		LabelDriven:   func(string) bool { return labelDriven },
		LabelApproval: policy,
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return result
}

func TestLabelApplierTrustedTiers(t *testing.T) {
	cases := []struct {
		name       string
		actor      *gh.User
		policy     LabelApprovalPolicy
		wantTrust  bool
		wantReason string
	}{
		{"triage role_name", &gh.User{Login: gh.Ptr(laTriager)}, laPolicy(), true, "triage collaborator"},
		{"write role_name", &gh.User{Login: gh.Ptr(laWriter)}, laPolicy(), true, "write collaborator"},
		{"read-only collaborator outside the org", &gh.User{Login: gh.Ptr(laReader)}, laPolicy(), false, ""},
		{"org member with read", &gh.User{Login: gh.Ptr(laOrgOnly)}, laPolicy(), true, "member of org"},
		{"org member when org trust is off", &gh.User{Login: gh.Ptr(laOrgOnly)}, LabelApprovalPolicy{Labels: []string{"triage/accepted"}}, false, ""},
		{"allowlisted bot", &gh.User{Login: gh.Ptr(laProwBot), Type: gh.Ptr("Bot")}, laPolicy(), true, "trusted bot"},
		{"other bot", &gh.User{Login: gh.Ptr(laOtherBot), Type: gh.Ptr("Bot")}, laPolicy(), false, ""},
		{"the hive itself", &gh.User{Login: gh.Ptr(laHive), Type: gh.Ptr("Bot")}, laPolicy(), false, ""},
		{"no login", &gh.User{}, laPolicy(), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := laServer(t, nil)
			reason, trusted, err := c.labelApplierTrusted(context.Background(), "org", "repo", tc.actor, tc.policy)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if trusted != tc.wantTrust || reason != tc.wantReason {
				t.Fatalf("trusted=%v reason=%q, want %v %q", trusted, reason, tc.wantTrust, tc.wantReason)
			}
		})
	}
}

func TestLabelApplierTrustedFailsClosedOnLookupError(t *testing.T) {
	c, rec := laServer(t, nil)
	rec.mu.Lock()
	rec.permissions = nil
	rec.mu.Unlock()
	// A 404 from the permission endpoint (not a collaborator) is not an
	// error; the org tier still runs. Simulate a hard failure by pointing the
	// client at a dead server instead.
	c.client.BaseURL.Host = "127.0.0.1:1"
	_, trusted, err := c.labelApplierTrusted(context.Background(), "org", "repo", &gh.User{Login: gh.Ptr(laTriager)}, laPolicy())
	if err == nil || trusted {
		t.Fatalf("a failed lookup must be an error and not trust, got trusted=%v err=%v", trusted, err)
	}
}

// A hive-parked issue accepted by a triager: Hive's park label goes,
// approved-direction arrives, one comment, one audit event.
func TestSweepLabelApprovalReleasesHiveParkedIssue(t *testing.T) {
	issue := laIssue(30, "needs-decision", "triage/accepted", "hold")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[30] = []unparkWireTimelineEvent{
		labeledAt(1, laHive, "needs-decision", laBase),
		labeledAt(2, laReader, "hold", laBase.Add(time.Minute)),
		labeledAt(3, laTriager, "triage/accepted", laBase.Add(2*time.Minute)),
	}
	var audited []IssueLabelApprovalEvent
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		LabelDriven:        func(string) bool { return true },
		LabelApproval:      laPolicyFn(laPolicy(), true),
		LabelApprovalAudit: func(e IssueLabelApprovalEvent) { audited = append(audited, e) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.LabelApproved) != 1 || len(audited) != 1 {
		t.Fatalf("expected one label approval, got %+v audited %+v", result.LabelApproved, audited)
	}
	ev := result.LabelApproved[0]
	if ev.Actor != laTriager || ev.Label != "triage/accepted" || !ev.Released || ev.Pending {
		t.Fatalf("unexpected event %+v", ev)
	}
	if got := strings.Join(rec.removed[30], ","); got != "needs-decision" {
		t.Fatalf("removed = %q, want only needs-decision", got)
	}
	if len(rec.added[30]) != 1 || rec.added[30][0] != HumanAckLabel {
		t.Fatalf("added = %v, want %s", rec.added[30], HumanAckLabel)
	}
	if len(rec.posted[30]) != 1 {
		t.Fatalf("expected one comment, got %v", rec.posted[30])
	}
	body := rec.posted[30][0]
	for _, want := range []string{labelApprovalMarker(3), "@" + laTriager, "triage collaborator", "`needs-decision`", "`hold` (added by @" + laReader + ")", "will not stop Hive"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "/") {
			t.Errorf("comment line starts with a command: %q", line)
		}
	}
	if strings.Contains(body, unparkNoticeMarker) {
		t.Error("no What-to-reply notice on a label-driven repo")
	}
}

// Not opted in: the same issue on a repo without label_driven is handled
// exactly as before — notice posted, no timeline read, labels untouched.
func TestSweepLabelApprovalNotLabelDrivenUnchanged(t *testing.T) {
	issue := laIssue(31, "needs-decision", "triage/accepted")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[31] = []unparkWireTimelineEvent{
		labeledAt(1, laHive, "needs-decision", laBase),
		labeledAt(2, laTriager, "triage/accepted", laBase.Add(time.Minute)),
	}
	policyAsked := false
	result := laSweep(t, c, false, func(string) (LabelApprovalPolicy, bool) { policyAsked = true; return laPolicy(), true })
	if policyAsked {
		t.Fatal("label approval policy must not be consulted for a repo that is not label-driven")
	}
	if rec.timelineReads != 0 {
		t.Fatalf("no timeline read expected on a repo that did not opt in, got %d", rec.timelineReads)
	}
	if len(result.LabelApproved) != 0 || len(rec.removed[31]) != 0 || len(rec.added[31]) != 0 {
		t.Fatalf("labels changed without opt-in: approved=%v removed=%v added=%v", result.LabelApproved, rec.removed[31], rec.added[31])
	}
	if len(rec.posted[31]) != 1 || !strings.Contains(rec.posted[31][0], unparkNoticeMarker) {
		t.Fatalf("expected only the legacy notice, got %v", rec.posted[31])
	}
}

// label_driven without a policy (the pre-#11313 wiring) is also unchanged.
func TestSweepLabelApprovalLabelDrivenWithoutPolicyUnchanged(t *testing.T) {
	issue := laIssue(32, "needs-decision", "triage/accepted")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[32] = []unparkWireTimelineEvent{labeledAt(1, laHive, "needs-decision", laBase), labeledAt(2, laTriager, "triage/accepted", laBase.Add(time.Minute))}
	result := laSweep(t, c, true, laPolicyFn(LabelApprovalPolicy{}, false))
	if rec.timelineReads != 0 || len(result.LabelApproved) != 0 || len(rec.removed[32]) != 0 || len(rec.added[32]) != 0 || len(rec.posted[32]) != 0 {
		t.Fatalf("label-driven repo without a policy must be silent: reads=%d result=%+v removed=%v added=%v posted=%v", rec.timelineReads, result.LabelApproved, rec.removed[32], rec.added[32], rec.posted[32])
	}
}

// A person's needs-human withholds approval: one pending comment, no labels.
// Once the hive's own marker is on the issue, nothing more is written.
func TestSweepLabelApprovalPersonParkLabelBlocks(t *testing.T) {
	issue := laIssue(33, "needs-decision", "needs-human", "triage/accepted")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[33] = []unparkWireTimelineEvent{
		labeledAt(1, laHive, "needs-decision", laBase),
		labeledAt(2, laWriter, "needs-human", laBase.Add(time.Minute)),
		labeledAt(3, laTriager, "triage/accepted", laBase.Add(2*time.Minute)),
	}
	result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
	if len(result.LabelApproved) != 1 || !result.LabelApproved[0].Pending {
		t.Fatalf("expected one pending event, got %+v", result.LabelApproved)
	}
	if len(rec.removed[33]) != 0 || len(rec.added[33]) != 0 {
		t.Fatalf("pending acceptance must change no labels: removed=%v added=%v", rec.removed[33], rec.added[33])
	}
	if len(rec.posted[33]) != 1 || !strings.Contains(rec.posted[33][0], "`needs-human` (added by @"+laWriter+")") || !strings.Contains(rec.posted[33][0], "withheld") {
		t.Fatalf("expected a pending comment naming the blocker, got %v", rec.posted[33])
	}

	// Second pass with the hive's own marker present: silence.
	marker := humanComment(500, laHive, rec.posted[33][0])
	issue.Comments = []unparkWireComment{marker}
	c2, rec2 := laServer(t, []unparkWireIssue{issue})
	rec2.timelines[33] = rec.timelines[33]
	result = laSweep(t, c2, true, laPolicyFn(laPolicy(), true))
	if len(result.LabelApproved) != 0 || len(rec2.posted[33]) != 0 {
		t.Fatalf("pending acceptance acted on twice: %+v %v", result.LabelApproved, rec2.posted[33])
	}

	// The same marker pasted by someone else does not count (A13).
	issue.Comments = []unparkWireComment{humanComment(501, laReader, rec.posted[33][0])}
	c3, rec3 := laServer(t, []unparkWireIssue{issue})
	rec3.timelines[33] = rec.timelines[33]
	result = laSweep(t, c3, true, laPolicyFn(laPolicy(), true))
	if len(result.LabelApproved) != 1 || len(rec3.posted[33]) != 1 {
		t.Fatalf("a forged marker must not suppress the acceptance: %+v %v", result.LabelApproved, rec3.posted[33])
	}
}

// R14: a park label the hive added after the acceptance stays; approval is
// still recorded.
func TestSweepLabelApprovalKeepsLaterHiveParkLabel(t *testing.T) {
	issue := laIssue(34, "needs-decision", "triage/accepted")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[34] = []unparkWireTimelineEvent{
		labeledAt(1, laTriager, "triage/accepted", laBase),
		labeledAt(2, laHive, "needs-decision", laBase.Add(time.Hour)),
	}
	result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
	if len(result.LabelApproved) != 1 || result.LabelApproved[0].Released {
		t.Fatalf("expected an unreleased approval, got %+v", result.LabelApproved)
	}
	if len(rec.removed[34]) != 0 {
		t.Fatalf("a later hive park label must stay, removed %v", rec.removed[34])
	}
	if len(rec.added[34]) != 1 || rec.added[34][0] != HumanAckLabel {
		t.Fatalf("approval must still be recorded, added %v", rec.added[34])
	}
	if len(rec.posted[34]) != 1 || !strings.Contains(rec.posted[34][0], "not yet offered") {
		t.Fatalf("expected a comment saying the issue stays parked, got %v", rec.posted[34])
	}
}

// Only the newest `labeled` event counts after an unlabel/relabel, and an
// acceptance by an untrusted account does nothing.
func TestSweepLabelApprovalUsesNewestLabeledEvent(t *testing.T) {
	cases := []struct {
		name  string
		last  string
		wantN int
	}{
		{"relabeled by a reader after a triager", laReader, 0},
		{"relabeled by a triager after a reader", laTriager, 1},
		{"added by the hive itself", laHive, 0},
		{"added by an unlisted bot", laOtherBot, 0},
		{"added by the trusted bot", laProwBot, 1},
		{"added by an org member", laOrgOnly, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := laIssue(35, "needs-decision", "triage/accepted")
			c, rec := laServer(t, []unparkWireIssue{issue})
			first := laTriager
			if tc.last == laTriager {
				first = laReader
			}
			rec.timelines[35] = []unparkWireTimelineEvent{
				labeledAt(1, laHive, "needs-decision", laBase),
				labeledAt(2, first, "triage/accepted", laBase.Add(time.Minute)),
				unlabeledAt(3, first, "triage/accepted", laBase.Add(2*time.Minute)),
				labeledAt(4, tc.last, "triage/accepted", laBase.Add(3*time.Minute)),
			}
			result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
			if len(result.LabelApproved) != tc.wantN || len(rec.added[35]) != tc.wantN {
				t.Fatalf("approvals=%d added=%v, want %d", len(result.LabelApproved), rec.added[35], tc.wantN)
			}
		})
	}
}

// R11 / R10: label gone, or timeline unreadable, means no action.
func TestSweepLabelApprovalFailsClosed(t *testing.T) {
	t.Run("label withdrawn", func(t *testing.T) {
		issue := laIssue(36, "needs-decision")
		c, rec := laServer(t, []unparkWireIssue{issue})
		rec.timelines[36] = []unparkWireTimelineEvent{labeledAt(1, laHive, "needs-decision", laBase), labeledAt(2, laTriager, "triage/accepted", laBase.Add(time.Minute))}
		result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
		if len(result.LabelApproved) != 0 || len(rec.added[36]) != 0 || len(rec.posted[36]) != 0 {
			t.Fatalf("withdrawn acceptance acted on: %+v %v %v", result.LabelApproved, rec.added[36], rec.posted[36])
		}
	})
	t.Run("timeline unreadable", func(t *testing.T) {
		issue := laIssue(37, "needs-decision", "triage/accepted")
		c, rec := laServer(t, []unparkWireIssue{issue})
		rec.timelineStatus = http.StatusInternalServerError
		result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
		if len(result.LabelApproved) != 0 || len(rec.added[37]) != 0 || len(rec.removed[37]) != 0 || len(rec.posted[37]) != 0 || result.Skipped != 1 {
			t.Fatalf("unreadable timeline must skip the issue: %+v added=%v removed=%v posted=%v skipped=%d", result.LabelApproved, rec.added[37], rec.removed[37], rec.posted[37], result.Skipped)
		}
	})
	t.Run("park label with no timeline event is not Hive's", func(t *testing.T) {
		issue := laIssue(38, "needs-decision", "triage/accepted")
		c, rec := laServer(t, []unparkWireIssue{issue})
		rec.timelines[38] = []unparkWireTimelineEvent{labeledAt(2, laTriager, "triage/accepted", laBase.Add(time.Minute))}
		result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
		if len(rec.removed[38]) != 0 || len(result.LabelApproved) != 1 || !result.LabelApproved[0].Pending {
			t.Fatalf("unattributed park label must block: removed=%v result=%+v", rec.removed[38], result.LabelApproved)
		}
	})
}

// R3 / R8: approval alone on an unparked issue adds the label, posts nothing;
// once the hive has added approved-direction after the acceptance it never
// re-adds it (A3).
func TestSweepLabelApprovalUnparkedIssueApprovalOnly(t *testing.T) {
	issue := laIssue(39, "triage/accepted", "bug")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[39] = []unparkWireTimelineEvent{labeledAt(1, laOrgOnly, "triage/accepted", laBase)}
	result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
	if len(result.LabelApproved) != 1 || len(rec.added[39]) != 1 || rec.added[39][0] != HumanAckLabel {
		t.Fatalf("expected approval only, got %+v added=%v", result.LabelApproved, rec.added[39])
	}
	if len(rec.posted[39]) != 0 {
		t.Fatalf("approval alone must post no comment, got %v", rec.posted[39])
	}

	// A person later removed approved-direction: the hive's own labeled
	// event after the acceptance is the once-only record.
	c2, rec2 := laServer(t, []unparkWireIssue{issue})
	rec2.timelines[39] = []unparkWireTimelineEvent{
		labeledAt(1, laOrgOnly, "triage/accepted", laBase),
		labeledAt(2, laHive, HumanAckLabel, laBase.Add(5*time.Minute)),
		unlabeledAt(3, laWriter, HumanAckLabel, laBase.Add(10*time.Minute)),
	}
	result = laSweep(t, c2, true, laPolicyFn(laPolicy(), true))
	if len(result.LabelApproved) != 0 || len(rec2.added[39]) != 0 {
		t.Fatalf("approval re-added after a person removed it: %+v %v", result.LabelApproved, rec2.added[39])
	}

	// Already approved: not even a candidate, no timeline read.
	approved := laIssue(40, "triage/accepted", HumanAckLabel)
	c3, rec3 := laServer(t, []unparkWireIssue{approved})
	result = laSweep(t, c3, true, laPolicyFn(laPolicy(), true))
	if result.Seen != 0 || rec3.timelineReads != 0 {
		t.Fatalf("approved unparked issue must not be examined: seen=%d reads=%d", result.Seen, rec3.timelineReads)
	}
}

// R5: on a label-driven repo with label approval, `/hive approve` removes
// only the park labels the hive added, and a triage-role commenter may use it.
func TestSweepLabelDrivenHiveApproveRemovesHiveLabelsOnly(t *testing.T) {
	for _, commenter := range []string{laTriager, laWriter} {
		t.Run(commenter, func(t *testing.T) {
			issue := laIssue(41, "needs-decision", "needs-human", "hold")
			issue.Comments = []unparkWireComment{humanComment(600, commenter, "/hive approve")}
			c, rec := laServer(t, []unparkWireIssue{issue})
			rec.timelines[41] = []unparkWireTimelineEvent{
				labeledAt(1, laHive, "needs-decision", laBase),
				labeledAt(2, laWriter, "needs-human", laBase.Add(time.Minute)),
			}
			result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
			if len(result.Unparked) != 1 || result.Unparked[0].Actor != commenter {
				t.Fatalf("expected un-park by %s, got %+v", commenter, result.Unparked)
			}
			if got := strings.Join(rec.removed[41], ","); got != "needs-decision" {
				t.Fatalf("removed = %q, want only the hive's needs-decision", got)
			}
			if len(rec.added[41]) != 1 || rec.added[41][0] != HumanAckLabel {
				t.Fatalf("added = %v", rec.added[41])
			}
			if len(rec.posted[41]) != 1 || !strings.Contains(rec.posted[41][0], "`needs-human` (added by @"+laWriter+")") {
				t.Fatalf("reply must name the label left in place, got %v", rec.posted[41])
			}
		})
	}
}

// The `/hive` permission check: triage is admitted only on label-driven repos
// with label approval; maintain (legacy "write") and custom roles with write
// permission stay admitted everywhere.
func TestCommenterMayUnparkRoleName(t *testing.T) {
	c, rec := laServer(t, nil)
	rec.permissions["maintainer"] = "write"
	rec.roles["maintainer"] = "maintain"
	rec.permissions["custom"] = "write"
	rec.roles["custom"] = "security-reviewer"
	cases := []struct {
		login       string
		allowTriage bool
		want        bool
	}{
		{laTriager, false, false},
		{laTriager, true, true},
		{laWriter, false, true},
		{"maintainer", false, true},
		{"custom", false, true},
		{laReader, true, false},
		{laOrgOnly, true, false},
	}
	for _, tc := range cases {
		got, err := c.commenterMayUnpark(context.Background(), "org", "repo", tc.login, tc.allowTriage)
		if err != nil {
			t.Fatalf("%s: %v", tc.login, err)
		}
		if got != tc.want {
			t.Errorf("commenterMayUnpark(%s, allowTriage=%v) = %v, want %v", tc.login, tc.allowTriage, got, tc.want)
		}
	}
}

// Triage stays ignored on a repo that did not opt in, end to end.
func TestSweepTriageIgnoredWithoutOptIn(t *testing.T) {
	issue := laIssue(42, "needs-decision")
	issue.Comments = []unparkWireComment{humanComment(700, laTriager, "/hive approve")}
	c, rec := laServer(t, []unparkWireIssue{issue})
	result := laSweep(t, c, false, laPolicyFn(laPolicy(), true))
	if len(result.Unparked) != 0 || len(rec.removed[42]) != 0 || len(rec.added[42]) != 0 {
		t.Fatalf("triage commenter must be ignored without opt-in: %+v removed=%v added=%v", result.Unparked, rec.removed[42], rec.added[42])
	}
}

// Once-only `/hive` reply markers count only when the hive wrote them (A13),
// unless the hive cannot name its own accounts at all.
func TestAnsweredUnparkCommentIDsRequiresHiveAuthor(t *testing.T) {
	marker := unparkReplyMarkerPrefix + "77 -->\nanswered"
	byHive := &gh.IssueComment{Body: gh.Ptr(marker), User: &gh.User{Login: gh.Ptr(laHive)}}
	byOther := &gh.IssueComment{Body: gh.Ptr(marker), User: &gh.User{Login: gh.Ptr(laReader)}}

	c, _ := laServer(t, nil)
	if got := c.answeredUnparkCommentIDs([]*gh.IssueComment{byOther}); got[77] {
		t.Fatal("a marker pasted by a non-hive account must not count")
	}
	if got := c.answeredUnparkCommentIDs([]*gh.IssueComment{byHive}); !got[77] {
		t.Fatal("the hive's own marker must count")
	}

	// A client with no logger and no resolvable identity (GET /user is denied)
	// must neither panic nor leave the sandbox, and keeps the legacy rule.
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	}))
	defer denied.Close()
	anonymous := NewClient("fake", "org", []string{"repo"}, nil, "")
	base, err := url.Parse(denied.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	anonymous.client.BaseURL = base
	if got := anonymous.answeredUnparkCommentIDs([]*gh.IssueComment{byOther}); !got[77] {
		t.Fatal("a hive with no known identity keeps the legacy any-author rule")
	}
}

// A forged `/hive` reply marker does not suppress the hive's answer.
func TestSweepForgedUnparkMarkerDoesNotSuppressReply(t *testing.T) {
	forged := humanComment(801, laReader, unparkReplyMarkerPrefix+"800 -->\nnothing to see")
	issue := parkedIssue(43, humanComment(800, laWriter, "/hive approve"), forged)
	c, rec := laServer(t, []unparkWireIssue{issue})
	result := laSweep(t, c, false, nil)
	if len(result.Unparked) != 1 || len(rec.posted[43]) == 0 {
		t.Fatalf("forged marker suppressed the answer: %+v posted=%v", result.Unparked, rec.posted[43])
	}
}

// PR self-authorization: a trusted acceptance label is sign-off on an opted-in
// repo, a person's needs-human withholds it (A29), and a repo without opt-in
// never reads the timeline.
func TestSelfAuthorizationLabelApproval(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	accepted := []unparkWireTimelineEvent{labeledAt(1, laTriager, "triage/accepted", laBase)}
	cases := []struct {
		name          string
		issue         *selfAuthIssue
		optIn         bool
		wantHeld      bool
		wantTimeline  bool
		trustOrgOnly  bool
		acceptedActor string
	}{
		{name: "trusted triager accepted", issue: &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Labels: []string{"triage/accepted"}, Timeline: accepted}, optIn: true, wantHeld: false, wantTimeline: true},
		{name: "same issue, repo not opted in", issue: &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Labels: []string{"triage/accepted"}, Timeline: accepted}, optIn: false, wantHeld: true, wantTimeline: false},
		{name: "a person's needs-human withholds sign-off", issue: &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Labels: []string{"triage/accepted", "needs-human"}, Timeline: append([]unparkWireTimelineEvent{labeledAt(0, "hanthor", "needs-human", laBase.Add(-time.Hour))}, accepted...)}, optIn: true, wantHeld: true, wantTimeline: true},
		{name: "accepted by a read-only account", issue: &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Labels: []string{"triage/accepted"}, Timeline: []unparkWireTimelineEvent{labeledAt(1, laReader, "triage/accepted", laBase)}}, optIn: true, wantHeld: true, wantTimeline: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: tc.issue}, roles: map[string]string{laTriager: "triage"}}
			c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
			c.SetAppBotLogin(botLogin)
			c.SetLabelApprovalPolicy(func(repo string) (LabelApprovalPolicy, bool) {
				if !tc.optIn {
					return LabelApprovalPolicy{}, false
				}
				return LabelApprovalPolicy{Labels: []string{"triage/accepted"}}, true
			})
			got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "title", "Closes #581", nil)
			if got.Held != tc.wantHeld {
				t.Fatalf("Held = %v, want %v (%s)", got.Held, tc.wantHeld, got.Reason)
			}
			srv.mu.Lock()
			reads := srv.timelineReads
			srv.mu.Unlock()
			if (reads > 0) != tc.wantTimeline {
				t.Fatalf("timeline reads = %d, want read=%v", reads, tc.wantTimeline)
			}
		})
	}
}

// Opened and relabeled issues wake evaluation and feed the un-park sweep;
// unrelated actions, PR-shaped payloads and unmanaged repos do not.
func TestHandleWebhookEventIssuesLabelMarksDirty(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    int
	}{
		{"labeled", "issues", `{"action":"labeled","issue":{"number":9},"label":{"name":"triage/accepted"},"repository":{"full_name":"org/repo"}}`, 1},
		{"unlabeled", "issues", `{"action":"unlabeled","issue":{"number":9},"repository":{"full_name":"org/repo"}}`, 1},
		{"opened", "issues", `{"action":"opened","issue":{"number":9},"repository":{"full_name":"org/repo"}}`, 1},
		{"edited", "issues", `{"action":"edited","issue":{"number":9},"repository":{"full_name":"org/repo"}}`, 0},
		{"opened PR via issues", "issues", `{"action":"opened","issue":{"number":9,"pull_request":{"url":"x"}},"repository":{"full_name":"org/repo"}}`, 0},
		{"opened unknown repo", "issues", `{"action":"opened","issue":{"number":9},"repository":{"full_name":"other/repo"}}`, 0},
		{"labeled PR via issues", "issues", `{"action":"labeled","issue":{"number":9,"pull_request":{"url":"x"}},"repository":{"full_name":"org/repo"}}`, 0},
		{"unknown repo", "issues", `{"action":"labeled","issue":{"number":9},"repository":{"full_name":"other/repo"}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setupWebhookCacheTest(t)
			res, err := c.HandleWebhookEvent(tc.event, []byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.DirtyIssues) != tc.want {
				t.Fatalf("dirty issues = %v, want %d", res.DirtyIssues, tc.want)
			}
			if got := TakeDirtyIssueCount(); got != tc.want {
				t.Fatalf("TakeDirtyIssueCount = %d, want %d", got, tc.want)
			}
			if got := TakeDirtyIssueCount(); got != 0 {
				t.Fatalf("second take must be empty, got %d", got)
			}
			if len(res.Invalidated) != 0 {
				t.Fatalf("issue events must not invalidate PR caches, got %v", res.Invalidated)
			}
		})
	}
}

// The label-driven path applies the same rule (hivecommons/hive#13075): a
// `needs-human` the hive itself added is answered by `/hive approve` and
// lifts with the hive's other park labels; `hold` is untouched.
func TestSweepLabelDrivenHiveApproveLiftsHiveNeedsHuman(t *testing.T) {
	issue := laIssue(42, "needs-decision", "needs-human", "hold")
	issue.Comments = []unparkWireComment{humanComment(601, laWriter, "/hive approve")}
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[42] = []unparkWireTimelineEvent{
		labeledAt(1, laHive, "needs-decision", laBase),
		labeledAt(2, laHive, "needs-human", laBase.Add(time.Minute)),
	}
	result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
	if len(result.Unparked) != 1 {
		t.Fatalf("expected un-park, got %+v", result.Unparked)
	}
	if got := strings.Join(rec.removed[42], ","); got != "needs-decision,needs-human" {
		t.Fatalf("removed = %q, want the hive's needs-human and needs-decision", got)
	}
	if len(rec.added[42]) != 1 || rec.added[42][0] != HumanAckLabel {
		t.Fatalf("added = %v", rec.added[42])
	}
	if len(rec.posted[42]) != 1 || !strings.Contains(rec.posted[42][0], "`needs-decision`, `needs-human` removed") || strings.Contains(rec.posted[42][0], "Left in place") {
		t.Fatalf("reply must list needs-human among the removed labels, got %v", rec.posted[42])
	}
}

// laSupersedePolicy lists the lifecycle bot whose park labels a later
// trusted acceptance supersedes (hivecommons/hive#13605).
func laSupersedePolicy() LabelApprovalPolicy {
	p := laPolicy()
	p.SupersedeParkActors = []string{laOtherBot}
	return p
}

// A listed lifecycle bot's needs-human added before a trusted acceptance is
// removed with the acceptance, the issue is released, and the comment says
// whose label went (hivecommons/hive#13605).
func TestSweepLabelApprovalSupersedesLifecycleBotParkLabel(t *testing.T) {
	issue := laIssue(43, "needs-human", "needs-decision", "triage/accepted")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[43] = []unparkWireTimelineEvent{
		labeledAt(1, laOtherBot, "needs-human", laBase),
		labeledAt(2, laHive, "needs-decision", laBase.Add(time.Minute)),
		labeledAt(3, laTriager, "triage/accepted", laBase.Add(2*time.Minute)),
	}
	result := laSweep(t, c, true, laPolicyFn(laSupersedePolicy(), true))
	if len(result.LabelApproved) != 1 {
		t.Fatalf("expected one label approval, got %+v", result.LabelApproved)
	}
	ev := result.LabelApproved[0]
	if ev.Pending || !ev.Released {
		t.Fatalf("expected a released approval, got %+v", ev)
	}
	if got := strings.Join(rec.removed[43], ","); got != "needs-decision,needs-human" {
		t.Fatalf("removed = %q, want needs-decision,needs-human", got)
	}
	if len(rec.added[43]) != 1 || rec.added[43][0] != HumanAckLabel {
		t.Fatalf("added = %v, want %s", rec.added[43], HumanAckLabel)
	}
	if len(rec.posted[43]) != 1 {
		t.Fatalf("expected one comment, got %v", rec.posted[43])
	}
	body := rec.posted[43][0]
	for _, want := range []string{"`needs-decision`", "`needs-human` (added by @" + laOtherBot + ")", "superseded by the acceptance", "now released"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "withheld") {
		t.Errorf("a superseded park label must not withhold approval:\n%s", body)
	}
}

// The four park-label situations around a listed lifecycle bot: only its
// label added before the acceptance is superseded. A person's label, an
// unlisted bot's label, and a listed bot's label added after the acceptance
// still block exactly as before, as does every label with the list empty.
func TestSweepLabelApprovalSupersedeParkActorsCases(t *testing.T) {
	cases := []struct {
		name     string
		actor    string
		parkedAt time.Time
		policy   LabelApprovalPolicy
		released bool
	}{
		{"listed bot before acceptance", laOtherBot, laBase, laSupersedePolicy(), true},
		{"listed bot after acceptance", laOtherBot, laBase.Add(time.Hour), laSupersedePolicy(), false},
		{"person", laWriter, laBase, laSupersedePolicy(), false},
		{"unlisted bot", laProwBot, laBase, laSupersedePolicy(), false},
		{"list empty", laOtherBot, laBase, laPolicy(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := laIssue(44, "needs-human", "triage/accepted")
			c, rec := laServer(t, []unparkWireIssue{issue})
			rec.timelines[44] = []unparkWireTimelineEvent{
				labeledAt(1, tc.actor, "needs-human", tc.parkedAt),
				labeledAt(2, laTriager, "triage/accepted", laBase.Add(time.Minute)),
			}
			result := laSweep(t, c, true, laPolicyFn(tc.policy, true))
			if len(result.LabelApproved) != 1 {
				t.Fatalf("expected one label approval event, got %+v", result.LabelApproved)
			}
			ev := result.LabelApproved[0]
			if tc.released {
				if ev.Pending || !ev.Released {
					t.Fatalf("expected a released approval, got %+v", ev)
				}
				if got := strings.Join(rec.removed[44], ","); got != "needs-human" {
					t.Fatalf("removed = %q, want needs-human", got)
				}
				if len(rec.added[44]) != 1 || rec.added[44][0] != HumanAckLabel {
					t.Fatalf("added = %v, want %s", rec.added[44], HumanAckLabel)
				}
				return
			}
			if !ev.Pending || ev.Released {
				t.Fatalf("expected a pending approval, got %+v", ev)
			}
			if len(rec.removed[44]) != 0 || len(rec.added[44]) != 0 {
				t.Fatalf("a blocking park label must change no labels: removed=%v added=%v", rec.removed[44], rec.added[44])
			}
			if len(rec.posted[44]) != 1 || !strings.Contains(rec.posted[44][0], "`needs-human` (added by @"+tc.actor+")") || !strings.Contains(rec.posted[44][0], "withheld") {
				t.Fatalf("expected the existing pending comment naming the blocker, got %v", rec.posted[44])
			}
		})
	}
}

// A hold label added by a listed lifecycle bot is never superseded.
func TestSweepLabelApprovalNeverSupersedesHold(t *testing.T) {
	issue := laIssue(45, "needs-decision", "hold", "triage/accepted")
	c, rec := laServer(t, []unparkWireIssue{issue})
	rec.timelines[45] = []unparkWireTimelineEvent{
		labeledAt(1, laHive, "needs-decision", laBase),
		labeledAt(2, laOtherBot, "hold", laBase.Add(time.Minute)),
		labeledAt(3, laTriager, "triage/accepted", laBase.Add(2*time.Minute)),
	}
	result := laSweep(t, c, true, laPolicyFn(laSupersedePolicy(), true))
	if len(result.LabelApproved) != 1 {
		t.Fatalf("expected one label approval, got %+v", result.LabelApproved)
	}
	if got := strings.Join(rec.removed[45], ","); got != "needs-decision" {
		t.Fatalf("removed = %q, want only needs-decision (hold must stay)", got)
	}
	if len(rec.posted[45]) != 1 || !strings.Contains(rec.posted[45][0], "`hold` (added by @"+laOtherBot+")") || !strings.Contains(rec.posted[45][0], "Left in place") {
		t.Fatalf("expected the comment to list hold as left in place, got %v", rec.posted[45])
	}
}

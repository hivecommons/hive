package github

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#18675: the scanner's not-agent-work verdict changes the
// issue's state, is not repeated unchanged, and a person's later comment
// re-queues the issue.

const notAgentWorkTestBot = "hive[bot]"

func TestNotAgentWorkMarkerRoundTrip(t *testing.T) {
	for _, v := range []string{NotAgentWorkVerdictNotADefect, NotAgentWorkVerdictNeedsHuman, NotAgentWorkVerdictNeedsEnvironment} {
		got, ok := ParseNotAgentWorkMarker("Scanner triage.\n" + NotAgentWorkMarker(v))
		if !ok || got != v {
			t.Fatalf("round trip %q: got %q ok=%v", v, got, ok)
		}
	}
	if _, ok := ParseNotAgentWorkMarker("<!-- hive:not-agent-work verdict=whatever -->"); ok {
		t.Fatal("unknown verdict must not parse")
	}
	if v, ok := NormalizeNotAgentWorkVerdict(" Needs-Environment "); !ok || v != NotAgentWorkVerdictNeedsEnvironment {
		t.Fatalf("normalize = %q %v", v, ok)
	}
	body := notAgentWorkNoteBody(NotAgentWorkVerdictNeedsHuman, "note\n"+NotAgentWorkMarker(NotAgentWorkVerdictNeedsHuman))
	if strings.Count(body, notAgentWorkMarkerPrefix) != 1 || !strings.HasPrefix(body, NotAgentWorkMarker(NotAgentWorkVerdictNeedsHuman)) {
		t.Fatalf("note body must carry the marker exactly once, first: %q", body)
	}
}

func actionableIssue(number int, comments ...unparkWireComment) unparkWireIssue {
	return unparkWireIssue{
		Number:   number,
		Title:    "ioscan selfreport",
		Body:     "something looks wrong",
		User:     wireUser{Login: "reporter"},
		Labels:   []wireLabel{{Name: "kind/bug"}, {Name: "help wanted"}, {Name: stagelabels.Actionable.Label()}},
		Comments: comments,
	}
}

func notAgentWorkClient(t *testing.T, issues []unparkWireIssue) (*Client, *unparkServer, string) {
	t.Helper()
	server, rec := newUnparkServer(t, "org", "repo", issues, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(notAgentWorkTestBot)
	c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
	return c, rec, withIssueDir(t)
}

func botComment(id int64, body string) unparkWireComment {
	cm := humanComment(id, notAgentWorkTestBot, body)
	cm.CreatedAt, cm.UpdatedAt = unparkOld(), unparkOld()
	return cm
}

// A "not startable" verdict moves the issue out of stage/010-actionable onto
// the needs-human park.
func TestIssueRequestWatcherNotAgentWorkParksIssue(t *testing.T) {
	c, rec, dir := notAgentWorkClient(t, []unparkWireIssue{actionableIssue(18636)})
	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "org/repo", Number: 18636, Agent: "scanner",
		NotAgentWork: NotAgentWorkVerdictNeedsEnvironment,
		Body:         "Scanner: not startable by an agent; it needs a Mac on a live Zoom call.",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	resp := readIssueResultFile(t, reqPath)
	if !resp.OK || resp.AlreadyExisted {
		t.Fatalf("result = %+v, want a posted note", resp)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posted[18636]) != 1 {
		t.Fatalf("posted = %v, want one note", rec.posted[18636])
	}
	if v, ok := ParseNotAgentWorkMarker(rec.posted[18636][0]); !ok || v != NotAgentWorkVerdictNeedsEnvironment {
		t.Fatalf("note lacks the verdict marker: %q", rec.posted[18636][0])
	}
	added := strings.Join(rec.added[18636], ",")
	if !strings.Contains(added, issueNeedsHumanLabel) || !strings.Contains(added, NeedsDecisionStageLabel) {
		t.Fatalf("added = %q, want needs-human and the needs-human park stage", added)
	}
	if !strings.Contains(strings.Join(rec.removed[18636], ","), stagelabels.Actionable.Label()) {
		t.Fatalf("removed = %v, want %s off", rec.removed[18636], stagelabels.Actionable.Label())
	}
}

// The marker in the body is read the same way as the structured field.
func TestIssueRequestWatcherNotAgentWorkMarkerInBody(t *testing.T) {
	c, rec, dir := notAgentWorkClient(t, []unparkWireIssue{actionableIssue(18421)})
	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "org/repo", Number: 18421, Agent: "scanner",
		Body: "Config-only condition, not a defect.\n" + NotAgentWorkMarker(NotAgentWorkVerdictNotADefect),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v", resp)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !strings.Contains(strings.Join(rec.added[18421], ","), issueNeedsHumanLabel) {
		t.Fatalf("added = %v, want needs-human", rec.added[18421])
	}
}

// The next scanner pass skips the parked issue: it is filtered out of the
// actionable set.
func TestEnumerateActionableSkipsNotAgentWorkParkedIssue(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 1, Title: "parked", User: wireUser{"alice"}, CreatedAt: hoursAgo(3),
			Labels: []wireLabel{{Name: "kind/bug"}, {Name: "help wanted"}, {Name: issueNeedsHumanLabel}, {Name: NeedsDecisionStageLabel}}},
		{Number: 2, Title: "open work", User: wireUser{"bob"}, CreatedAt: hoursAgo(2),
			Labels: []wireLabel{{Name: "kind/bug"}, {Name: "help wanted"}}},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, []wirePR{}))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	for _, it := range result.Issues.Items {
		if it.Number == 1 {
			t.Fatalf("not-agent-work issue entered the actionable set: %+v", result.Issues.Items)
		}
	}
	for _, row := range result.WorkDetailsByRepo[repo].Issues {
		if row.Number == 1 && row.Bucket != workBucketFiltered {
			t.Fatalf("parked row bucket = %q, want %q", row.Bucket, workBucketFiltered)
		}
	}
}

// A repeated identical triage on the same issue is suppressed; a different
// verdict, or one after a person's comment, is posted.
func TestIssueRequestWatcherNotAgentWorkRepeatSuppressed(t *testing.T) {
	prior := botComment(100, "Scanner triage: needs real logins.\n"+NotAgentWorkMarker(NotAgentWorkVerdictNeedsEnvironment))
	cases := []struct {
		name     string
		comments []unparkWireComment
		verdict  string
		wantPost bool
	}{
		{"identical repeat", []unparkWireComment{prior}, NotAgentWorkVerdictNeedsEnvironment, false},
		{"different verdict", []unparkWireComment{prior}, NotAgentWorkVerdictNotADefect, true},
		{"human commented since", []unparkWireComment{prior, humanComment(101, "reporter", "I can run it on my Mac")}, NotAgentWorkVerdictNeedsEnvironment, true},
		{"marker pasted by a person", []unparkWireComment{humanComment(102, "reporter", prior.Body)}, NotAgentWorkVerdictNeedsEnvironment, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := actionableIssue(18659, tc.comments...)
			c, rec, dir := notAgentWorkClient(t, []unparkWireIssue{issue})
			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "comment", Repo: "org/repo", Number: 18659, Agent: "scanner",
				NotAgentWork: tc.verdict, Body: "Scanner triage: needs real logins.",
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			resp := readIssueResultFile(t, reqPath)
			if !resp.OK {
				t.Fatalf("result = %+v", resp)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			posted := len(rec.posted[18659]) > 0
			if posted != tc.wantPost || resp.AlreadyExisted == tc.wantPost {
				t.Fatalf("posted=%v already_existed=%v, want post=%v (posted %v)", posted, resp.AlreadyExisted, tc.wantPost, rec.posted[18659])
			}
			// Suppressed or not, the issue ends parked.
			if !strings.Contains(strings.Join(rec.added[18659], ","), issueNeedsHumanLabel) {
				t.Fatalf("added = %v, want needs-human", rec.added[18659])
			}
		})
	}
}

func notAgentWorkParkedIssue(number int, comments ...unparkWireComment) unparkWireIssue {
	issue := actionableIssue(number, comments...)
	issue.Labels = []wireLabel{{Name: "kind/bug"}, {Name: issueNeedsHumanLabel}, {Name: NeedsDecisionStageLabel}}
	return issue
}

// A person's comment after the verdict lifts the hive's own needs-human and
// puts the issue back in the queue; a person's needs-human stays.
func TestSweepIssueUnparkRequeuesNotAgentWorkOnHumanComment(t *testing.T) {
	verdict := botComment(200, "Scanner triage.\n"+NotAgentWorkMarker(NotAgentWorkVerdictNeedsHuman))
	reply := humanComment(201, "reporter", "Here is the log you asked for.")
	for _, tc := range []struct {
		name      string
		applier   string
		comments  []unparkWireComment
		wantLift  bool
		wantReply bool
	}{
		{"hive park, human reply", notAgentWorkTestBot, []unparkWireComment{verdict, reply}, true, true},
		{"hive park, no reply", notAgentWorkTestBot, []unparkWireComment{verdict}, false, false},
		{"person's needs-human", "maintainer", []unparkWireComment{verdict, reply}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := notAgentWorkParkedIssue(18700, tc.comments...)
			server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
			rec.timelines = map[int][]unparkWireTimelineEvent{
				18700: {labeledAt(1, tc.applier, issueNeedsHumanLabel, time.Now().Add(-48*time.Hour))},
			}
			c := newTestClient(t, server, "org", []string{"repo"})
			c.SetAppBotLogin(notAgentWorkTestBot)

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			lifted := strings.Contains(strings.Join(rec.removed[18700], ","), issueNeedsHumanLabel)
			if lifted != tc.wantLift {
				t.Fatalf("needs-human lifted=%v, want %v (removed %v)", lifted, tc.wantLift, rec.removed[18700])
			}
			replied := strings.Contains(strings.Join(rec.posted[18700], "\n"), notAgentWorkRequeueMarker)
			if replied != tc.wantReply {
				t.Fatalf("requeue reply=%v, want %v (posted %v)", replied, tc.wantReply, rec.posted[18700])
			}
			if tc.wantLift && (len(result.Unparked) != 1 || result.Unparked[0].Actor != "reporter") {
				t.Fatalf("unparked = %+v", result.Unparked)
			}
			if strings.Contains(strings.Join(rec.added[18700], ","), HumanAckLabel) {
				t.Fatalf("a re-queue is not an approval: %s must not be added (added %v)", HumanAckLabel, rec.added[18700])
			}
		})
	}
}

package github

import (
	"context"
	"testing"
)

// startCall captures one AgentStartHook invocation.
type startCall struct {
	agent  string
	repo   string
	number int
	signal string
}

func recordStarts(c *Client) *[]startCall {
	var calls []startCall
	c.SetAgentStartHook(func(agent, repo string, number int, signal string) {
		calls = append(calls, startCall{agent: agent, repo: repo, number: number, signal: signal})
	})
	return &calls
}

// #10527: an agent's comment through the issue relay is its start signal on
// that issue.
func TestAgentStartHook_FiresOnRelayComment(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	calls := recordStarts(c)
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "verified on v5", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "new finding", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if commented != 1 {
		t.Fatalf("expected 1 comment, got %d", commented)
	}
	want := startCall{agent: "scanner", repo: "o/r", number: 41, signal: AgentStartSignalComment}
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("start signals = %+v, want only %+v (creating an issue is not one)", *calls, want)
	}
}

// A request the relay refuses never reports a start.
func TestAgentStartHook_SilentOnDeniedRequest(t *testing.T) {
	srv := newIssueMockServer(t, "", nil, nil, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	calls := recordStarts(c)
	dir := withIssueDir(t)
	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "b", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if len(*calls) != 0 {
		t.Fatalf("denied request reported start signals: %+v", *calls)
	}
}

// A hive-open-pr request naming issues is a start signal on each of them.
func TestAgentStartHook_FiresOnPRRequestIssues(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	calls := recordStarts(c)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "🐛 fix: thing", Body: "Fixes #1\nRefs #2", Agent: "scanner", IssueN: []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if len(*calls) != 2 || (*calls)[0].number != 1 || (*calls)[1].number != 2 || (*calls)[0].signal != AgentStartSignalPRRequest {
		t.Fatalf("start signals = %+v, want #1 and #2 as pr_request", *calls)
	}
}

func TestAgentStartHook_SetAndClear(t *testing.T) {
	var nilClient *Client
	nilClient.SetAgentStartHook(func(string, string, int, string) {})
	nilClient.notifyAgentStart("a", "o/r", 1, AgentStartSignalComment)

	c := &Client{}
	calls := recordStarts(c)
	c.notifyAgentStart("a", "o/r", 0, AgentStartSignalComment)
	c.notifyAgentStart("", "o/r", 1, AgentStartSignalComment)
	c.notifyAgentStart("a", "o/r", 1, AgentStartSignalLabel)
	c.SetAgentStartHook(nil)
	c.notifyAgentStart("a", "o/r", 2, AgentStartSignalLabel)
	if len(*calls) != 1 || (*calls)[0].number != 1 {
		t.Fatalf("calls = %+v, want one for #1", *calls)
	}
}

func TestIssueRequestStartSignal(t *testing.T) {
	for kind, want := range map[string]string{
		"comment": AgentStartSignalComment, "label": AgentStartSignalLabel, "claim": AgentStartSignalClaim,
		"issue": "", "close": "", "request_review": "",
	} {
		if got := issueRequestStartSignal(kind, "verified on v5", []string{"bug"}); got != want {
			t.Errorf("issueRequestStartSignal(%q) = %q, want %q", kind, got, want)
		}
	}
}

// #12387: a deferral comment parks the issue; it is not a start signal.
func TestIssueRequestStartSignal_Deferral(t *testing.T) {
	cases := []struct {
		name string
		kind string
		body string
		want string
	}{
		{"reported deferral", "comment", "**Deferred this cycle**: #12322 is open and edits the same files; implementing here now would conflict with it. **Picks up once #12322 merges.**", ""},
		{"deferring until", "comment", "Deferring implementation until #12322 merges.", ""},
		{"blocked by bare ref", "comment", "Blocked by #12322.", ""},
		{"blocked by cross-repo ref", "comment", "Blocked by hivecommons/hive#12322; picks up after it merges.", ""},
		{"blocked by PR URL", "comment", "Status: still blocked by https://github.com/hivecommons/hive/pull/12322", ""},
		{"this is blocked on cross-repo ref", "comment", "This is blocked on other/repo#7.", ""},
		{"waiting for ref", "comment", "Waiting for #12322 to land.", ""},
		{"not starting", "comment", "Not starting yet: #12322 touches the same files.", ""},
		{"picks up after cross-repo merges", "comment", "Picks up after hivecommons/hive#12322 merges.", ""},
		{"negated start does not override", "comment", "Deferred: not starting now.", ""},
		{"resume after merge with comma", "comment", "Picking this up after #12322 merged, starting now.", AgentStartSignalComment},
		{"resume after cross-repo merge", "comment", "Picking this up after hivecommons/hive#12322 merged.", AgentStartSignalComment},
		{"historical blocker then start", "comment", "Blocked by #12322 earlier; that merged, starting now.", AgentStartSignalComment},
		{"deferral then unblocked", "comment", "Deferred earlier; no longer blocked, working on it now.", AgentStartSignalComment},
		{"progress comment", "comment", "Working on a fix; repro confirmed on v5.", AgentStartSignalComment},
		{"progress with wait aside", "comment", "Opened PR #5, waiting on #6 CI.", AgentStartSignalComment},
		{"progress with deferral aside", "comment", "Implementing now. Deferring the rename until #9 lands.", AgentStartSignalComment},
		{"past blocker", "comment", "Previously blocked by #12322, which has merged.", AgentStartSignalComment},
		{"label request with deferral text", "label", "Deferred this cycle.", AgentStartSignalLabel},
		{"label request with escalation text", "label", "needs-direction: parking this.", AgentStartSignalLabel},
		{"claim request with deferral text", "claim", "Blocked by #1.", AgentStartSignalClaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var labels []string
			if tc.kind == "label" {
				labels = []string{"kind/bug"}
			}
			if got := issueRequestStartSignal(tc.kind, tc.body, labels); got != tc.want {
				t.Errorf("issueRequestStartSignal(%q, %q) = %q, want %q", tc.kind, tc.body, got, tc.want)
			}
		})
	}
}

// hivecommons/hive#12570, #12481: an escalation comment parks the issue on a
// person; it is not a start signal either.
func TestIssueRequestStartSignal_Escalation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"needs-decision verdict (#12570)", "needs-decision (scanner): parking this RFC out of the agent work queue until the **Maintainer review points** are settled — they are design decisions, not implementation gaps:\n\n1. Final public preset schema.", ""},
		{"needs-direction second pass (#12481)", "needs-direction (scanner, second pass): triage above stands — the 403 is the LiteLLM team key lacking any Claude entitlement, not a Hive defect, and this lane has no PR to open.", ""},
		{"triage not a defect (#12481)", "Triage (scanner): not a Hive code defect — the LiteLLM team key this hive uses has **no Claude entitlement at all**.", ""},
		{"parking opener", "**Parking** this one: the spec is missing.", ""},
		{"parks it mid-comment", "Read through the thread; this parks it until a maintainer answers.", ""},
		{"out of the work queue", "Taking this out of the work queue pending a decision.", ""},
		{"escalation then start now", "needs-direction earlier; maintainer answered, starting now.", AgentStartSignalComment},
		{"progress mentions a bug", "Confirmed: this is not a config problem but a real bug in the parser; fixing.", AgentStartSignalComment},
		{"progress mentions parking lot", "Added the parking-lot note to the design doc and started on the loader.", AgentStartSignalComment},
		{"needs-signal filed as aside", "Repro confirmed; filed a needs-signal issue for the flaky check and working on the fix here.", AgentStartSignalComment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueRequestStartSignal("comment", tc.body, nil); got != tc.want {
				t.Errorf("issueRequestStartSignal(comment, %q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// hivecommons/hive#12481: a label request that only hands the issue to a
// person (needs-direction, needs-spec, …) is not a start signal; one that
// adds any other label still is.
func TestIssueRequestStartSignal_EscalationLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"needs-direction only", []string{"needs-direction"}, ""},
		{"needs-decision only", []string{"needs-decision"}, ""},
		{"needs-spec and needs-signal", []string{"needs-spec", "needs-signal"}, ""},
		{"needs-human mixed case", []string{"Needs-Human"}, ""},
		{"remove-only request", nil, ""},
		{"escalation plus work label", []string{"needs-direction", "area/relay"}, AgentStartSignalLabel},
		{"work label", []string{"kind/bug"}, AgentStartSignalLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueRequestStartSignal("label", "", tc.labels); got != tc.want {
				t.Errorf("issueRequestStartSignal(label, %v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// #12387: the relay still posts a deferral comment but reports no start.
func TestAgentStartHook_DeferralCommentRecordsNoStart(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	calls := recordStarts(c)
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "Deferred this cycle: picks up once #40 merges.", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if commented != 1 {
		t.Fatalf("expected the deferral comment to be posted, got %d comments", commented)
	}
	if len(*calls) != 0 {
		t.Fatalf("deferral comment reported start signals: %+v", *calls)
	}
}

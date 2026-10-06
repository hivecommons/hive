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
		if got := issueRequestStartSignal(kind); got != want {
			t.Errorf("issueRequestStartSignal(%q) = %q, want %q", kind, got, want)
		}
	}
}

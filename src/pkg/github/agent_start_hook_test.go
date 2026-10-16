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

// #12387: a successful deferral comment must still be posted, but must not
// create or renew the two-hour claim that parks the issue after its blocker
// lands. A later, independent work-start request must still signal normally.
func TestAgentStartHook_DeferralCommentIsNotStart(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	calls := recordStarts(c)
	dir := withIssueDir(t)
	body := "**Deferred this cycle**: #12322 is open and edits the same files; implementing here now would conflict with it. **Picks up once #12322 merges.**"
	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: body, Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if commented != 1 || len(*calls) != 0 {
		t.Fatalf("deferral: comments = %d, starts = %+v; want one comment and no start", commented, *calls)
	}

	// The resume comment names the former blocker in past tense: the start
	// phrase must win so the claim is recorded while the agent works.
	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "Previously blocked by #12322; that merged, starting the fix now.", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if commented != 2 || len(*calls) != 1 || (*calls)[0].signal != AgentStartSignalComment {
		t.Fatalf("work start: comments = %d, starts = %+v; want two comments and one start", commented, *calls)
	}
}

func TestIssueRequestStartSignal_Deferrals(t *testing.T) {
	for _, body := range []string{
		"Deferred this cycle: conflicts with #12322.",
		"Deferring implementation until #12322 merges.",
		"Picks up once #12322 merges.",
		"Will pick up after #12322 merges.",
		"I'll pick this up once #12322 merges.",
		"Will pick it up after #12322 lands.",
		"Picks up when the next sweep runs.",
		"Deferred this cycle. Not working on it now.",
		"Deferred this cycle. Once #12322 merges, working on it now will be possible.",
		"BLOCKED BY #12322",
		"This is blocked by #12322 until the schema lands.",
		"Still blocked by #12322.",
		"Waiting on #12322 to land.",
		"Waiting for #12322 to merge.",
		"**Deferred this cycle**: #12322 is open and edits the same files. **Picks up once #12322 merges.**",
		"Not starting yet; still blocked by #12322.",
		"Can't start yet: waiting on #12322.",
		"Picks up once #12322 merges and this is unblocked.",
		"Deferring until #12322 merges. Resuming after that.",
		"Deferred this cycle: blocked by #12322. Will resume once it merges; #12322 was started by scanner.",
		"Deferred this cycle: #12322 already started this refactor; picks up once it merges.",
		"Blocked on #12322; picks up when it merges.",
		"Still blocked on #12322.",
		"Deferring until #12322 merges; the scanner started it already.",
		"Deferred this cycle: #12322 is open. I started a spike but it conflicts.",
		"Deferring until #12322 merges; the scanner is working on it.",
		"Blocked by #12322.\n\nWorking on it means waiting for the schema.",
		"- Blocked by #12322\n- Deferring until it merges",
		"Not starting yet - blocked by #12322 earlier today and still blocked.",
		"Deferred this cycle: #12322 is starting the same refactor; picks up once it merges.",
		"Deferred this cycle: scanner is picking this up in #12322; picks up once it merges.",
		"Deferred this cycle: #12322 is unblocked and the scanner is resuming it; picks up once it merges.",
		"Waiting for #500 CI, then merging.",
		"Waiting on #12322, picks up when it merges.",
	} {
		t.Run(body, func(t *testing.T) {
			if got := issueRequestStartSignal("comment", body); got != "" {
				t.Fatalf("deferral comment reported %q", got)
			}
			for kind, want := range map[string]string{"label": AgentStartSignalLabel, "claim": AgentStartSignalClaim} {
				if got := issueRequestStartSignal(kind, body); got != want {
					t.Errorf("explicit %s with deferral body reported %q, want %q", kind, got, want)
				}
			}
		})
	}
	// Work and resume comments that mention a blocker or a deferral in
	// passing must still signal a start, or the #10527 duplicate-dispatch
	// gap reopens on exactly the resume path #12387 is about. Anchored forms
	// apply to the comment's opening clause only: a later line or clause
	// that defers an aside, or that waits on CI of the PR just opened, is
	// still a work comment.
	for _, body := range []string{
		"Starting work on this issue.",
		"Working on it now.\n\nPicks up once #12322 merges for the second half.",
		"verified on v5",
		"Previously blocked by #12322; that merged, starting now.",
		"Was blocked by #12322, now merged, starting.",
		"No longer blocked by #12322 - starting now",
		"Unblocked: #12322 landed. Resuming.",
		"Picking this up now that #12322 merged.",
		"Deferred earlier, but #12322 merged, so starting now.",
		"Deferred earlier, but #12322 merged; started work now.",
		"Deferring the cleanup, but started now on the main fix.",
		"Still blocked on #12322 an hour ago; unblocked now, picking this up.",
		"Done; cleanup deferred to #999.",
		"Fixed; deferred the cleanup to #999.",
		"Working on a fix.\n- Deferring the refactor to a follow-up.",
		"Working on a fix.\n\nDeferred the refactor to #999.",
		"Implemented the fix in PR #500; waiting on #500 CI.",
		"Implemented the fix in PR #500. Waiting for #500 CI.",
		"Opened PR #500, waiting on #500 CI.",
		"Implementing now. Deferring the rename until #999 lands.",
		"Taking this. Deferring the docs until the API settles.",
		"I am picking this up now that #12322 merged.",
		"#12322 merged so this is unblocked; starting now.",
		"I'm working on this. Second half is blocked by #5 and will follow.",
		"Working on it. Note: the follow-up is blocked by #12322.",
		"Working on it; the follow-up is still blocked by #12322.",
		"Blocked by #12322 earlier; when it merged I started implementing.",
		"Was blocked by #12322 previously. Implementing now.",
		"Tests pass.\nBlocked by #12322 for the last mile, pushing the rest.",
	} {
		t.Run(body, func(t *testing.T) {
			if got := issueRequestStartSignal("comment", body); got != AgentStartSignalComment {
				t.Fatalf("work comment reported %q", got)
			}
		})
	}
}

func TestIssueRequestStartSignal(t *testing.T) {
	for kind, want := range map[string]string{
		"comment": AgentStartSignalComment, "label": AgentStartSignalLabel, "claim": AgentStartSignalClaim,
		"issue": "", "close": "", "request_review": "",
	} {
		if got := issueRequestStartSignal(kind, ""); got != want {
			t.Errorf("issueRequestStartSignal(%q) = %q, want %q", kind, got, want)
		}
	}
}

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

	if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "Starting on this now; repro confirmed on v5.", Agent: "scanner"}); err != nil {
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
		"comment": AgentStartSignalComment, "claim": AgentStartSignalClaim,
		"label": "", "issue": "", "close": "", "request_review": "",
	} {
		if got := issueRequestStartSignal(kind, "Starting on this now."); got != want {
			t.Errorf("issueRequestStartSignal(%q) = %q, want %q", kind, got, want)
		}
	}
}

// A start signal is an explicit start (hivecommons/hive#13011 wore
// stage/030-pr-in-progress on a "Gate check … no implementation started"
// comment when any comment counted): the start marker, a start-now phrase or
// an opening verb of beginning work. Reports, verdicts and status updates
// are not.
func TestIssueRequestStartSignal_CommentMustStateStart(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"gate check (#13011)", "Gate check (scanner, 2026-10-19T20:20Z) — still blocked per the maintainer decision above; no implementation started.\n\n- A #13019 merged ✅\n\nSkipping each kick until then; no further comments unless the gate state changes.", ""},
		{"verification report", "Verified on v5 at d62fe5a: the issue reproduces; the fix in #13054 does not cover it.", ""},
		{"triage verdict", "Triage (scanner): confirmed, kind/bug, area/relay. Priority P2.", ""},
		{"status report", "Status: CI on #5 is red on the podman lane; nothing to do here until it clears.", ""},
		{"progress note without a start", "Opened PR #5, waiting on #6 CI.", ""},
		{"finding without a start", "Confirmed: this is not a config problem but a real bug in the parser; fixing.", ""},
		{"empty body", "", ""},
		{"start marker", CommentStartMarker + "\nTaking a look at the parser.", AgentStartSignalComment},
		{"start marker with attributes", "Plan below.\n<!-- hive:start agent=scanner at=2026-10-19T20:00:00Z -->", AgentStartSignalComment},
		{"starting now phrase", "Repro confirmed; starting now.", AgentStartSignalComment},
		{"working on it now", "Reviewed the thread, working on it now.", AgentStartSignalComment},
		{"implementing now", "Implementing now; PR to follow.", AgentStartSignalComment},
		{"opening Starting", "Starting on the parser fix; repro confirmed on v5.", AgentStartSignalComment},
		{"opening Working on", "Working on a fix; repro confirmed on v5.", AgentStartSignalComment},
		{"opening Implementing", "**Implementing** the relay guard described above.", AgentStartSignalComment},
		{"opening Picking this up", "Picking this up — plan: …", AgentStartSignalComment},
		{"opening Taking this", "Update: taking this one.", AgentStartSignalComment},
		{"negated opener", "Not starting yet: #12322 touches the same files.", ""},
		{"negated start now", "Deferred: not starting now.", ""},
		{"started mid-sentence is not an opener", "The maintainer started a thread on this; summarising it here.", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueRequestStartSignal("comment", tc.body); got != tc.want {
				t.Errorf("issueRequestStartSignal(comment, %q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// #12387: a deferral comment parks the issue; it is not a start signal, and
// a start phrase overrides the deferral.
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
		{"resume after cross-repo merge opens with picking up", "comment", "Picking this up after hivecommons/hive#12322 merged.", AgentStartSignalComment},
		{"historical blocker then start", "comment", "Blocked by #12322 earlier; that merged, starting now.", AgentStartSignalComment},
		{"deferral then unblocked", "comment", "Deferred earlier; no longer blocked, working on it now.", AgentStartSignalComment},
		{"progress with deferral aside", "comment", "Implementing now. Deferring the rename until #9 lands.", AgentStartSignalComment},
		{"start marker overrides deferral", "comment", "Deferred this cycle.\n<!-- hive:start -->", AgentStartSignalComment},
		{"label request with deferral text", "label", "Deferred this cycle.", ""},
		{"claim request with deferral text", "claim", "Blocked by #1.", AgentStartSignalClaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueRequestStartSignal(tc.kind, tc.body); got != tc.want {
				t.Errorf("issueRequestStartSignal(%q, %q) = %q, want %q", tc.kind, tc.body, got, tc.want)
			}
		})
	}
}

// hivecommons/hive#12570, #12481: an escalation comment parks the issue on a
// person; it is not a start signal either, unless it also says work starts.
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
		{"start opener mentions parking lot", "Started on the loader; added the parking-lot note to the design doc.", AgentStartSignalComment},
		{"start opener with needs-signal aside", "Working on the fix here; filed a needs-signal issue for the flaky check.", AgentStartSignalComment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueRequestStartSignal("comment", tc.body); got != tc.want {
				t.Errorf("issueRequestStartSignal(comment, %q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// A label request is never a start signal: the labels an agent may add
// through the relay are triage and verification verdicts. The epic
// hivecommons/hive#12999 was claimed on the scanner's hive/verified-open.
func TestIssueRequestStartSignal_LabelNeverStarts(t *testing.T) {
	for _, body := range []string{"", "Starting now.", CommentStartMarker} {
		if got := issueRequestStartSignal("label", body); got != "" {
			t.Errorf("issueRequestStartSignal(label, %q) = %q, want none", body, got)
		}
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

// hivecommons/hive#13069: only a PR request or a start marker naming a
// branch / PR is PR evidence; a bare claim or start comment is not.
func TestStartSignalIsPREvidence(t *testing.T) {
	cases := map[string]bool{
		AgentStartSignalPRRequest: true,
		AgentStartSignalBranch:    true,
		AgentStartSignalClaim:     false,
		AgentStartSignalComment:   false,
		AgentStartSignalLabel:     false,
		"":                        false,
	}
	for signal, want := range cases {
		if got := StartSignalIsPREvidence(signal); got != want {
			t.Errorf("StartSignalIsPREvidence(%q) = %v, want %v", signal, got, want)
		}
	}
	bodies := map[string]string{
		"Starting now.\n<!-- hive:start branch=fix/13069-stage030 -->": AgentStartSignalBranch,
		"<!-- hive:start pr=13080 -->":                                 AgentStartSignalBranch,
		`<!-- hive:start branch="feat/x" -->`:                          AgentStartSignalBranch,
		"<!-- hive:start -->":                                          AgentStartSignalComment,
		"<!-- hive:start by=scanner -->":                               AgentStartSignalComment,
		"Working on branch fix/x now.":                                 AgentStartSignalComment,
		"Gate check: branch=none, no implementation started.":          "",
	}
	for body, want := range bodies {
		if got := issueRequestStartSignal("comment", body); got != want {
			t.Errorf("issueRequestStartSignal(comment, %q) = %q, want %q", body, got, want)
		}
	}
}

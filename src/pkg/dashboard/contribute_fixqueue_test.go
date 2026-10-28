package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// Stage-072 contributor nudge → relay fix-pr task (hivecommons/hive#12436).

func fixConn(hub *ContributeWSHub, user string, caps *ContributorCapabilities, pong time.Time) *ContributorConnection {
	c := &ContributorConnection{
		connID:       "conn-" + user + "-" + pong.Format("150405.000"),
		profile:      &ContributorProfile{GitHubUsername: user, TrustTier: "contributor"},
		capabilities: caps,
		lastPong:     pong,
	}
	hub.mu.Lock()
	hub.connections[c.connID] = c
	hub.mu.Unlock()
	return c
}

func fixCaps(tokens ...string) *ContributorCapabilities {
	return &ContributorCapabilities{RelayCapabilities: tokens}
}

func fixNudge(repo string, n int, check string) automerge.ContributorNudge {
	return automerge.ContributorNudge{
		Owner: "org", Repo: repo, Display: "org/" + repo, Number: n, HeadSHA: "abc123",
		Author: "alice", Title: "fix thing", URL: "https://x/" + repo + "/" + repo,
		OpenedAt: time.Now().Add(-time.Duration(n) * time.Hour), Reason: "check-failing:" + check,
		FailingChecks: []string{check},
	}
}

func captureFixDispatch(hub *ContributeWSHub) *[]WSMessage {
	var got []WSMessage
	hub.fixDispatched = func(_ *ContributorConnection, m WSMessage) { got = append(got, m) }
	return &got
}

func TestFixQueueNudgeExternalAuthorIsCommentOnly(t *testing.T) {
	hub, _ := covK2Hub(t)
	out := hub.Nudge(context.Background(), fixNudge("r", 1, "build"))
	if out.Mode != string(fixqueue.ModeComment) || out.Online || out.QueueDepth != 0 {
		t.Fatalf("external author: %+v", out)
	}
	if st := hub.FixQueueStatus("alice"); st.Queued != 0 {
		t.Fatalf("nothing should be queued for an external author: %+v", st)
	}
}

func TestFixQueueNudgeOnlinePreemptAndSpawn(t *testing.T) {
	hub, srv := covK2Hub(t)
	got := captureFixDispatch(hub)

	// Relay understands fix-pr but has no parallel sessions → preempt.
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	out := hub.Nudge(context.Background(), fixNudge("r", 1, "build"))
	if !out.Online || out.Mode != string(fixqueue.ModePreempt) {
		t.Fatalf("preempt expected: %+v", out)
	}
	if len(*got) != 1 {
		t.Fatalf("one dispatch expected, got %d", len(*got))
	}
	m := (*got)[0]
	if m.Type != "task_assign" || m.Kind != taskKindFixPR || m.Priority != taskPriorityInterrupt || m.Mode != "preempt" {
		t.Fatalf("task_assign shape: %+v", m)
	}
	if m.Forge != repoScopeDefaultForge || m.Host == "" {
		t.Fatalf("task_assign must carry forge/host: %+v", m)
	}
	if len(m.FixPRs) != 1 || m.FixPRs[0].Number != 1 || m.FixPRs[0].HeadSHA != "abc123" || m.FixPRs[0].FailingChecks[0] != "build" {
		t.Fatalf("payload: %+v", m.FixPRs)
	}
	if !strings.Contains(m.Prompt, "EXISTING PR branch") || !strings.Contains(m.Prompt, "preempts your current work") {
		t.Fatalf("prompt: %q", m.Prompt)
	}
	c.mu.Lock()
	_, tracked := c.fixTasks[m.TaskID]
	c.mu.Unlock()
	if !tracked {
		t.Fatal("fix task should be tracked on the connection")
	}
	if c.currentTask != nil {
		t.Fatal("fix task must not replace currentTask")
	}

	// A second red PR while the preemption is in flight → queued, no interrupt.
	out = hub.Nudge(context.Background(), fixNudge("r", 2, "build"))
	if out.Mode != "queued" || out.QueueDepth != 1 || len(*got) != 1 {
		t.Fatalf("second nudge should queue: %+v dispatches=%d", out, len(*got))
	}

	// Spawn mode on a parallel-capable relay for another contributor: the
	// relay advertises 2 fix sub-sessions beside its primary (capacity 3).
	srv.deps.Config.Contribute.MaxParallelFixes = 4
	bob := fixConn(hub, "bob", &ContributorCapabilities{RelayCapabilities: []string{capFixPRInterrupt, capParallelSessions}, MaxParallelFixes: 2}, time.Now())
	_ = bob
	for i := 10; i < 15; i++ {
		n := fixNudge("r", i, "lint")
		n.Author = "bob"
		n.Reason = "check-failing:lint-" + n.HeadSHA + string(rune('a'+i))
		hub.Nudge(context.Background(), n)
	}
	st := hub.FixQueueStatus("bob")
	if st.InFlight != 2 || st.Queued != 3 || st.Mode != string(fixqueue.ModeSpawn) {
		t.Fatalf("capacity 3 with 5 red PRs: want 2 in flight / 3 queued, got %+v", st)
	}
	spawned := 0
	for _, m := range *got {
		if m.Mode == "spawn" {
			spawned++
		}
	}
	if spawned != 2 {
		t.Fatalf("spawned=%d", spawned)
	}
}

func TestFixQueueCompleteRefillsAndResolveRemoves(t *testing.T) {
	hub, _ := covK2Hub(t)
	got := captureFixDispatch(hub)
	c := fixConn(hub, "alice", &ContributorCapabilities{RelayCapabilities: []string{capFixPRInterrupt, capParallelSessions}, MaxParallelFixes: 1}, time.Now())
	for i := 1; i <= 3; i++ {
		n := fixNudge("r", i, "build")
		n.Reason = "cause-" + string(rune('0'+i))
		hub.Nudge(context.Background(), n)
	}
	if st := hub.FixQueueStatus("alice"); st.InFlight != 1 || st.Queued != 2 {
		t.Fatalf("status: %+v", st)
	}
	first := (*got)[0]
	// task_complete for the fix task settles it and refills the slot.
	s := &wsSession{h: hub, contributor: c}
	s.handleTaskComplete(WSMessage{Type: "task_complete", TaskID: first.TaskID})
	if st := hub.FixQueueStatus("alice"); st.InFlight != 1 || st.Queued != 1 {
		t.Fatalf("after complete: %+v", st)
	}
	if len(*got) != 2 {
		t.Fatalf("refill dispatch expected, got %d", len(*got))
	}
	// An unknown task id is not a fix task.
	if hub.completeFixTask(c, "nope") {
		t.Fatal("unknown task must not be treated as a fix task")
	}
	// Resolve (PR left 072) removes the queued one without a dispatch.
	queued := hub.FixQueueItems("alice")
	if len(queued) != 1 {
		t.Fatalf("queued: %+v", queued)
	}
	hub.Resolve(context.Background(), "org", "r", queued[0].Number)
	if st := hub.FixQueueStatus("alice"); st.Queued != 0 || st.InFlight != 1 || len(*got) != 2 {
		t.Fatalf("after resolve: %+v dispatches=%d", st, len(*got))
	}
	// task_failed on the in-flight fix also settles it.
	second := (*got)[1]
	s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: second.TaskID})
	if st := hub.FixQueueStatus("alice"); st.InFlight != 0 {
		t.Fatalf("after failed: %+v", st)
	}
}

func TestFixQueueBoundaryServesFixesBeforeIssues(t *testing.T) {
	hub, _ := covK2Hub(t)
	// Offline relay contributor (profile on disk, no live connection): the
	// nudge queues without interrupting.
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "carol", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	n := fixNudge("r", 7, "test")
	n.Author = "carol"
	out := hub.Nudge(context.Background(), n)
	if out.Online || out.Mode != "queued" || out.QueueDepth != 1 {
		t.Fatalf("offline relay contributor should queue: %+v", out)
	}
	// Relay comes back and asks for work: the fix is served first.
	c := fixConn(hub, "carol", fixCaps(capFixPRInterrupt), time.Now())
	msg := hub.selectFixTask(c)
	if msg == nil || msg.Kind != taskKindFixPR || msg.Priority != taskPriorityQueued || msg.Mode != "boundary" {
		t.Fatalf("boundary fix expected: %+v", msg)
	}
	// Nothing else queued → the hook yields to issue selection.
	if hub.selectFixTask(c) != nil {
		t.Fatal("in flight only, nothing queued → nil")
	}
	// Another red PR lands while the slot is busy (capacity 1): new work waits.
	n2 := fixNudge("r", 8, "test")
	n2.Author = "carol"
	n2.Reason = "other-cause"
	hub.Nudge(context.Background(), n2)
	msg = hub.selectFixTask(c)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableFixInFlight {
		t.Fatalf("new work must wait: %+v", msg)
	}
	// Nothing queued for a relay without the capability → nil (issue path).
	d := fixConn(hub, "dave", nil, time.Now())
	if hub.selectFixTask(d) != nil {
		t.Fatal("no fix work → nil")
	}
	// Completing the boundary fix refills the slot with the queued one.
	c.mu.Lock()
	var id string
	for k := range c.fixTasks {
		id = k
	}
	c.mu.Unlock()
	if !hub.completeFixTask(c, id) {
		t.Fatal("boundary fix should settle")
	}
	if st := hub.FixQueueStatus("carol"); st.InFlight != 1 || st.Queued != 0 {
		t.Fatalf("refill after boundary completion: %+v", st)
	}
}

func TestFixQueueProfileAPIAndCapacity(t *testing.T) {
	hub, srv := covK2Hub(t)
	srv.contributeHub = hub
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "erin", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	if fq := srv.contributorFixQueue("erin"); fq != nil {
		t.Fatalf("empty queue should be nil, got %+v", fq)
	}
	n := fixNudge("r", 4, "build")
	n.Author = "erin"
	hub.Nudge(context.Background(), n)
	resp := srv.BuildContributorProfile("erin")
	if resp.FixQueue == nil || resp.FixQueue.Queued != 1 || len(resp.FixQueue.PRs) != 1 || resp.FixQueue.PRs[0].Cause != "build" {
		t.Fatalf("profile fix_queue: %+v", resp.FixQueue)
	}

	// Capacity derivation: unadvertised → 1/preempt; parallel clamps to config.
	if cap := hub.fixCapacity(nil); cap.Max != 1 || cap.Parallel {
		t.Fatalf("nil conn: %+v", cap)
	}
	c := &ContributorConnection{capabilities: &ContributorCapabilities{RelayCapabilities: []string{capParallelSessions}, MaxParallelFixes: 99}}
	if cap := hub.fixCapacity(c); !cap.Parallel || cap.Max != hub.maxParallelFixes()+1 || cap.Slots() != hub.maxParallelFixes() {
		t.Fatalf("clamped: %+v (ceiling %d)", cap, hub.maxParallelFixes())
	}
	c.capabilities.MaxParallelFixes = 0
	if cap := hub.fixCapacity(c); !cap.Parallel || cap.Max != 2 || cap.Slots() != 1 {
		t.Fatalf("parallel w/o count: %+v", cap)
	}
	// Sanitized clamps the self-report and IsZero sees it.
	s := ContributorCapabilities{MaxParallelFixes: 1000}.Sanitized()
	if s.MaxParallelFixes != maxParallelFixesCeiling || s.IsZero() {
		t.Fatalf("sanitized: %+v", s)
	}
	if (ContributorCapabilities{MaxParallelFixes: -1}).Sanitized().MaxParallelFixes != 0 {
		t.Fatal("negative must clamp to 0")
	}
	found := false
	for _, tok := range serverCapabilities() {
		if tok == capFixPRInterrupt {
			found = true
		}
	}
	if !found {
		t.Fatal("hub must advertise fix_pr_interrupt_v1")
	}
}

func TestFixPRInstructionCoalesced(t *testing.T) {
	prs := []WSFixPR{{Repo: "o/r", Number: 1, HeadSHA: "0123456789abcdef"}, {Repo: "o/r", Number: 2, HeadSHA: "x", ReviewAsks: []string{"address review"}}}
	s := fixPRInstruction(prs, fixqueue.ModeSpawn)
	for _, want := range []string{"2 of your PRs", "o/r#1 (head 0123456789ab)", "review ask: address review", "parallel fix session"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
}

// hivecommons/hive#14668: a relay that fails the fix-pr task right after
// dispatch must not be reported as reached. The nudge reads "queued" with the
// relay's reason, the PR waits in the queue (no immediate re-interrupt), the
// next task boundary retries it, and after fixTaskMaxAttempts failures it
// leaves the queue for the stage engine's re-nudge / adoption path.
func TestFixQueueInstantRelayFailureIsNotDelivered(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.fixAckGrace = time.Minute
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	s := &wsSession{h: hub, contributor: c}
	var got []WSMessage
	hub.fixDispatched = func(_ *ContributorConnection, m WSMessage) {
		got = append(got, m)
		if len(got) == 1 {
			go s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: m.TaskID, Message: "codex: session busy"})
		}
	}

	out := hub.Nudge(context.Background(), fixNudge("r", 1, "build"))
	if !out.Online || out.Mode != "queued" || !out.RelayFailed || out.RelayError != "codex: session busy" || out.QueueDepth != 1 {
		t.Fatalf("instantly failed dispatch reported as %+v, want queued + relay failure", out)
	}
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want the failed fix not interrupted straight back", len(got))
	}
	if st := hub.FixQueueStatus("alice"); st.Queued != 1 || st.InFlight != 0 {
		t.Fatalf("status after failure = %+v, want requeued", st)
	}
	if q := hub.FixQueueItems("alice"); len(q) != 1 || q[0].Attempts != 1 {
		t.Fatalf("queued = %+v, want one item with 1 failed attempt", q)
	}
	c.mu.Lock()
	tracked := len(c.fixTasks)
	c.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("failed fix task still tracked on the connection (%d)", tracked)
	}

	// The relay's next task boundary retries it; two more failures exhaust it.
	for attempt := 2; attempt <= fixTaskMaxAttempts; attempt++ {
		msg := hub.selectFixTask(c)
		if msg == nil || msg.Kind != taskKindFixPR || msg.Priority != taskPriorityQueued {
			t.Fatalf("attempt %d: boundary should serve the queued fix, got %+v", attempt, msg)
		}
		s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: msg.TaskID})
	}
	if st := hub.FixQueueStatus("alice"); st.Queued != 0 || st.InFlight != 0 {
		t.Fatalf("status after %d failures = %+v, want the PR out of the queue", fixTaskMaxAttempts, st)
	}
	if msg := hub.selectFixTask(c); msg != nil {
		t.Fatalf("exhausted fix served again: %+v", msg)
	}
	hub.fixQueueMu.Lock()
	owned := len(hub.fixOwners["alice"])
	waiters, failures := hub.fixWaiters, len(hub.fixFailures)
	hub.fixQueueMu.Unlock()
	if owned != 0 || waiters != 0 || failures != 0 {
		t.Fatalf("leftover state: owners=%d waiters=%d failures=%d", owned, waiters, failures)
	}
}

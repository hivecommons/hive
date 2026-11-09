package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/github/automerge"
)

// A merged PR must never come back to its author as rework. Observed
// 2026-11-09 on hivecommons/hive#17385: the 12:32Z stage-072 nudge queued a
// fix, the PR left 072 and merged at 13:13:58Z, and at ~14:25Z the hub
// still handed the relay "fix-pr hive#17385", which then ran twice.

// fixPRStates answers the dispatch check from a map keyed by fixqueue key;
// a PR not in the map is open.
func fixPRStates(hub *ContributeWSHub, states map[string]fixPRState) *[]string {
	var asked []string
	hub.fixPRStateHook = func(_ context.Context, repo string, number int) (fixPRState, string, error) {
		key := repo + "#" + strconv.Itoa(number)
		asked = append(asked, key)
		if st, ok := states[key]; ok {
			if st == fixPRStateGone {
				return st, "merged", nil
			}
			return st, "", nil
		}
		return fixPRStateOpen, "", nil
	}
	return &asked
}

// bareNudge is a nudge from a hive whose repos are configured without the
// org: the stage engine's Display is the bare name.
func bareNudge(n int) automerge.ContributorNudge {
	return automerge.ContributorNudge{
		Owner: "hivecommons", Repo: "hive", Display: "hive", Number: n, HeadSHA: "6be425a93fe8",
		Author: "alice", Title: "feat", URL: "https://github.com/hivecommons/hive/pull/" + strconv.Itoa(n),
		OpenedAt: time.Now().Add(-time.Duration(n) * time.Minute), Reason: "conflicting",
		ReviewAsks: []string{"the branch conflicts with the base and needs a rebase"},
	}
}

// The fix task names the repo as owner/name, and the PR leaving stage 072
// (Resolve, which keys on owner/name) finds the queued entry.
func TestFixQueueBareConfiguredRepoQueuesOwnerName(t *testing.T) {
	hub, _ := covK2Hub(t)
	got := captureFixDispatch(hub)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())

	hub.Nudge(context.Background(), bareNudge(1))
	if len(*got) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(*got))
	}
	m := (*got)[0]
	if m.Repo != "hivecommons/hive" || len(m.FixPRs) != 1 || m.FixPRs[0].Repo != "hivecommons/hive" {
		t.Fatalf("fix task repo = %q / %+v, want owner/name hivecommons/hive", m.Repo, m.FixPRs)
	}

	// A second PR waits behind the preemption; it leaves 072 before its turn.
	hub.Nudge(context.Background(), bareNudge(17385))
	if q := hub.FixQueueItems("alice"); len(q) != 1 || q[0].Repo != "hivecommons/hive" {
		t.Fatalf("queued = %+v, want hivecommons/hive#17385", q)
	}
	hub.Resolve(context.Background(), "hivecommons", "hive", 17385)
	if q := hub.FixQueueItems("alice"); len(q) != 0 {
		t.Fatalf("Resolve left %+v in the queue", q)
	}
}

// A PR that merged while it waited is dropped at the task boundary: the relay
// gets no fix task, falls through to issue selection, and the queue is empty.
func TestFixQueueBoundaryDropsPRThatMergedWhileQueued(t *testing.T) {
	hub, _ := covK2Hub(t)
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	got := captureFixDispatch(hub)
	// Queued while the relay was away.
	if out := hub.Nudge(context.Background(), bareNudge(17385)); out.Mode != "queued" || out.QueueDepth != 1 {
		t.Fatalf("offline nudge = %+v, want queued", out)
	}
	// It merges; the relay comes back and asks for work.
	asked := fixPRStates(hub, map[string]fixPRState{"hivecommons/hive#17385": fixPRStateGone})
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	if msg := hub.selectFixTask(c); msg != nil {
		t.Fatalf("selectFixTask = %+v, want nil (no fix for a merged PR, go select an issue)", msg)
	}
	if len(*got) != 0 {
		t.Fatalf("dispatched %+v for a merged PR", *got)
	}
	if len(*asked) != 1 {
		t.Fatalf("state reads = %v, want one", *asked)
	}
	if st := hub.FixQueueStatus("alice"); st.Queued != 0 || st.InFlight != 0 {
		t.Fatalf("status = %+v, want the merged PR out of the queue", st)
	}
	hub.fixQueueMu.Lock()
	owned := len(hub.fixOwners["alice"])
	hub.fixQueueMu.Unlock()
	if owned != 0 {
		t.Fatalf("owner index still holds %d key(s)", owned)
	}
}

// Only the merged one is dropped: an open PR behind it is still dispatched.
func TestFixQueueDispatchSkipsClosedPRAndServesTheNextOpenOne(t *testing.T) {
	hub, _ := covK2Hub(t)
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	got := captureFixDispatch(hub)
	old, open := bareNudge(30), bareNudge(20)
	old.Reason, open.Reason = "conflicting-a", "conflicting-b"
	hub.Nudge(context.Background(), old)
	hub.Nudge(context.Background(), open)
	fixPRStates(hub, map[string]fixPRState{"hivecommons/hive#30": fixPRStateGone})
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	msg := hub.selectFixTask(c)
	if msg == nil || msg.Kind != taskKindFixPR || msg.Number != 20 {
		t.Fatalf("selectFixTask = %+v, want the open PR #20", msg)
	}
	if len(*got) != 1 || (*got)[0].Number != 20 {
		t.Fatalf("dispatches = %+v", *got)
	}
}

// A nudge for a PR that is already merged by the time it reaches the hub
// queues nothing and is reported as a comment-only nudge.
func TestFixQueueNudgeForMergedPRDispatchesNothing(t *testing.T) {
	hub, _ := covK2Hub(t)
	got := captureFixDispatch(hub)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	fixPRStates(hub, map[string]fixPRState{"hivecommons/hive#17385": fixPRStateGone})
	out := hub.Nudge(context.Background(), bareNudge(17385))
	if out.Mode != "comment" || out.QueueDepth != 0 {
		t.Fatalf("nudge = %+v, want comment with nothing queued", out)
	}
	if len(*got) != 0 {
		t.Fatalf("dispatched %+v for a merged PR", *got)
	}
}

// When the PR's state cannot be read the fix is not dispatched and stays
// queued, and the relay is not held idle: selectFixTask returns nothing, so
// the ready handler offers ordinary work. The next boundary asks again.
func TestFixQueueHoldsDispatchWhenPRStateUnreadable(t *testing.T) {
	hub, _ := covK2Hub(t)
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	got := captureFixDispatch(hub)
	hub.Nudge(context.Background(), bareNudge(5))
	hub.fixPRStateHook = func(context.Context, string, int) (fixPRState, string, error) {
		return fixPRStateUnchecked, "", errors.New("GET pulls/5: 502")
	}
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	if msg := hub.selectFixTask(c); msg != nil {
		t.Fatalf("selectFixTask = %+v, want nil (ordinary work offered) while the state is unreadable", msg)
	}
	if len(*got) != 0 || hub.FixQueueStatus("alice").Queued != 1 {
		t.Fatalf("dispatches=%d status=%+v, want nothing dispatched and the fix still queued", len(*got), hub.FixQueueStatus("alice"))
	}
	fixPRStates(hub, nil)
	if msg := hub.selectFixTask(c); msg == nil || msg.Kind != taskKindFixPR {
		t.Fatalf("readable again: selectFixTask = %+v, want the fix", msg)
	}
}

// readFrames collects every frame the relay receives within d.
func readFrames(t *testing.T, client *websocket.Conn, d time.Duration) []map[string]any {
	t.Helper()
	var out []map[string]any
	deadline := time.Now().Add(d)
	for {
		_ = client.SetReadDeadline(deadline)
		_, raw, err := client.ReadMessage()
		if err != nil {
			return out
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, m)
	}
}

// At a task boundary the fix task reaches the relay once. It used to be
// pushed by sendFixTask and then again by the ready handler, so the relay
// started it as its primary task and, on the second copy, in a parallel fix
// session as well.
func TestFixQueueBoundaryTaskIsSentOnce(t *testing.T) {
	hub, s := covK2Hub(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{}
	s.statusMu.Unlock()
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	hub.Nudge(context.Background(), bareNudge(17385))

	server, client := wsPipe(t)
	relay := coverageConn("alice", "scanner", time.Now())
	relay.capabilities = fixCaps(capFixPRInterrupt, capParallelSessions)
	relay.ws = server
	hub.mu.Lock()
	hub.connections["alice"] = relay
	hub.mu.Unlock()
	sess := &wsSession{h: hub, conn: server, contributor: relay}
	if stop := sess.handleReady(WSMessage{Type: "ready"}); stop {
		t.Fatal("ready must not stop the session")
	}
	assigns := 0
	for _, f := range readFrames(t, client, 500*time.Millisecond) {
		if f["type"] == "task_assign" {
			assigns++
			if f["kind"] != taskKindFixPR || f["repo"] != "hivecommons/hive" {
				t.Fatalf("task_assign = %v, want fix-pr for hivecommons/hive", f)
			}
		}
	}
	if assigns != 1 {
		t.Fatalf("relay received %d task_assign frames for one boundary fix, want 1", assigns)
	}
}

// A boundary fix runs as the relay's primary task and reports progress like
// one. It holds no lease, so the progress must not be read as a resume
// claim and answered with task_revoke (which killed the fix mid-run).
func TestFixTaskProgressIsNotRevoked(t *testing.T) {
	hub, _ := covK2Hub(t)
	server, client := wsPipe(t)
	relay := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	relay.ws = server
	relay.fixTasks = map[string]*WSTaskAssign{"fix-alice-1": {TaskID: "fix-alice-1", Kind: taskKindFixPR, Repo: "hivecommons/hive", Number: 17385}}
	sess := &wsSession{h: hub, conn: server, contributor: relay}
	sess.handleTaskProgress(WSMessage{Type: "task_progress", TaskID: "fix-alice-1", Repo: "hivecommons/hive", Number: 17385})
	// A task the hub never issued is still a resume claim and is still
	// revoked; its frame doubles as the marker that the first progress was
	// handled (frames arrive in order).
	sess.handleTaskProgress(WSMessage{Type: "task_progress", TaskID: "not-mine", Repo: "hivecommons/hive", Number: 1})
	var revoked []string
	for _, f := range readFrames(t, client, time.Second) {
		if f["type"] == "task_revoke" {
			id, _ := f["task_id"].(string)
			revoked = append(revoked, id)
		}
	}
	if len(revoked) != 1 || revoked[0] != "not-mine" {
		t.Fatalf("task_revoke frames for %v, want only [not-mine] (never the in-flight fix task)", revoked)
	}
}

// projectbluefin/utah-packages#972 (2026-11-09): the relay refused
// fix-karibhou-1, the hub requeued it and served it again at the relay's
// next ready as fix-karibhou-2 (a boundary fix on the same connection), and
// the relay's progress for that retried task (14:50:21Z and 14:54:36Z, repo
// empty, client_gen 1870) was answered with "task_progress resume rejected:
// no matching server-issued lease" and task_revoke. Here the retry goes
// through the real fail → requeue → boundary dispatch path, and its progress
// carries no repo, as the relay's did: it must not be revoked. The first
// task, which the relay failed, is no longer the relay's and still is.
func TestRetriedBoundaryFixProgressIsNotRevoked(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.fixAckGrace = time.Millisecond
	fixPRStates(hub, nil)
	server, client := wsPipe(t)
	relay := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	relay.ws = server
	sess := &wsSession{h: hub, conn: server, contributor: relay}
	got := captureFixDispatch(hub)

	hub.Nudge(context.Background(), fixNudge("utah-packages", 972, "Canary"))
	if len(*got) != 1 {
		t.Fatalf("dispatches = %d, want the nudge's interrupt", len(*got))
	}
	first := (*got)[0]
	sess.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: first.TaskID, Reason: "Hub is not the active polling slot"})
	retry := hub.selectFixTask(relay)
	if retry == nil || retry.Kind != taskKindFixPR || retry.TaskID == first.TaskID {
		t.Fatalf("the relay's next ready must get the fix again under a new task id, got %+v", retry)
	}

	sess.handleTaskProgress(WSMessage{Type: "task_progress", TaskID: retry.TaskID, TaskGen: retry.TaskGen})
	sess.handleTaskProgress(WSMessage{Type: "task_progress", TaskID: retry.TaskID, TaskGen: retry.TaskGen})
	sess.handleTaskProgress(WSMessage{Type: "task_progress", TaskID: first.TaskID, TaskGen: first.TaskGen})
	var revoked []string
	for _, f := range readFrames(t, client, time.Second) {
		if f["type"] == "task_revoke" {
			id, _ := f["task_id"].(string)
			revoked = append(revoked, id)
		}
	}
	if len(revoked) != 1 || revoked[0] != first.TaskID {
		t.Fatalf("task_revoke frames for %v, want only the failed first task %q (never the retried fix %q)", revoked, first.TaskID, retry.TaskID)
	}
}

package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// handoffFleet is the roster of hivecommons/hive#18749 plus a second agent
// that may open pull requests: scanner and reviewer are advisory,
// ci-maintainer and quality are not.
func handoffFleet() *config.Config {
	level := 5
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Project.Org = "cncf"
	cfg.Agents = map[string]config.AgentConfig{
		"scanner":       {Enabled: true, Mode: "ADVISORY"},
		"reviewer":      {Enabled: true, Mode: "ADVISORY"},
		"ci-maintainer": {Enabled: true, Mode: "ISSUES_AND_PRS"},
		"quality":       {Enabled: true, Mode: "ISSUES_AND_PRS"},
	}
	return cfg
}

func handoffDrafterItem() github.WorkflowDispatch {
	return github.WorkflowDispatch{Repo: "cncf/techdocs", Number: 395, Kind: workflowmodel.KindIssue, Title: "Assess X", State: "accepted",
		Model:  "cncf-techdocs-assessment@1",
		Target: workflowmodel.DispatchTarget{Profile: "techdocs-drafter", State: "accepted", Produces: []string{"pull-request", "push"}, Tools: []string{"write-draft-pr"}}}
}

func handoffRuntime(dlog *scheduler.WorkflowDispatchLog) *workflowDispatchRuntime {
	rt := &workflowDispatchRuntime{pending: map[string]time.Time{}, replies: map[string]workflowReplyCacheEntry{}, drafts: map[string]workflowDraftCacheEntry{}}
	rt.once.Do(func() { rt.log = dlog })
	return rt
}

func handoffLedger(t *testing.T) *claims.Ledger {
	t.Helper()
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// deliver records a planned kick as landed, the way the eval cycle does.
func deliver(t *testing.T, rt *workflowDispatchRuntime, dlog *scheduler.WorkflowDispatchLog, k workflowDispatchKick, at time.Time) {
	t.Helper()
	e := k.Entry
	e.At = at
	if err := dlog.Record(e); err != nil {
		t.Fatal(err)
	}
	rt.clearPending(e.Key)
}

// declinedTurn is the agent manager's view of a kick the agent answered by
// standing down 31 seconds later.
func declinedTurn(at time.Time, line string) func(string) (workflowAgentTurn, bool) {
	return func(string) (workflowAgentTurn, bool) {
		return workflowAgentTurn{LastKick: at, Outcome: crux.KickOutcome{Kind: crux.KickOutcomeStandDown, Reason: line, At: at.Add(31 * time.Second), KickAt: at}}, true
	}
}

// A hand-off the agent declines is not recorded as done: the item goes once
// more to a different agent that may do the work, the first agent's claim
// is released, and after the retry the item waits for the owner
// (hivecommons/hive#18749).
func TestWorkflowDispatchDeclinedHandoffRetriesOnce(t *testing.T) {
	cfg := handoffFleet()
	dlog, _ := scheduler.OpenWorkflowDispatchLog("")
	rt := handoffRuntime(dlog)
	ledger := handoffLedger(t)
	d := handoffDrafterItem()
	items := []github.WorkflowDispatch{d}
	key := scheduler.WorkflowDispatchKey(d, nil)
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// The advisory scanner is never picked: the first agent that may open
	// pull requests is.
	kicks := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, ledger, nil, at, nil)
	if len(kicks) != 1 || kicks[0].Message.Agent != "ci-maintainer" || kicks[0].Entry.Status != scheduler.WorkflowHandoffPending || kicks[0].Entry.Attempts != 1 {
		t.Fatalf("first hand-off = %+v", kicks)
	}
	if msg := kicks[0].Message.Message; !strings.Contains(msg, "Your session runs in mode `ISSUES_AND_PRS`") || strings.Contains(msg, "Tools the model grants") {
		t.Fatalf("the prompt must state the session's own mode:\n%s", msg)
	}
	if c, ok := ledger.Lookup("cncf/techdocs", 395); !ok || c.Holder != "ci-maintainer" {
		t.Fatalf("claim = %+v %v", c, ok)
	}
	deliver(t, rt, dlog, kicks[0], at)
	if again := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, ledger, nil, at.Add(time.Minute), nil); len(again) != 0 {
		t.Fatalf("a pending hand-off was repeated: %+v", again)
	}

	// The agent stands down. The review marks the hand-off not started and
	// frees the claim; the item is listed while it waits.
	seen := reviewWorkflowHandoffs(cfg, dlog, items, nil, declinedTurn(at, "STAND DOWN: my session may not push here"), ledger, true, nil, at.Add(time.Minute), nil)
	rt.noteSeen(seen)
	e, _ := dlog.Get(key)
	if e.Status != scheduler.WorkflowHandoffNotStarted || !strings.Contains(e.Reason, "ci-maintainer declined the hand-off: my session may not push here") || !e.Retryable() {
		t.Fatalf("declined hand-off = %+v", e)
	}
	if dlog.Done(key) {
		t.Fatal("a declined hand-off counts as delivered")
	}
	if _, held := ledger.Lookup("cncf/techdocs", 395); held {
		t.Fatal("the declining agent still holds the claim")
	}
	listed := rt.notStarted(cfg, nil)
	if len(listed) != 1 || listed[0].Repo != "cncf/techdocs" || listed[0].Number != 395 || listed[0].Agent != "ci-maintainer" || !listed[0].Retrying || listed[0].Profile != "techdocs-drafter" {
		t.Fatalf("not-started list = %+v", listed)
	}

	// One automatic retry, to a different agent.
	kicks = buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, ledger, nil, at.Add(2*time.Minute), nil)
	if len(kicks) != 1 || kicks[0].Message.Agent != "quality" || kicks[0].Entry.Attempts != 2 || strings.Join(kicks[0].Entry.Tried, ",") != "ci-maintainer,quality" {
		t.Fatalf("retry = %+v", kicks)
	}
	if !strings.Contains(kicks[0].Message.Message, "An earlier hand-off of this item to ci-maintainer was not started") {
		t.Fatalf("the retry prompt does not say what happened:\n%s", kicks[0].Message.Message)
	}
	if c, ok := ledger.Lookup("cncf/techdocs", 395); !ok || c.Holder != "quality" {
		t.Fatalf("retry claim = %+v %v", c, ok)
	}
	retryAt := at.Add(2 * time.Minute)
	deliver(t, rt, dlog, kicks[0], retryAt)

	// The retry is not started either: no third hand-off, and the list
	// says the item waits for the owner.
	rt.noteSeen(reviewWorkflowHandoffs(cfg, dlog, items, nil, declinedTurn(retryAt, "STAND DOWN: no"), ledger, true, nil, retryAt.Add(time.Minute), nil))
	if e, _ = dlog.Get(key); e.Status != scheduler.WorkflowHandoffNotStarted || e.Retryable() || !dlog.Done(key) {
		t.Fatalf("after the retry = %+v", e)
	}
	if third := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, ledger, nil, retryAt.Add(time.Hour), nil); len(third) != 0 {
		t.Fatalf("a third hand-off was planned: %+v", third)
	}
	if listed = rt.notStarted(cfg, nil); len(listed) != 1 || listed[0].Retrying || !strings.Contains(listed[0].Next, "Send to techdocs-drafter") {
		t.Fatalf("not-started list after the retry = %+v", listed)
	}
	// Once the item leaves the state the hand-off is no longer listed.
	rt.noteSeen(reviewWorkflowHandoffs(cfg, dlog, nil, nil, nil, ledger, true, nil, retryAt.Add(2*time.Hour), nil))
	if listed = rt.notStarted(cfg, nil); len(listed) != 0 {
		t.Fatalf("an item that left the state is still listed: %+v", listed)
	}
	if got := workflowDispatchHandoffs(nil, nil)(); got == nil || len(got) != 0 {
		t.Fatalf("nil config hand-offs = %v", got)
	}
}

// Recovery on upgrade: a record written when the kick landed, for an agent
// that may not open pull requests, is re-evaluated on the next pass. The
// item is handed to the agent that may; nobody has to hunt for it
// (hivecommons/hive#18749).
func TestWorkflowDispatchRecoversStaleHandoff(t *testing.T) {
	cfg := handoffFleet()
	delete(cfg.Agents, "ci-maintainer")
	dlog, _ := scheduler.OpenWorkflowDispatchLog("")
	rt := handoffRuntime(dlog)
	ledger := handoffLedger(t)
	d := handoffDrafterItem()
	items := []github.WorkflowDispatch{d}
	key := scheduler.WorkflowDispatchKey(d, nil)
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	// What the previous version left behind: "dispatched" to the advisory
	// scanner, which holds the claim.
	if err := dlog.Record(scheduler.WorkflowDispatchEntry{Key: key, Ref: d.Ref(), Profile: "techdocs-drafter", State: "accepted", Agent: "scanner", By: "governor", At: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Claim(claims.Request{Repo: "cncf/techdocs", Issue: 395, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if !dlog.Done(key) {
		t.Fatal("the stale record must block until it is reviewed")
	}

	now := at.Add(6 * time.Hour)
	rt.noteSeen(reviewWorkflowHandoffs(cfg, dlog, items, nil, nil, ledger, true, nil, now, nil))
	e, _ := dlog.Get(key)
	if e.Status != scheduler.WorkflowHandoffNotStarted || e.Reason != "scanner may not open pull requests in cncf/techdocs (mode ADVISORY)" || e.Attempts != 0 || !e.Retryable() {
		t.Fatalf("recovered record = %+v", e)
	}
	if _, held := ledger.Lookup("cncf/techdocs", 395); held {
		t.Fatal("the scanner's claim was not released")
	}
	kicks := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, ledger, nil, now, nil)
	if len(kicks) != 1 || kicks[0].Message.Agent != "quality" || kicks[0].Entry.Attempts != 1 {
		t.Fatalf("recovered hand-off = %+v", kicks)
	}

	// On a hive where no agent may open pull requests the item is not
	// handed off, the reason says so, and it is listed as not started.
	cfg.Agents["quality"] = config.AgentConfig{Enabled: true, Mode: "ADVISORY"}
	rt.clearPending(key)
	if none := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, ledger, nil, now, nil); len(none) != 0 {
		t.Fatalf("an advisory-only hive handed the item off: %+v", none)
	}
	res := resolveWorkflowDispatchAgent(cfg, d, false)
	if res.OK() || res.Problem != "no agent on this hive may open pull requests in cncf/techdocs; agents are advisory-only" {
		t.Fatalf("advisory-only resolution = %+v", res)
	}
	if listed := rt.notStarted(cfg, nil); len(listed) != 1 || listed[0].Agent != "scanner" {
		t.Fatalf("not-started list = %+v", listed)
	}
}

// What the agent manager knows about the agent's kick becomes evidence only
// when it is about the hand-off's own turn.
func TestWorkflowHandoffEvidence(t *testing.T) {
	cfg := handoffFleet()
	d := handoffDrafterItem()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	now := at.Add(time.Hour)
	e := scheduler.WorkflowDispatchEntry{Agent: "quality", At: at}
	turn := func(tt workflowAgentTurn, known bool) func(string) (workflowAgentTurn, bool) {
		return func(string) (workflowAgentTurn, bool) { return tt, known }
	}

	if ev := workflowHandoffEvidence(cfg, d, scheduler.WorkflowDispatchEntry{Agent: "scanner", At: at}, nil, now); !strings.Contains(ev.Unqualified, "may not open pull requests") {
		t.Fatalf("advisory agent evidence = %+v", ev)
	}
	if ev := workflowHandoffEvidence(cfg, d, e, nil, now); ev.Unqualified != "" || ev.Declined != "" || !ev.TurnEndedAt.IsZero() || ev.Reviews {
		t.Fatalf("no turn reader = %+v", ev)
	}
	if ev := workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{}, false), now); ev.Declined != "" || !ev.TurnEndedAt.IsZero() {
		t.Fatalf("unknown turn = %+v", ev)
	}
	// The kick-refusal flag the pane watcher raises (agent_declined).
	ev := workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at.Add(time.Second), Refused: true, RefusalReason: "I can't help with that"}, true), now)
	if ev.Declined != "I can't help with that" {
		t.Fatalf("refused kick = %+v", ev)
	}
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at, Refused: true}, true), now); ev.Declined != "the agent refused the kick" {
		t.Fatalf("refused kick without a line = %+v", ev)
	}
	// A kick deferred while the agent starts is not a decline.
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at, Refused: true, RefusalReason: "deferred until startup completes"}, true), now); ev.Declined != "" {
		t.Fatalf("startup deferral read as a decline: %+v", ev)
	}
	// A turn that just ended: the time, no decline.
	ended := crux.KickOutcome{Kind: crux.KickOutcomeEnded, At: at.Add(31 * time.Second), KickAt: at}
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at, Outcome: ended}, true), now); ev.Declined != "" || !ev.TurnEndedAt.Equal(ended.At) {
		t.Fatalf("ended turn = %+v", ev)
	}
	// A question or a nothing-produced report is a hand-off not acted on.
	q := crux.KickOutcome{Kind: crux.KickOutcomeQuestion, At: at.Add(time.Minute), KickAt: at}
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at, Outcome: q}, true), now); ev.Declined != "the turn ended as question" {
		t.Fatalf("question turn = %+v", ev)
	}
	// An outcome of an earlier kick says nothing about this turn.
	old := crux.KickOutcome{Kind: crux.KickOutcomeStandDown, Reason: "STAND DOWN", At: at.Add(-time.Hour), KickAt: at.Add(-2 * time.Hour)}
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at, Outcome: old}, true), now); ev.Declined != "" || !ev.TurnEndedAt.IsZero() {
		t.Fatalf("an earlier turn's outcome was used: %+v", ev)
	}
	// A later kick was delivered: the hand-off's turn was over by then.
	later := at.Add(30 * time.Minute)
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: later, Refused: true, RefusalReason: "about another kick"}, true), now); ev.Declined != "" || !ev.TurnEndedAt.Equal(later) {
		t.Fatalf("later kick = %+v", ev)
	}
	// The manager's last kick predates the hand-off: nothing is known.
	if ev = workflowHandoffEvidence(cfg, d, e, turn(workflowAgentTurn{LastKick: at.Add(-time.Hour), Refused: true}, true), now); ev.Declined != "" || !ev.TurnEndedAt.IsZero() {
		t.Fatalf("older kick = %+v", ev)
	}

	kick := at
	if tt, ok := workflowAgentTurnFrom(&crux.Process{LastKick: &kick, KickRefused: true, KickRefusalReason: "no", KickOutcome: ended}); !ok || !tt.LastKick.Equal(at) || !tt.Refused || tt.RefusalReason != "no" || tt.Outcome.Kind != crux.KickOutcomeEnded {
		t.Fatalf("turn from a process = %+v %v", tt, ok)
	}
	if _, ok := workflowAgentTurnFrom(nil); ok {
		t.Fatal("a nil process has a turn")
	}
	if _, ok := workflowAgentTurnFrom(&crux.Process{}); ok {
		t.Fatal("a never-kicked agent has a turn")
	}
	for ref, want := range map[string]struct {
		repo string
		n    int
	}{"cncf/techdocs#395": {"cncf/techdocs", 395}, "no-number": {"no-number", 0}, "repo#x": {"repo", 0}} {
		if repo, n := splitWorkflowRef(ref); repo != want.repo || n != want.n {
			t.Errorf("splitWorkflowRef(%q) = %q %d", ref, repo, n)
		}
	}
}

// A reply-started profile is reviewed under the key of its triggering
// reply, and only when that reply is still there.
func TestReviewWorkflowHandoffsReplyStarted(t *testing.T) {
	cfg := handoffFleet()
	dlog, _ := scheduler.OpenWorkflowDispatchLog("")
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	v := github.WorkflowDispatch{Repo: "cncf/techdocs", Number: 400, Kind: workflowmodel.KindPR, State: "drafting", UpdatedAt: at.Add(-time.Minute),
		Target: workflowmodel.DispatchTarget{Profile: "techdocs-verifier", State: "drafting", Reply: "/verify", Produces: []string{"comment"}}}
	trigger := github.DispatchReply{CommentID: 7, Author: "nate"}
	key := scheduler.WorkflowDispatchKey(v, &trigger)
	_ = dlog.Record(scheduler.NextWorkflowDispatchEntry(scheduler.WorkflowDispatchEntry{Key: key, Ref: v.Ref(), Profile: "techdocs-verifier", State: "drafting", Agent: "quality", At: at}, nil))
	items := []github.WorkflowDispatch{v, v}
	found := func(github.WorkflowDispatch) (github.DispatchReply, bool, error) { return trigger, true, nil }
	gone := func(github.WorkflowDispatch) (github.DispatchReply, bool, error) {
		return github.DispatchReply{}, false, nil
	}

	// No reply lookup, or the reply is gone: nothing is decided.
	reviewWorkflowHandoffs(cfg, dlog, items, nil, declinedTurn(at, "STAND DOWN: no"), nil, true, nil, at.Add(time.Minute), nil)
	reviewWorkflowHandoffs(cfg, dlog, items, gone, declinedTurn(at, "STAND DOWN: no"), nil, true, nil, at.Add(time.Minute), nil)
	if e, _ := dlog.Get(key); e.Status != scheduler.WorkflowHandoffPending {
		t.Fatalf("decided without the reply: %+v", e)
	}
	seen := reviewWorkflowHandoffs(cfg, dlog, items, found, declinedTurn(at, "STAND DOWN: comments are not allowed"), nil, true, nil, at.Add(time.Minute), nil)
	if e, _ := dlog.Get(key); e.Status != scheduler.WorkflowHandoffNotStarted || !strings.Contains(e.Reason, "comments are not allowed") {
		t.Fatalf("declined verifier = %+v", e)
	}
	if len(seen) != 1 || !seen[workflowHandoffSeenKey(v.Ref(), "techdocs-verifier", "drafting")] {
		t.Fatalf("seen = %v", seen)
	}
}

// An item that left the handed-off state settles its hand-off: the
// deliverable landed, or a person moved the item on. If the item later
// returns to the state (a discarded draft), it is not handed off again on
// its own. A pass that did not enumerate the repositories decides nothing.
func TestWorkflowDispatchItemThatLeftTheStateIsSettled(t *testing.T) {
	cfg := handoffFleet()
	dlog, _ := scheduler.OpenWorkflowDispatchLog("")
	rt := handoffRuntime(dlog)
	d := handoffDrafterItem()
	key := scheduler.WorkflowDispatchKey(d, nil)
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := dlog.Record(scheduler.NextWorkflowDispatchEntry(scheduler.WorkflowDispatchEntry{Key: key, Ref: d.Ref(), Profile: "techdocs-drafter", State: "accepted", Agent: "ci-maintainer", At: at}, nil)); err != nil {
		t.Fatal(err)
	}
	reviewWorkflowHandoffs(cfg, dlog, nil, nil, nil, nil, false, nil, at.Add(time.Hour), nil)
	if e, _ := dlog.Get(key); e.Status != scheduler.WorkflowHandoffPending {
		t.Fatalf("an incomplete pass decided the hand-off: %+v", e)
	}
	reviewWorkflowHandoffs(cfg, dlog, nil, nil, nil, nil, true, nil, at.Add(time.Hour), nil)
	e, _ := dlog.Get(key)
	if e.Status != scheduler.WorkflowHandoffStarted || e.CheckedAt == nil || e.Reason != "" {
		t.Fatalf("an item that left the state = %+v", e)
	}
	// With a counter, one pass without the item settles nothing: a short
	// enumeration must not end the watch. The third in a row does, and an
	// item seen again in between starts the count over.
	dlog2, _ := scheduler.OpenWorkflowDispatchLog("")
	_ = dlog2.Record(scheduler.NextWorkflowDispatchEntry(scheduler.WorkflowDispatchEntry{Key: key, Ref: d.Ref(), Profile: "techdocs-drafter", State: "accepted", Agent: "ci-maintainer", At: at}, nil))
	absences := map[string]int{}
	reviewWorkflowHandoffs(cfg, dlog2, nil, nil, nil, nil, true, absences, at.Add(time.Minute), nil)
	reviewWorkflowHandoffs(cfg, dlog2, nil, nil, nil, nil, true, absences, at.Add(2*time.Minute), nil)
	if p, _ := dlog2.Get(key); p.Status != scheduler.WorkflowHandoffPending || absences[key] != 2 {
		t.Fatalf("settled before %d passes without the item: %+v %v", workflowHandoffLeftPasses, p, absences)
	}
	reviewWorkflowHandoffs(cfg, dlog2, []github.WorkflowDispatch{d}, nil, nil, nil, true, absences, at.Add(3*time.Minute), nil)
	if len(absences) != 0 {
		t.Fatalf("an item seen again did not reset the count: %v", absences)
	}
	for i := 0; i < workflowHandoffLeftPasses; i++ {
		reviewWorkflowHandoffs(cfg, dlog2, nil, nil, nil, nil, true, absences, at.Add(time.Duration(4+i)*time.Minute), nil)
	}
	if p, _ := dlog2.Get(key); p.Status != scheduler.WorkflowHandoffStarted || len(absences) != 0 {
		t.Fatalf("after %d passes without the item = %+v %v", workflowHandoffLeftPasses, p, absences)
	}

	// The issue is back in the state hours later: the settled record
	// stands, whatever the agent's last turn looked like.
	items := []github.WorkflowDispatch{d}
	reviewWorkflowHandoffs(cfg, dlog, items, nil, declinedTurn(at.Add(2*time.Hour), "STAND DOWN: no"), nil, true, nil, at.Add(3*time.Hour), nil)
	if e, _ = dlog.Get(key); e.Status != scheduler.WorkflowHandoffStarted {
		t.Fatalf("a settled hand-off was reviewed again: %+v", e)
	}
	if kicks := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, handoffLedger(t), nil, at.Add(3*time.Hour), nil); len(kicks) != 0 {
		t.Fatalf("an item that returned to the state was handed off again on its own: %+v", kicks)
	}
	if listed := rt.notStarted(cfg, nil); len(listed) != 0 {
		t.Fatalf("a started hand-off is listed as not started: %+v", listed)
	}
}

// cncf/techdocs#395: the owner's manual hand-off and the governor's pass
// are one item with one key. After the governor had declined the item
// (a record under the short repo form, before the key was canonical) and
// the owner then handed it off by hand, the next governor pass sees the
// hand-off: nothing is kicked again, no stale "declined" reason is logged,
// and the dashboard's not-started list does not show the item.
func TestWorkflowDispatchManualHandoffThenGovernorPass(t *testing.T) {
	cfg := handoffFleet()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "workflow-dispatch.json")
	declined := scheduler.WorkflowDispatchEntry{
		Key: "techdocs#395|techdocs-drafter|accepted", Ref: "techdocs#395", Profile: "techdocs-drafter", State: "accepted",
		Agent: "quality", By: "governor", At: at, Status: scheduler.WorkflowHandoffNotStarted,
		Reason: "quality declined the hand-off: stale text", Attempts: 1, Tried: []string{"quality"},
	}
	// The manual path's record, built by the same key function the planner uses.
	d := handoffDrafterItem()
	manual := scheduler.NextWorkflowDispatchEntry(scheduler.WorkflowDispatchEntry{
		Key: scheduler.WorkflowDispatchKey(d, nil), Ref: d.Ref(), Profile: "techdocs-drafter", State: "accepted", Agent: "ci-maintainer", By: "clubanderson", At: at.Add(time.Hour),
	}, nil)
	seed, _ := scheduler.OpenWorkflowDispatchLog(path)
	if err := seed.RecordAll([]scheduler.WorkflowDispatchEntry{declined, manual}); err != nil {
		t.Fatal(err)
	}

	// A restart: the log opens, migrates once, and the governor pass runs.
	dlog, err := scheduler.OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := dlog.MigrateQualifiedRefs(cfg.Project.Org); err != nil || n != 1 {
		t.Fatalf("migration = %d, %v", n, err)
	}
	rt := handoffRuntime(dlog)
	items := []github.WorkflowDispatch{d}
	if e, ok := dlog.Get(scheduler.WorkflowDispatchKey(d, nil)); !ok || e.Status != scheduler.WorkflowHandoffPending || e.By != "clubanderson" {
		t.Fatalf("the governor's key does not find the manual hand-off: %+v %v", e, ok)
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	kicks := buildWorkflowDispatchKicks(cfg, rt, dlog, items, nil, handoffLedger(t), nil, at.Add(2*time.Hour), logger)
	if len(kicks) != 0 {
		t.Fatalf("a manual hand-off was kicked again: %+v", kicks)
	}
	if strings.Contains(logs.String(), "declined") || strings.Contains(logs.String(), "item not handed off") {
		t.Fatalf("a stale reason was logged:\n%s", logs.String())
	}
	rt.noteSeen(reviewWorkflowHandoffs(cfg, dlog, items, nil, nil, nil, true, nil, at.Add(2*time.Hour), logger))
	if listed := rt.notStarted(cfg, nil); len(listed) != 0 {
		t.Fatalf("the dashboard lists a stale not-started record: %+v", listed)
	}
}

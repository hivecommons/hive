package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func pendingHandoff(agent string, at time.Time) WorkflowDispatchEntry {
	return NextWorkflowDispatchEntry(WorkflowDispatchEntry{
		Key: WorkflowDispatchKey(drafterDispatch(), nil), Ref: "cncf/techdocs#395", Profile: "techdocs-drafter", State: "accepted", Agent: agent, By: "governor", At: at,
	}, nil)
}

// A delivered kick is a pending hand-off, not a started one: an agent that
// declines, or ends its turn without the deliverable, did not start it
// (hivecommons/hive#18749).
func TestReviewWorkflowHandoff(t *testing.T) {
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	e := pendingHandoff("quality", at)
	if e.Status != WorkflowHandoffPending || e.Attempts != 1 || len(e.Tried) != 1 || e.Tried[0] != "quality" || e.Settled() || e.Retryable() {
		t.Fatalf("a delivered hand-off = %+v", e)
	}

	// The turn is still running, or nothing is known yet: wait.
	if got, changed := ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: at.Add(30 * time.Minute)}); changed || got.Status != WorkflowHandoffPending {
		t.Fatalf("a running turn was decided: %+v", got)
	}

	// The agent declined: not started at once, with its line, and one
	// retry is left.
	got, changed := ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: at.Add(time.Minute), Declined: "STAND DOWN: my session is advisory"})
	if !changed || got.Status != WorkflowHandoffNotStarted || got.Reason != "quality declined the hand-off: my session is advisory" {
		t.Fatalf("declined hand-off = %+v", got)
	}
	if !got.Retryable() || got.Attempts != 1 || len(got.Tried) != 1 || got.CheckedAt == nil || !got.Settled() {
		t.Fatalf("a declined first hand-off must be retryable once: %+v", got)
	}
	if again, changed := ReviewWorkflowHandoff(got, WorkflowHandoffEvidence{Now: at.Add(time.Hour), Declined: "again"}); changed || again.Reason != got.Reason {
		t.Fatalf("a decided hand-off was reviewed again: %+v", again)
	}

	// The turn ended 31 seconds in and no pull request covers the issue:
	// within the grace the hive waits, past it the hand-off did not start.
	ended := at.Add(31 * time.Second)
	if got, changed = ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: ended.Add(time.Minute), TurnEndedAt: ended}); changed {
		t.Fatalf("decided inside the grace: %+v", got)
	}
	got, changed = ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: ended.Add(WorkflowHandoffGrace), TurnEndedAt: ended})
	if !changed || got.Status != WorkflowHandoffNotStarted || got.Reason != "quality ended its turn without opening the pull request" {
		t.Fatalf("turn ended without the deliverable = %+v", got)
	}

	// A turn end from before the hand-off says nothing about it.
	if got, changed = ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: at.Add(time.Hour), TurnEndedAt: at.Add(-time.Hour)}); changed {
		t.Fatalf("an older turn decided the hand-off: %+v", got)
	}

	// Nothing known about the turn (a restart forgot it): the ceiling.
	got, changed = ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: at.Add(WorkflowHandoffCeiling)})
	if !changed || got.Status != WorkflowHandoffNotStarted || !strings.Contains(got.Reason, "quality did not finish opening the pull request within") {
		t.Fatalf("past the ceiling = %+v", got)
	}

	// A reviewing profile's deliverable is its comment: the item's
	// updated_at moves past the hand-off.
	v := e
	v.Profile, v.State = "techdocs-verifier", "drafting"
	got, changed = ReviewWorkflowHandoff(v, WorkflowHandoffEvidence{Now: ended.Add(time.Minute), Reviews: true, TurnEndedAt: ended, ItemUpdatedAt: at.Add(20 * time.Second)})
	if !changed || got.Status != WorkflowHandoffStarted || got.Reason != "" || got.Retryable() {
		t.Fatalf("a verifier that commented = %+v", got)
	}
	got, changed = ReviewWorkflowHandoff(v, WorkflowHandoffEvidence{Now: ended.Add(WorkflowHandoffGrace), Reviews: true, TurnEndedAt: ended, ItemUpdatedAt: at.Add(-time.Minute)})
	if !changed || got.Status != WorkflowHandoffNotStarted || got.Reason != "quality ended its turn without commenting on the item" {
		t.Fatalf("a verifier that did not comment = %+v", got)
	}
	got, changed = ReviewWorkflowHandoff(v, WorkflowHandoffEvidence{Now: at.Add(WorkflowHandoffCeiling), Reviews: true, ItemUpdatedAt: at.Add(time.Minute)})
	if !changed || got.Status != WorkflowHandoffStarted {
		t.Fatalf("a verifier comment seen at the ceiling = %+v", got)
	}

	// The second hand-off that is not started either is the last: the item
	// waits for the owner.
	second := NextWorkflowDispatchEntry(WorkflowDispatchEntry{Key: e.Key, Ref: e.Ref, Profile: e.Profile, State: e.State, Agent: "ci-maintainer", At: at.Add(time.Hour)},
		&WorkflowDispatchEntry{Agent: "quality", Status: WorkflowHandoffNotStarted, Attempts: 1, Tried: []string{"quality"}})
	if second.Attempts != 2 || strings.Join(second.Tried, ",") != "quality,ci-maintainer" || second.Status != WorkflowHandoffPending {
		t.Fatalf("the retry's record = %+v", second)
	}
	got, _ = ReviewWorkflowHandoff(second, WorkflowHandoffEvidence{Now: at.Add(2 * time.Hour), Declined: "no"})
	if got.Retryable() || got.Attempts != 2 {
		t.Fatalf("a second hand-off that was not started must not be retried: %+v", got)
	}
	if next := WorkflowHandoffNext(got); next != "left for the owner: Send to techdocs-drafter on the item's row" {
		t.Fatalf("next = %q", next)
	}
	if next := WorkflowHandoffNext(WorkflowDispatchEntry{Status: WorkflowHandoffNotStarted, Attempts: 1}); !strings.Contains(next, "another agent") {
		t.Fatalf("next for a retryable hand-off = %q", next)
	}
	// The marker a declining agent opens its line with is not part of the
	// reason; a bare marker still yields one.
	for line, want := range map[string]string{
		"● STAND DOWN — advisory session": "advisory session",
		"Standing down: no push rights":   "no push rights",
		"STAND DOWN.":                     "it stood down without a reason",
		"What should I focus on?":         "What should I focus on?",
	} {
		if got := declineReason(line); got != want {
			t.Errorf("declineReason(%q) = %q; want %q", line, got, want)
		}
	}
}

// Recovery (hivecommons/hive#18749): a record written before outcomes were
// tracked, for an agent that may not do what the profile needs, is not
// started, costs no attempt and leaves the agent free to be picked once it
// can do the work.
func TestReviewWorkflowHandoffRecoversStaleRecord(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	stale := WorkflowDispatchEntry{Key: "cncf/techdocs#395|techdocs-drafter|accepted", Ref: "cncf/techdocs#395", Profile: "techdocs-drafter", State: "accepted", Agent: "scanner", By: "governor", At: at}
	if stale.Settled() || stale.Retryable() {
		t.Fatalf("a record without a status is undecided: %+v", stale)
	}
	got, changed := ReviewWorkflowHandoff(stale, WorkflowHandoffEvidence{Now: at.Add(time.Minute), Unqualified: "may not open pull requests in cncf/techdocs (mode ADVISORY)"})
	if !changed || got.Status != WorkflowHandoffNotStarted || got.Reason != "scanner may not open pull requests in cncf/techdocs (mode ADVISORY)" {
		t.Fatalf("stale record for an advisory agent = %+v", got)
	}
	if !got.Retryable() || got.Attempts != 0 || len(got.Tried) != 0 {
		t.Fatalf("a hand-off to an agent that could not act is no attempt: %+v", got)
	}
	// A pending record of the new kind whose agent has since lost the
	// capability gives its attempt back.
	p := pendingHandoff("scanner", at)
	got, _ = ReviewWorkflowHandoff(p, WorkflowHandoffEvidence{Now: at.Add(time.Minute), Unqualified: "may not open pull requests in cncf/techdocs (mode ADVISORY)"})
	if got.Attempts != 0 || len(got.Tried) != 0 {
		t.Fatalf("attempt not given back: %+v", got)
	}
	// A stale record for an agent that could act is judged like any other:
	// long past the ceiling with the item still in its state, not started,
	// and that counts as the first attempt.
	got, changed = ReviewWorkflowHandoff(stale, WorkflowHandoffEvidence{Now: at.Add(3 * time.Hour)})
	if !changed || got.Status != WorkflowHandoffNotStarted || got.Attempts != 1 || len(got.Tried) != 1 || got.Tried[0] != "scanner" {
		t.Fatalf("stale record for a capable agent = %+v", got)
	}
}

// The log treats a hand-off that was not started and may be retried as not
// done, plans the retry for a different agent, and stops after it.
func TestPlanWorkflowDispatchRetriesNotStartedOnce(t *testing.T) {
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "workflow-dispatch.json")
	l, err := OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	d := drafterDispatch()
	key := WorkflowDispatchKey(d, nil)
	if err := l.Record(pendingHandoff("quality", at)); err != nil {
		t.Fatal(err)
	}
	deps := func() WorkflowDispatchPlanDeps {
		return WorkflowDispatchPlanDeps{
			Agent: func(github.WorkflowDispatch) (string, bool) { return "quality", true },
			Done:  l.Done,
			Prior: func(k string) (WorkflowDispatchEntry, bool) {
				e, ok := l.Get(k)
				return e, ok && e.Retryable()
			},
			RetryAgent: func(_ github.WorkflowDispatch, tried []string) (string, bool) {
				for _, a := range []string{"quality", "ci-maintainer"} {
					if !strings.Contains(","+strings.Join(tried, ",")+",", ","+a+",") {
						return a, true
					}
				}
				return "", false
			},
		}
	}
	// Pending: the hand-off is not repeated while it is awaited.
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{d}, deps()); len(cands) != 0 || len(skips) != 1 || skips[0].Reason != WorkflowSkipDone {
		t.Fatalf("a pending hand-off was planned again: %+v %+v", cands, skips)
	}
	if got := l.NotStarted(); len(got) != 0 {
		t.Fatalf("a pending hand-off is listed as not started: %+v", got)
	}

	// The agent declined: the record is not done, and the retry goes to a
	// different agent.
	e, _ := l.Get(key)
	declined, _ := ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: at.Add(time.Minute), Declined: "STAND DOWN: advisory"})
	if err := l.Record(declined); err != nil {
		t.Fatal(err)
	}
	if l.Done(key) {
		t.Fatal("a declined hand-off still counts as done")
	}
	if got := l.NotStarted(); len(got) != 1 || got[0].Agent != "quality" {
		t.Fatalf("not started = %+v", got)
	}
	cands, _ := PlanWorkflowDispatch([]github.WorkflowDispatch{d}, deps())
	if len(cands) != 1 || cands[0].Agent != "ci-maintainer" || cands[0].Prior == nil || cands[0].Prior.Agent != "quality" {
		t.Fatalf("retry plan = %+v", cands)
	}
	prompt := BuildWorkflowDispatchPrompt(cands[0], "")
	if !strings.Contains(prompt, "An earlier hand-off of this item to quality was not started (quality declined the hand-off: advisory)") {
		t.Errorf("the retry prompt does not say what happened:\n%s", prompt)
	}

	// The retry lands, and is not started either: no third hand-off.
	retry := NextWorkflowDispatchEntry(WorkflowDispatchEntry{Key: key, Ref: d.Ref(), Profile: d.Target.Profile, State: d.State, Agent: cands[0].Agent, At: at.Add(time.Hour)}, cands[0].Prior)
	if err := l.Record(retry); err != nil {
		t.Fatal(err)
	}
	e, _ = l.Get(key)
	failed, _ := ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: at.Add(2 * time.Hour), Declined: "no"})
	if err := l.Record(failed); err != nil {
		t.Fatal(err)
	}
	if !l.Done(key) {
		t.Fatal("after the one retry the hand-off must wait for the owner")
	}
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{d}, deps()); len(cands) != 0 || len(skips) != 1 || skips[0].Reason != WorkflowSkipDone {
		t.Fatalf("a third hand-off was planned: %+v %+v", cands, skips)
	}
	// The record survives a restart with its status.
	reopened, err := OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get(key); !ok || got.Status != WorkflowHandoffNotStarted || got.Attempts != 2 || got.Reason == "" || len(got.Tried) != 2 {
		t.Fatalf("reopened record = %+v %v", got, ok)
	}

	// No other agent qualifies: the item is skipped with the reason, and a
	// retry agent the gate holds back is skipped as ineligible.
	l2, _ := OpenWorkflowDispatchLog("")
	_ = l2.Record(declined)
	only := deps()
	only.Done = l2.Done
	only.Prior = func(k string) (WorkflowDispatchEntry, bool) { e, ok := l2.Get(k); return e, ok && e.Retryable() }
	only.RetryAgent = func(github.WorkflowDispatch, []string) (string, bool) { return "", false }
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{d}, only); len(cands) != 0 || len(skips) != 1 || !strings.HasPrefix(skips[0].Reason, WorkflowSkipNotStarted) || !strings.Contains(skips[0].Reason, "declined") {
		t.Fatalf("no other agent: %+v %+v", cands, skips)
	}
	only.RetryAgent = nil
	if cands, _ := PlanWorkflowDispatch([]github.WorkflowDispatch{d}, only); len(cands) != 0 {
		t.Fatalf("a retry without a resolver was planned: %+v", cands)
	}
	held := deps()
	held.Done, held.Prior = only.Done, only.Prior
	held.Eligible = func(agent string) bool { return agent != "ci-maintainer" }
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{d}, held); len(cands) != 0 || len(skips) != 1 || skips[0].Reason != WorkflowSkipIneligible {
		t.Fatalf("held retry agent: %+v %+v", cands, skips)
	}

	// A recovered record (no attempt counted) is planned for the agent
	// the resolver picks now, with no agent excluded.
	l3, _ := OpenWorkflowDispatchLog("")
	stale, _ := ReviewWorkflowHandoff(WorkflowDispatchEntry{Key: key, Ref: d.Ref(), Profile: "techdocs-drafter", State: "accepted", Agent: "scanner", At: at},
		WorkflowHandoffEvidence{Now: at.Add(time.Minute), Unqualified: "may not open pull requests in cncf/techdocs (mode ADVISORY)"})
	_ = l3.Record(stale)
	rec := deps()
	rec.Done = l3.Done
	rec.Prior = func(k string) (WorkflowDispatchEntry, bool) { e, ok := l3.Get(k); return e, ok && e.Retryable() }
	rec.RetryAgent = func(github.WorkflowDispatch, []string) (string, bool) {
		t.Fatal("a recovered record excluded an agent")
		return "", false
	}
	cands, _ = PlanWorkflowDispatch([]github.WorkflowDispatch{d}, rec)
	if len(cands) != 1 || cands[0].Agent != "quality" || cands[0].Prior == nil {
		t.Fatalf("recovery plan = %+v", cands)
	}
	if next := NextWorkflowDispatchEntry(WorkflowDispatchEntry{Agent: "quality"}, cands[0].Prior); next.Attempts != 1 || len(next.Tried) != 1 || next.Tried[0] != "quality" {
		t.Fatalf("the recovered hand-off's record = %+v", next)
	}
	var nilLog *WorkflowDispatchLog
	if _, ok := nilLog.Get(key); ok || len(nilLog.NotStarted()) != 0 || nilLog.RecordAll([]WorkflowDispatchEntry{{Key: key}}) != nil {
		t.Fatal("nil log misbehaved")
	}

	// An item that left the state settles its hand-off as started; several
	// records are stored in one write, and one without a key is ignored.
	l4, _ := OpenWorkflowDispatchLog(filepath.Join(t.TempDir(), "workflow-dispatch.json"))
	left := WorkflowHandoffLeftState(pendingHandoff("quality", at), at.Add(time.Hour))
	if left.Status != WorkflowHandoffStarted || left.CheckedAt == nil || !left.Settled() || left.Retryable() {
		t.Fatalf("left the state = %+v", left)
	}
	other := left
	other.Key, other.Ref = "cncf/techdocs#396|techdocs-drafter|accepted", "cncf/techdocs#396"
	if err := l4.RecordAll([]WorkflowDispatchEntry{left, other, {Ref: "no key"}}); err != nil {
		t.Fatal(err)
	}
	if err := l4.RecordAll(nil); err != nil {
		t.Fatal(err)
	}
	if got := l4.Entries(); len(got) != 2 || !l4.Done(left.Key) || !l4.Done(other.Key) {
		t.Fatalf("batch record = %+v", got)
	}
}

// The prompt states what the kicked session holds, never what the model's
// grant describes as if it were a permission (hivecommons/hive#18749).
func TestWorkflowDispatchPromptStatesOnlyHeldRights(t *testing.T) {
	d := drafterDispatch()
	d.Target.Tools = []string{"read-repo", "web-research", "write-draft-pr"}
	msg := BuildWorkflowDispatchPrompt(WorkflowDispatchCandidate{Dispatch: d, Agent: "quality", AgentMode: "ISSUES_AND_PRS"}, "")
	for _, want := range []string{
		"The model describes the role: it uses read-repo, web-research, write-draft-pr and produces pull-request, push.",
		"not a permission: it grants your session nothing",
		"Your session runs in mode `ISSUES_AND_PRS`, which lets it push a branch and open a pull request through `hive-open-pr`",
		"do not work around them",
		"`STAND` `DOWN`",
		// A session that looks for a tool named hive-open-pr and stands down
		// never ran a command (hivecommons/hive#18777).
		"`git` and `hive-open-pr` are commands in your shell, not entries in your tool list",
		"`git push origin <branch>`",
		"`hive-open-pr --repo " + d.Repo + " --head <branch>",
		"`--issues " + strconv.Itoa(d.Number) + "`",
		"an empty tool list is not a refusal",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("prompt missing %q:\n%s", want, msg)
		}
	}
	for _, bad := range []string{"Tools the model grants this profile", "It may produce", "You are that profile", "${"} {
		if strings.Contains(msg, bad) {
			t.Errorf("prompt still says %q:\n%s", bad, msg)
		}
	}
	// The instruction must not itself read as a stated stand-down when the
	// pane echoes it: no line of the prompt starts with the marker.
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimLeft(line, " `*-")), WorkflowDeclineMarker) {
			t.Errorf("a prompt line starts with the decline marker: %q", line)
		}
	}

	v := BuildWorkflowDispatchPrompt(WorkflowDispatchCandidate{Dispatch: verifierDispatch(), Agent: "reviewer", AgentMode: "ISSUES_ONLY"}, "")
	if !strings.Contains(v, "Your session runs in mode `ISSUES_ONLY`, which lets it comment on the item") {
		t.Errorf("verifier prompt:\n%s", v)
	}
	if strings.Contains(v, "hive-open-pr --repo") {
		t.Errorf("a commenting hand-off must not carry the pull-request recipe:\n%s", v)
	}
	// With the mode unknown the prompt claims nothing about the session.
	unknown := BuildWorkflowDispatchPrompt(WorkflowDispatchCandidate{Dispatch: d, Agent: "quality"}, "")
	if strings.Contains(unknown, "Your session runs in mode") || !strings.Contains(unknown, "The deliverable needs a session that may push a branch") {
		t.Errorf("prompt with an unknown mode:\n%s", unknown)
	}
	if got := BuildWorkflowDispatchPrompt(WorkflowDispatchCandidate{Dispatch: d, Agent: "quality", AgentMode: "ISSUES_AND_PRS"}, "${AGENT} in ${AGENT_MODE}"); got != "quality in ISSUES_AND_PRS" {
		t.Fatalf("template = %q", got)
	}
}

// writeDispatchLog writes entries as the log's JSON file.
func writeDispatchLog(t *testing.T, entries []WorkflowDispatchEntry) string {
	t.Helper()
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workflow-dispatch.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The live file of cncf/techdocs#395: the governor's declined record under
// the short repo form, and the owner's later manual hand-off under the
// qualified one. After the migration there is one record, the manual
// hand-off, under the qualified key.
func TestMigrateQualifiedRefsTwoEntryFixture(t *testing.T) {
	declinedAt := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	manualAt := declinedAt.Add(3 * time.Hour)
	short := WorkflowDispatchEntry{
		Key: "techdocs#395|techdocs-drafter|accepted", Ref: "techdocs#395", Profile: "techdocs-drafter", State: "accepted",
		Agent: "quality", By: "governor", At: declinedAt, Status: WorkflowHandoffNotStarted,
		Reason:   "quality declined the hand-off: advisory session",
		Attempts: 1, Tried: []string{"quality"},
	}
	manual := WorkflowDispatchEntry{
		Key: "cncf/techdocs#395|techdocs-drafter|accepted", Ref: "cncf/techdocs#395", Profile: "techdocs-drafter", State: "accepted",
		Agent: "ci-maintainer", By: "clubanderson", At: manualAt, Status: WorkflowHandoffPending,
		Attempts: 1, Tried: []string{"ci-maintainer"},
	}
	path := writeDispatchLog(t, []WorkflowDispatchEntry{manual, short})
	l, err := OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := l.MigrateQualifiedRefs("cncf")
	if err != nil || n != 1 {
		t.Fatalf("migrated %d, %v", n, err)
	}
	got := l.Entries()
	if len(got) != 1 {
		t.Fatalf("entries after migration = %+v", got)
	}
	e := got[0]
	if e.Key != manual.Key || e.Ref != "cncf/techdocs#395" || e.Status != WorkflowHandoffPending || e.By != "clubanderson" || e.Agent != "ci-maintainer" || e.Reason != "" {
		t.Fatalf("the handed-off record did not win: %+v", e)
	}
	if e.Attempts != 1 || strings.Join(e.Tried, ",") != "ci-maintainer,quality" {
		t.Fatalf("attempts/tried not carried over: %+v", e)
	}
	if _, ok := l.Get(short.Key); ok {
		t.Fatal("the short-form key is still stored")
	}
	if !l.Done(manual.Key) || len(l.NotStarted()) != 0 {
		t.Fatal("the item is not seen as handed off after the migration")
	}

	// Persisted, and idempotent: a reload migrates nothing and a second run
	// is a no-op.
	re, err := OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := re.Entries(); len(got) != 1 || got[0].Key != manual.Key {
		t.Fatalf("persisted entries = %+v", got)
	}
	if n, err := re.MigrateQualifiedRefs("cncf"); err != nil || n != 0 {
		t.Fatalf("second migration = %d, %v", n, err)
	}
}

// A short record alone is renamed; a not-started record never displaces a
// handed-off one even when it is newer; a comment-triggered key keeps its
// suffix; an empty org changes nothing; the log stays bounded.
func TestMigrateQualifiedRefsRules(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	mk := func(key string, status string, when time.Time) WorkflowDispatchEntry {
		ref := key[:strings.Index(key, "|")]
		return WorkflowDispatchEntry{Key: key, Ref: ref, Profile: "p", State: "s", Agent: "a", At: when, Status: status}
	}
	l, _ := OpenWorkflowDispatchLog("")
	_ = l.RecordAll([]WorkflowDispatchEntry{
		mk("techdocs#1|p|s", WorkflowHandoffStarted, at),
		mk("techdocs#2|p|s", WorkflowHandoffNotStarted, at.Add(time.Hour)),
		mk("cncf/techdocs#2|p|s", WorkflowHandoffPending, at),
		mk("techdocs#3|p|s|77", WorkflowHandoffPending, at),
		mk("cncf/techdocs#4|p|s", WorkflowHandoffPending, at),
	})
	if n, _ := l.MigrateQualifiedRefs(""); n != 0 {
		t.Fatalf("empty org migrated %d", n)
	}
	if n, err := l.MigrateQualifiedRefs("cncf"); err != nil || n != 3 {
		t.Fatalf("migrated %d, %v", n, err)
	}
	if e, ok := l.Get("cncf/techdocs#1|p|s"); !ok || e.Ref != "cncf/techdocs#1" || e.Status != WorkflowHandoffStarted {
		t.Fatalf("renamed record = %+v %v", e, ok)
	}
	if e, ok := l.Get("cncf/techdocs#2|p|s"); !ok || e.Status != WorkflowHandoffPending {
		t.Fatalf("a newer not-started record displaced a handed-off one: %+v %v", e, ok)
	}
	if _, ok := l.Get("cncf/techdocs#3|p|s|77"); !ok {
		t.Fatal("the trigger suffix was lost")
	}
	if got := len(l.Entries()); got != 4 {
		t.Fatalf("entries = %d, want 4", got)
	}
	if n, _ := l.MigrateQualifiedRefs("cncf"); n != 0 {
		t.Fatalf("second run migrated %d", n)
	}
}

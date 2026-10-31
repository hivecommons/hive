package rebasemerge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArmRecordsApprovalFirst(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("hivecommons/hive", 14822, "clubanderson", "v5", "b0f5f80aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, now)
	if f.State != StateApproved || !f.Live() {
		t.Fatalf("state=%q live=%v, want approved/live", f.State, f.Live())
	}
	if len(f.Steps) != 1 || f.Steps[0].Step != StepApprovalRecorded || f.Steps[0].Detail != "approved by @clubanderson on b0f5f80" {
		t.Fatalf("first step = %+v, want approval-recorded", f.Steps)
	}
	f.Kicked(now.Add(time.Second), "fixer")
	if f.State != StateRebasing || f.Steps[1].Step != StepRebaseKicked {
		t.Fatalf("after kick: %+v", f)
	}
}

func TestAdvanceGreenMerges(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 1, "op", "v5", "aaaa111", true, now)
	f.Kicked(now, "update-branch")
	// Still the approved head, dirty, inside the deadline: wait.
	if a := Advance(&f, Observation{Now: now.Add(time.Minute), Head: "aaaa111", State: "open", MergeableState: "dirty"}, Deadlines{}); a != ActionNone || f.State != StateRebasing {
		t.Fatalf("waiting: action=%q state=%q", a, f.State)
	}
	// The rebase lands: new head, CI pending.
	if a := Advance(&f, Observation{Now: now.Add(2 * time.Minute), Head: "bbbb222", State: "open", MergeableState: "blocked"}, Deadlines{}); a != ActionNone || f.State != StateCIPending || f.RebasedHead != "bbbb222" {
		t.Fatalf("rebased: action=%q state=%q head=%q", a, f.State, f.RebasedHead)
	}
	// Green → merge.
	if a := Advance(&f, Observation{Now: now.Add(10 * time.Minute), Head: "bbbb222", State: "open", MergeableState: "clean"}, Deadlines{}); a != ActionMerge {
		t.Fatalf("clean: action=%q", a)
	}
	f.Merged(now.Add(11*time.Minute), "cccc333")
	if f.State != StateMerged || f.Live() {
		t.Fatalf("merged: %+v", f)
	}
	want := []string{StepApprovalRecorded, StepRebaseKicked, StepRebased, StepCIGreen, StepMerged}
	if len(f.Steps) != len(want) {
		t.Fatalf("steps=%+v", f.Steps)
	}
	for i, s := range want {
		if f.Steps[i].Step != s {
			t.Fatalf("step %d = %q, want %q", i, f.Steps[i].Step, s)
		}
	}
}

func TestAdvanceConflictFailsWithFilesAndOptions(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 2, "op", "v5", "aaaa111", false, now)
	f.Kicked(now, "fixer")
	obs := Observation{Now: now.Add(DefaultRebaseDeadline), Head: "aaaa111", State: "open", MergeableState: "dirty", ConflictFiles: []string{"a.go", "b.go"}}
	if a := Advance(&f, obs, Deadlines{}); a != ActionFail {
		t.Fatalf("action=%q", a)
	}
	if f.State != StateFailed || f.FailReason != FailConflict || len(f.ConflictFiles) != 2 {
		t.Fatalf("failed: %+v", f)
	}
	if len(f.Options) != 2 || f.Options[0] != OptionAdoptFresh || f.Options[1] != OptionCloseSuperseded {
		t.Fatalf("options=%v", f.Options)
	}
	if got := f.FailureText(); got != "Rebase of #2 onto v5 conflicts in a.go, b.go." {
		t.Fatalf("text=%q", got)
	}
	// A rebased head that goes dirty again fails as a conflict once the
	// fixer's attempts are exhausted.
	g := Arm("o/r", 3, "op", "v5", "aaaa111", false, now)
	Advance(&g, Observation{Now: now.Add(time.Minute), Head: "bbbb222", State: "open", MergeableState: "blocked", HeadByHive: true}, Deadlines{})
	if a := Advance(&g, Observation{Now: now.Add(2 * time.Minute), Head: "bbbb222", State: "open", MergeableState: "dirty", RebaseExhausted: true}, Deadlines{}); a != ActionFail || g.FailReason != FailConflict {
		t.Fatalf("re-dirty: action=%q reason=%q", a, g.FailReason)
	}
}

// The loop guard inside the follow-through: a rebased head still dirty
// goes back to rebasing while attempts remain (and the rebase clock
// restarts), then fails for a person when the guard says exhausted.
func TestAdvanceStillConflictingRetriesWithinLoopGuard(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 6, "op", "v5", "aaaa111", true, now)
	f.Kicked(now, "scanner")
	Advance(&f, Observation{Now: now.Add(time.Minute), Head: "bbbb222", State: "open", MergeableState: "unknown"}, Deadlines{})
	if f.State != StateCIPending {
		t.Fatalf("state after push = %q", f.State)
	}
	a := Advance(&f, Observation{Now: now.Add(2 * time.Minute), Head: "bbbb222", State: "open", MergeableState: "dirty", RebaseAttempt: 2}, Deadlines{})
	if a != ActionNone || f.State != StateRebasing {
		t.Fatalf("still dirty with attempts left: action=%q state=%q", a, f.State)
	}
	last := f.Steps[len(f.Steps)-1]
	if last.Step != StepStillConflicting || last.Detail != "head bbbb222 still conflicting — rebase attempt 2/2" {
		t.Fatalf("step = %+v", last)
	}
	// The rebase deadline counts from the retry, not from arming.
	at := now.Add(2*time.Minute + DefaultRebaseDeadline - time.Second)
	if a := Advance(&f, Observation{Now: at, Head: "bbbb222", State: "open", MergeableState: "dirty", RebaseAttempt: 2}, Deadlines{}); a != ActionNone || f.State != StateRebasing {
		t.Fatalf("within the restarted deadline: action=%q state=%q", a, f.State)
	}
	// Second fixer head lands and is clean: merge.
	Advance(&f, Observation{Now: at.Add(time.Second), Head: "cccc333", State: "open", MergeableState: "blocked"}, Deadlines{})
	if a := Advance(&f, Observation{Now: at.Add(2 * time.Second), Head: "cccc333", State: "open", MergeableState: "clean"}, Deadlines{}); a != ActionMerge {
		t.Fatalf("clean rebased head: action=%q", a)
	}
	// Exhausted while waiting in rebasing fails at once with the files.
	g := Arm("o/r", 7, "op", "v5", "aaaa111", true, now)
	if a := Advance(&g, Observation{Now: now.Add(time.Minute), Head: "aaaa111", State: "open", MergeableState: "dirty", RebaseExhausted: true, ConflictFiles: []string{"x.go"}}, Deadlines{}); a != ActionFail || g.FailReason != FailConflict || len(g.ConflictFiles) != 1 {
		t.Fatalf("exhausted: action=%q %+v", a, g)
	}
}

func TestAdvanceCIRedTimeoutAndClosed(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 4, "op", "v5", "aaaa111", true, now)
	Advance(&f, Observation{Now: now, Head: "bbbb222", State: "open", MergeableState: "unstable"}, Deadlines{})
	if a := Advance(&f, Observation{Now: now.Add(time.Minute), Head: "bbbb222", State: "open", MergeableState: "unstable", CIRed: true}, Deadlines{}); a != ActionFail || f.FailReason != FailCIRed {
		t.Fatalf("ci red: action=%q reason=%q", a, f.FailReason)
	}
	g := Arm("o/r", 5, "op", "v5", "aaaa111", true, now)
	Advance(&g, Observation{Now: now, Head: "bbbb222", State: "open", MergeableState: "blocked"}, Deadlines{})
	if a := Advance(&g, Observation{Now: now.Add(DefaultCIDeadline), Head: "bbbb222", State: "open", MergeableState: "blocked"}, Deadlines{}); a != ActionFail || g.FailReason != FailTimeout {
		t.Fatalf("timeout: action=%q reason=%q", a, g.FailReason)
	}
	h := Arm("o/r", 6, "op", "v5", "aaaa111", true, now)
	if a := Advance(&h, Observation{Now: now, Head: "aaaa111", State: "closed"}, Deadlines{}); a != ActionFail || h.FailReason != FailClosed {
		t.Fatalf("closed: action=%q reason=%q", a, h.FailReason)
	}
	m := Arm("o/r", 7, "op", "v5", "aaaa111", true, now)
	if a := Advance(&m, Observation{Now: now, Head: "aaaa111", State: "closed", Merged: true}, Deadlines{}); a != ActionNone || m.State != StateMerged {
		t.Fatalf("merged elsewhere: action=%q state=%q", a, m.State)
	}
	// A settled follow-through ignores further observations.
	if a := Advance(&m, Observation{Now: now, Head: "zzz", State: "open", MergeableState: "clean"}, Deadlines{}); a != ActionNone {
		t.Fatalf("settled: action=%q", a)
	}
}

func TestAdvanceCleanWithoutRebaseMerges(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 8, "op", "v5", "aaaa111", true, now)
	if a := Advance(&f, Observation{Now: now, Head: "aaaa111", State: "open", MergeableState: "clean"}, Deadlines{}); a != ActionMerge {
		t.Fatalf("stale needs-rebase: action=%q", a)
	}
}

func TestStorePersistsAndRefusesDoubleArm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	st := NewStore(path)
	f := Arm("O/R", 9, "op", "v5", "aaaa111", true, now)
	if err := st.Put(f); err != nil {
		t.Fatal(err)
	}
	if !st.Armed("o/r", 9) {
		t.Fatal("not armed after Put")
	}
	if err := st.Put(Arm("o/r", 9, "other", "v5", "aaaa111", true, now.Add(time.Minute))); err != ErrAlreadyArmed {
		t.Fatalf("double arm err=%v", err)
	}
	f.Merged(now.Add(time.Hour), "cccc")
	if err := st.Update(f); err != nil {
		t.Fatal(err)
	}
	st2 := NewStore(path)
	got, ok := st2.Get("o/r", 9)
	if !ok || got.State != StateMerged || st2.Armed("o/r", 9) || len(st2.Live()) != 0 {
		t.Fatalf("reloaded: ok=%v %+v live=%d", ok, got, len(st2.Live()))
	}
	// A settled record may be re-armed.
	if err := st2.Put(Arm("o/r", 9, "op", "v5", "dddd", true, now.Add(2*time.Hour))); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
}

// The loop guard: the first dirty head is attempt 1, each new dirty head
// the next, the third is exhausted; a PR that stops conflicting starts
// over; the ledger survives a reload.
func TestAttemptsLoopGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempts.json")
	s := NewAttempts(path)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	a := s.Observe("o/r", 1, "aaa", now)
	if a.Count() != 1 || a.Exhausted() || AttemptText(a) != "Rebasing · attempt 1/2" {
		t.Fatalf("first head: %+v %q", a, AttemptText(a))
	}
	if a = s.Observe("o/r", 1, "aaa", now.Add(time.Minute)); a.Count() != 1 {
		t.Fatalf("same head is the same attempt: %+v", a)
	}
	if a = s.Observe("o/r", 1, "bbb", now.Add(2*time.Minute)); a.Count() != 2 || a.Exhausted() || AttemptText(a) != "Rebasing · attempt 2/2" {
		t.Fatalf("second head: %+v", a)
	}
	if a = s.Observe("o/r", 1, "ccc", now.Add(3*time.Minute)); !a.Exhausted() || AttemptText(a) != "Rebase failed · 2/2 — needs a person" || a.Head() != "ccc" {
		t.Fatalf("third head: %+v %q", a, AttemptText(a))
	}
	if got, ok := NewAttempts(path).Get("o/r", 1); !ok || got.Count() != 3 {
		t.Fatalf("reload: %+v ok=%v", got, ok)
	}
	s.Clear("o/r", 1)
	if _, ok := s.Get("o/r", 1); ok {
		t.Fatal("cleared episode still present")
	}
	if a = s.Observe("o/r", 1, "ddd", now.Add(time.Hour)); a.Count() != 1 {
		t.Fatalf("a new episode starts at 1: %+v", a)
	}
}

// hivecommons/hive#16023: a dispatched attempt the fixer never pushes for
// counts as failed past the deadline, so the episode advances (and is
// exhausted) without a new dirty head; each attempt is dispatched once.
func TestAttemptsExpireAdvancesStalledAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempts.json")
	s := NewAttempts(path)
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	const timeout = 15 * time.Minute
	s.Observe("o/r", 1, "aaa", now)
	if _, ok := s.Expire("o/r", 1, timeout, now.Add(time.Hour)); ok {
		t.Fatal("an attempt never dispatched must not expire")
	}
	a, first := s.Dispatch("o/r", 1, "scanner", now)
	if !first || !a.Dispatched() || a.Fixer != "scanner" || a.DispatchedAttempt != 1 {
		t.Fatalf("first dispatch: %+v first=%v", a, first)
	}
	if a, first = s.Dispatch("o/r", 1, "scanner", now.Add(5*time.Minute)); first || !a.DispatchedAt.Equal(now) {
		t.Fatalf("re-dispatch of the same attempt: %+v first=%v", a, first)
	}
	if got := AttemptTextAt(a, now.Add(12*time.Minute)); got != "Rebasing · attempt 1/2 · waiting for scanner 12m" {
		t.Fatalf("caption: %q", got)
	}
	if got := AttemptTextAt(a, now.Add(30*time.Second)); got != "Rebasing · attempt 1/2 · waiting for scanner <1m" {
		t.Fatalf("fresh caption: %q", got)
	}
	if _, ok := s.Expire("o/r", 1, timeout, now.Add(14*time.Minute)); ok {
		t.Fatal("expired before the deadline")
	}
	if _, ok := s.Expire("o/r", 1, 0, now.Add(time.Hour)); ok {
		t.Fatal("a zero timeout must never expire")
	}
	a, ok := s.Expire("o/r", 1, timeout, now.Add(timeout))
	if !ok || a.Count() != 2 || a.Stalled != 1 || a.Exhausted() || a.Dispatched() || a.Head() != "aaa" {
		t.Fatalf("timed-out attempt 1: %+v ok=%v", a, ok)
	}
	if got := AttemptTextAt(a, now.Add(timeout)); got != "Rebasing · attempt 2/2" {
		t.Fatalf("undispatched attempt 2 caption: %q", got)
	}
	if a, first = s.Dispatch("o/r", 1, "", now.Add(timeout)); !first || a.DispatchedAttempt != 2 {
		t.Fatalf("attempt 2 dispatch: %+v first=%v", a, first)
	}
	if got := AttemptTextAt(a, now.Add(timeout+3*time.Minute)); got != "Rebasing · attempt 2/2 · waiting for the fixer 3m" {
		t.Fatalf("no-fixer caption: %q", got)
	}
	a, ok = s.Expire("o/r", 1, timeout, now.Add(2*timeout))
	if !ok || !a.Exhausted() || AttemptTextAt(a, now.Add(2*timeout)) != "Rebase failed · 2/2 — needs a person" {
		t.Fatalf("timed-out attempt 2: %+v ok=%v", a, ok)
	}
	if _, first = s.Dispatch("o/r", 1, "scanner", now.Add(2*timeout)); first {
		t.Fatal("an exhausted episode must not be dispatched")
	}
	if _, ok = s.Expire("o/r", 1, timeout, now.Add(3*timeout)); ok {
		t.Fatal("an exhausted episode must not expire again")
	}
	if got, ok := NewAttempts(path).Get("o/r", 1); !ok || got.Stalled != 2 || !got.Exhausted() {
		t.Fatalf("reload: %+v ok=%v", got, ok)
	}
	// A push (new dirty head) is the next attempt and needs a fresh dispatch.
	s.Clear("o/r", 1)
	s.Observe("o/r", 1, "bbb", now)
	s.Dispatch("o/r", 1, "scanner", now)
	if a = s.Observe("o/r", 1, "ccc", now.Add(5*time.Minute)); a.Count() != 2 || a.Dispatched() {
		t.Fatalf("pushed head: %+v", a)
	}
	if _, ok = s.Expire("o/r", 1, timeout, now.Add(time.Hour)); ok {
		t.Fatal("a pushed attempt not yet dispatched must not expire")
	}
	if _, first = s.Dispatch("o/r", 9, "scanner", now); first {
		t.Fatal("dispatch without an episode")
	}
}

// Request opens the episode on an operator's behalf (approve / merge-now on
// a dirty PR): the actor is recorded once, a blank actor records nothing,
// and the request rides the same head count as the sweep's Observe.
func TestAttemptsRequestRecordsOperator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempts.json")
	s := NewAttempts(path)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	a := s.Request("o/r", 2, "aaa", "  ", now)
	if a.RequestedBy != "" || a.Count() != 1 {
		t.Fatalf("blank actor: %+v", a)
	}
	a = s.Request("o/r", 2, "aaa", "clubanderson", now.Add(time.Minute))
	if a.RequestedBy != "clubanderson" || a.Count() != 1 || !a.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("request: %+v", a)
	}
	if a = s.Request("o/r", 2, "aaa", "clubanderson", now.Add(2*time.Minute)); !a.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("same actor again must not rewrite the episode: %+v", a)
	}
	if a = s.Request("o/r", 2, "bbb", "other", now.Add(3*time.Minute)); a.RequestedBy != "other" || a.Count() != 2 {
		t.Fatalf("new actor on a new head: %+v", a)
	}
	if got, ok := NewAttempts(path).Get("o/r", 2); !ok || got.RequestedBy != "other" || got.Head() != "bbb" {
		t.Fatalf("reload: %+v ok=%v", got, ok)
	}
	// An empty episode has no head and still reads as attempt 1.
	if (Attempt{}).Head() != "" || AttemptText(Attempt{}) != "Rebasing · attempt 1/2" {
		t.Fatalf("zero attempt: %q", AttemptText(Attempt{}))
	}
	// In-memory ledgers never touch disk; a Clear on nothing is a no-op.
	mem := NewAttempts("")
	mem.Clear("o/r", 99)
	if _, ok := mem.Get("o/r", 99); ok {
		t.Fatal("phantom episode")
	}
	if mem.Observe("o/r", 3, "x", now).Count() != 1 {
		t.Fatal("in-memory observe")
	}
}

// The process-wide singletons point at the configured /data paths and are
// stable across calls.
func TestSharedSingletons(t *testing.T) {
	first, second := Shared(), Shared()
	if first == nil || first != second {
		t.Fatal("Shared store is not a singleton")
	}
	a1, a2 := SharedAttempts(), SharedAttempts()
	if a1 == nil || a1 != a2 {
		t.Fatal("SharedAttempts is not a singleton")
	}
	var nilStore *Store
	if nilStore.Armed("o/r", 1) || nilStore.Live() != nil || nilStore.Put(FollowThrough{}) != nil || nilStore.Update(FollowThrough{}) != nil {
		t.Fatal("nil store must be inert")
	}
}

// FailNow is the driver's own failure (a refused merge, an unreachable
// head); FailureText reads each reason in plain words; Kicked is a no-op
// once the follow-through has left the approved state.
func TestFailNowAndFailureText(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 5, "op", "v5", "aaaa111", false, now)
	f.FailNow(now.Add(time.Minute), FailNotReachable, nil)
	if f.State != StateFailed || f.FailReason != FailNotReachable || f.Live() {
		t.Fatalf("FailNow: %+v", f)
	}
	f.Kicked(now.Add(2*time.Minute), "fixer")
	if f.State != StateFailed {
		t.Fatalf("Kicked after failure moved state: %+v", f)
	}
	cases := map[string]string{
		FailNotReachable: "The hive cannot push to #5's head (fork without maintainer edits).",
		FailCIRed:        "CI went red on the rebased head of #5; nothing merged.",
		FailTimeout:      "The rebase of #5 did not land in time; nothing merged.",
		FailClosed:       "#5 was closed before the hive could merge it.",
		FailConflict:     "Rebase of #5 onto v5 conflicts in a.go, b.go.",
		"odd":            "Rebase-then-merge of #5 failed: odd.",
	}
	for reason, want := range cases {
		g := f
		g.FailReason = reason
		g.ConflictFiles = []string{"a.go", "b.go"}
		if got := g.FailureText(); got != want {
			t.Errorf("%s: %q, want %q", reason, got, want)
		}
	}
	g := f
	g.FailReason, g.Base, g.ConflictFiles = FailConflict, "", nil
	if got := g.FailureText(); got != "Rebase of #5 onto its base conflicts." {
		t.Errorf("conflict without files/base: %q", got)
	}
}

// Persistence failures are reported, not hidden: a store whose path cannot
// be created returns the error from Put, and a corrupt file loads as empty.
func TestStoreAndAttemptsPersistenceEdges(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(blocker, "sub", "f.json"))
	if err := st.Put(Arm("o/r", 1, "op", "v5", "aaa", true, now)); err == nil {
		t.Fatal("Put under a file-as-directory must fail")
	}
	// The attempts ledger swallows the same failure and stays usable.
	at := NewAttempts(filepath.Join(blocker, "sub", "a.json"))
	if at.Observe("o/r", 1, "aaa", now).Count() != 1 {
		t.Fatal("attempts observe under a bad path")
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(corrupt).Live(); len(got) != 0 {
		t.Fatalf("corrupt store loaded %d items", len(got))
	}
	if _, ok := NewAttempts(corrupt).Get("o/r", 1); ok {
		t.Fatal("corrupt attempts loaded an episode")
	}
}

// The approval is bound to the head the operator read. A new head on a
// contributor's PR counts as the rebase only when the hive produced it; a
// head somebody else pushed fails closed with "approve the new head" as
// the way out, and nothing merges. The hive's own PR needs no such check.
func TestAdvanceForeignHeadFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := Arm("o/r", 4, "op", "v5", "aaaa111", false, now)
	f.Kicked(now, "fixer")
	if a := Advance(&f, Observation{Now: now.Add(time.Minute), Head: "bbbb222", State: "open", MergeableState: "clean"}, Deadlines{}); a != ActionFail {
		t.Fatalf("foreign head: action=%q", a)
	}
	if f.State != StateFailed || f.FailReason != FailForeignHead || f.ForeignHead != "bbbb222" || f.RebasedHead != "" {
		t.Fatalf("failed: %+v", f)
	}
	if len(f.Options) != 2 || f.Options[0] != OptionReapprove || f.Options[1] != OptionCloseSuperseded {
		t.Fatalf("options=%v", f.Options)
	}
	if got := f.FailureText(); !strings.Contains(got, "someone pushed bbbb222") || !strings.Contains(got, "approval was for aaaa111") || !strings.Contains(got, "nothing merged") {
		t.Fatalf("text=%q", got)
	}
	if last := f.Steps[len(f.Steps)-1]; last.Step != StepFailed || !strings.Contains(last.Detail, "someone other than the hive") {
		t.Fatalf("last step=%+v", last)
	}
	// The hive's rebase of the same PR is the expected path.
	g := Arm("o/r", 5, "op", "v5", "aaaa111", false, now)
	if a := Advance(&g, Observation{Now: now.Add(time.Minute), Head: "bbbb222", State: "open", MergeableState: "blocked", HeadByHive: true}, Deadlines{}); a != ActionNone || g.State != StateCIPending || g.RebasedHead != "bbbb222" {
		t.Fatalf("hive head: action=%q state=%q head=%q", a, g.State, g.RebasedHead)
	}
	// A hive-authored PR: any new head is the hive's own lane at work.
	h := Arm("o/r", 6, "op", "v5", "aaaa111", true, now)
	if a := Advance(&h, Observation{Now: now.Add(time.Minute), Head: "bbbb222", State: "open", MergeableState: "blocked"}, Deadlines{}); a != ActionNone || h.State != StateCIPending {
		t.Fatalf("hive-authored: action=%q state=%q", a, h.State)
	}
}

package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fake GitHub surface
//
// The fake counts calls per logical effect. Exactly-once is asserted at THIS
// boundary — the number of times the remote was actually asked to do
// something — not merely from the journal's own bookkeeping, which could agree
// with itself while still double-posting.
// ---------------------------------------------------------------------------

type fakeGitHub struct {
	// calls counts Perform invocations per "kind repo/target: body".
	calls map[string]int
	// created records the effects that exist remotely, in creation order.
	created []string
	// reconcileCalls counts lookups, to prove reconciliation actually ran.
	reconcileCalls int
	seq            int
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{calls: map[string]int{}}
}

func effectID(in OpIntent) string {
	return fmt.Sprintf("%s %s/%s: %s", in.Kind, in.Repo, in.Target, in.Body)
}

func (f *fakeGitHub) Perform(ctx context.Context, in OpIntent) (EffectResult, error) {
	id := effectID(in)
	f.calls[id]++
	f.seq++
	ref := fmt.Sprintf("https://github.test/%s/%s#%d", in.Repo, in.Kind, f.seq)
	f.created = append(f.created, id)
	return EffectResult{ExternalRef: ref}, nil
}

// Reconcile is the "natural GitHub idempotency surrogate": ask the remote
// whether this effect already exists. Mirrors github.CreatePR's head-branch
// lookup and its AlreadyExisted result.
func (f *fakeGitHub) Reconcile(ctx context.Context, in OpIntent) (string, bool, error) {
	f.reconcileCalls++
	id := effectID(in)
	for i, c := range f.created {
		if c == id {
			return fmt.Sprintf("https://github.test/%s/%s#%d", in.Repo, in.Kind, i+1), true, nil
		}
	}
	return "", false, nil
}

// duplicates returns every effect the remote was asked to perform more than
// once. A non-empty result is the duplicate-PR/comment bug class from
// #3768 → #3792 → #3980 → #3987.
func (f *fakeGitHub) duplicates() []string {
	var dups []string
	for id, n := range f.calls {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("%s (performed %d times)", id, n))
		}
	}
	return dups
}

// ---------------------------------------------------------------------------
// Killable persister
//
// Persist is the operation boundary. killAt selects which boundary crossing
// dies; every Persist call is one boundary, so counting them and failing at
// index N cuts the turn precisely at the Nth boundary.
// ---------------------------------------------------------------------------

type killablePersister struct {
	// saved is the last successfully persisted envelope JSON — the ONLY state
	// a re-entering process is allowed to read. Deliberately stored as
	// serialized bytes, not a struct, so a test cannot accidentally carry
	// in-memory state across the simulated process boundary.
	saved []byte
	// boundary counts Persist calls made so far.
	boundary int
	// killAt is the 1-based boundary at which to simulate process death.
	// Zero means never kill (the positive control).
	killAt int
}

func (p *killablePersister) Persist(ctx context.Context, env SessionEnvelope) error {
	p.boundary++
	if p.killAt > 0 && p.boundary == p.killAt {
		// Death BEFORE the write lands: the envelope at this boundary never
		// reaches disk. This is the strictly harder case — it is what creates
		// the ambiguous window when the death happens on an intent write.
		return ErrKilled
	}
	// ToJSON applies scrub-on-persist, so the durable artifact the test
	// inspects is exactly what production would write.
	b, err := env.ToJSON()
	if err != nil {
		return err
	}
	p.saved = b
	return nil
}

// ---------------------------------------------------------------------------
// The workload under test
//
// A contribute-shaped turn: comment on the issue, push the branch, open the
// PR, label it. Four side-effectful ops, run in order, each journaled. The
// function is written to be re-entrant — calling it again with the recovered
// envelope resumes rather than restarts.
// ---------------------------------------------------------------------------

const (
	testRepo    = "hivecommons/hive"
	testIssue   = "4002"
	testBranch  = "fix-4002"
	testSession = "sess-replay-1"
)

func workloadOps() []OpIntent {
	return []OpIntent{
		{Kind: OpComment, Repo: testRepo, Target: testIssue, Body: "on it", ToolCallID: "tc-1"},
		{Kind: OpPush, Repo: testRepo, Target: testBranch, Body: "abc123", ToolCallID: "tc-2"},
		{Kind: OpPRCreate, Repo: testRepo, Target: testBranch, Body: "fix: the thing", ToolCallID: "tc-3"},
		{Kind: OpLabel, Repo: testRepo, Target: testIssue, Body: "triage/accepted", ToolCallID: "tc-4"},
	}
}

// runWorkload executes the ops in order against env, stopping at the first
// error (which, for ErrKilled, simulates the process dying mid-turn). It
// returns the terminal verdict when it completes.
func runWorkload(ctx context.Context, x *JournaledExecutor, env *SessionEnvelope) (string, error) {
	for _, op := range workloadOps() {
		if _, err := x.Do(ctx, env, op); err != nil {
			return "", err
		}
	}
	// Terminal verdict, in the contribute plane's vocabulary: a PR landed, so
	// this task shipped. (completionVerdictShipped in pkg/dashboard.)
	env.Status = StatusCompleted
	return "shipped", nil
}

func newEnv() SessionEnvelope {
	return SessionEnvelope{
		SessionID:   testSession,
		ACMMLevel:   4,
		WorkingRepo: testRepo,
		Status:      StatusActive,
	}
}

// ---------------------------------------------------------------------------
// TestKillMidTurnReplayExactlyOnce is the acceptance test mandated by the
// #4002 maintainer review: kill the process at EACH operation boundary,
// re-enter from the persisted envelope, and assert exactly-once effects.
// ---------------------------------------------------------------------------

func TestKillMidTurnReplayExactlyOnce(t *testing.T) {
	ctx := context.Background()

	// --- Positive control: an unkilled run. -------------------------------
	// Establishes the reference effect set and verdict, and proves the
	// workload actually performs effects (without which every kill case below
	// would pass vacuously).
	controlGH := newFakeGitHub()
	controlP := &killablePersister{}
	controlEnv := newEnv()
	controlX := &JournaledExecutor{Sink: controlGH, Reconciler: controlGH, Persister: controlP}

	controlVerdict, err := runWorkload(ctx, controlX, &controlEnv)
	if err != nil {
		t.Fatalf("positive control failed: %v", err)
	}
	if controlVerdict != "shipped" {
		t.Fatalf("positive control verdict = %q, want shipped", controlVerdict)
	}
	wantEffects := len(workloadOps())
	if got := len(controlGH.created); got != wantEffects {
		t.Fatalf("positive control performed %d effects, want %d", got, wantEffects)
	}
	if dups := controlGH.duplicates(); len(dups) > 0 {
		t.Fatalf("positive control itself duplicated: %v", dups)
	}
	controlSummary := controlEnv.Journal.Summary()
	if controlSummary == "" {
		t.Fatal("positive control produced an empty effect summary; comparisons below would be vacuous")
	}
	// The control crossed this many boundaries; every one is a kill site.
	totalBoundaries := controlP.boundary
	if totalBoundaries < wantEffects*2 {
		t.Fatalf("expected at least %d boundaries (intent+settle per op), got %d",
			wantEffects*2, totalBoundaries)
	}

	// --- Table-driven kill at every boundary. -----------------------------
	for boundary := 1; boundary <= totalBoundaries; boundary++ {
		t.Run(fmt.Sprintf("kill_at_boundary_%02d", boundary), func(t *testing.T) {
			gh := newFakeGitHub()
			p := &killablePersister{killAt: boundary}
			env := newEnv()
			x := &JournaledExecutor{Sink: gh, Reconciler: gh, Persister: p}

			// Attempt 1: dies at the chosen boundary.
			_, err := runWorkload(ctx, x, &env)
			if !errors.Is(err, ErrKilled) {
				t.Fatalf("attempt 1: expected ErrKilled at boundary %d, got %v", boundary, err)
			}

			// The process is now gone. Everything in memory is forfeit: the
			// only thing that survives is what reached disk.
			var recovered SessionEnvelope
			if p.saved != nil {
				recovered, err = ParseEnvelope(p.saved)
				if err != nil {
					t.Fatalf("re-entry: parse persisted envelope: %v", err)
				}
			} else {
				// Killed at the very first boundary: nothing persisted, so a
				// re-entering process starts from the task's initial state.
				recovered = newEnv()
			}

			// Attempt 2: re-enter from the persisted envelope, no kill.
			p.killAt = 0
			verdict, err := runWorkload(ctx, x, &recovered)
			if err != nil {
				t.Fatalf("re-entry failed: %v", err)
			}

			// (a) EXACTLY-ONCE EFFECTS.
			if dups := gh.duplicates(); len(dups) > 0 {
				t.Errorf("duplicate effects after kill at boundary %d: %v", boundary, dups)
			}
			if got := len(gh.created); got != wantEffects {
				t.Errorf("performed %d effects across both attempts, want exactly %d (created: %v)",
					got, wantEffects, gh.created)
			}

			// (b) SAME TERMINAL VERDICT as the unkilled control.
			if verdict != controlVerdict {
				t.Errorf("verdict = %q, want %q", verdict, controlVerdict)
			}
			if recovered.Status != StatusCompleted {
				t.Errorf("status = %q, want %q", recovered.Status, StatusCompleted)
			}
			// The set of landed effects must match the control exactly. Refs
			// are position-derived in the fake, and an interrupted run
			// performs the same effects in the same order, so the summaries
			// are directly comparable.
			if got := recovered.Journal.Summary(); got != controlSummary {
				t.Errorf("effect summary diverged from control.\n got:\n%s\nwant:\n%s", got, controlSummary)
			}

			// No operation may be left unsettled once the turn completes.
			if amb := recovered.Journal.Ambiguous(); len(amb) > 0 {
				t.Errorf("%d operations left ambiguous after completion: %+v", len(amb), amb)
			}

			// (c) NO CREDENTIAL IN THE PERSISTED ARTIFACT — asserted
			// separately and more thoroughly in
			// TestReplayPersistedArtifactCarriesNoCredential.
			if strings.Contains(string(p.saved), fakeToken) {
				t.Error("persisted artifact contains an unscrubbed credential")
			}
		})
	}
}

// TestReplayReconcilesAmbiguousIntent proves the mechanism that makes the
// hardest boundary safe: a death AFTER the effect landed but BEFORE the
// outcome was recorded. The journal cannot know locally whether the effect
// happened, so it must ask the remote — and must not replay.
func TestReplayReconcilesAmbiguousIntent(t *testing.T) {
	ctx := context.Background()
	gh := newFakeGitHub()
	env := newEnv()
	op := OpIntent{Kind: OpComment, Repo: testRepo, Target: testIssue, Body: "hello"}

	// Hand-construct the ambiguous state: intent journaled, effect performed
	// remotely, outcome never recorded. This is exactly what a crash in the
	// window between step 5 and step 6 of the protocol leaves behind.
	key := DeriveIdempotencyKey(env.SessionID, op)
	env.Journal.RecordIntent(key, op, env.CreatedAt)
	if _, err := gh.Perform(ctx, op); err != nil {
		t.Fatalf("seed effect: %v", err)
	}
	if got := gh.calls[effectID(op)]; got != 1 {
		t.Fatalf("seed: expected 1 remote call, got %d", got)
	}

	x := &JournaledExecutor{Sink: gh, Reconciler: gh, Persister: &killablePersister{}}
	res, err := x.Do(ctx, &env, op)
	if err != nil {
		t.Fatalf("Do on ambiguous entry: %v", err)
	}

	if gh.reconcileCalls == 0 {
		t.Error("reconciliation never ran; the ambiguous window was resolved by guesswork")
	}
	if got := gh.calls[effectID(op)]; got != 1 {
		t.Errorf("effect performed %d times, want exactly 1 — the pre-crash effect was replayed", got)
	}
	if res.ExternalRef == "" {
		t.Error("expected the reconciled external ref to be returned")
	}
	entry, ok := env.Journal.Lookup(key)
	if !ok || !entry.Done() {
		t.Errorf("journal entry not settled after reconciliation: %+v", entry)
	}
}

// TestReplayNotFoundAfterIntentPerformsEffect is the counterpart positive
// control: an ambiguous entry whose effect did NOT land must be performed on
// re-entry. Without this, a journal that simply skipped every ambiguous entry
// would pass the exactly-once test while silently dropping work — the
// invisible failure mode.
func TestReplayNotFoundAfterIntentPerformsEffect(t *testing.T) {
	ctx := context.Background()
	gh := newFakeGitHub()
	env := newEnv()
	op := OpIntent{Kind: OpComment, Repo: testRepo, Target: testIssue, Body: "never landed"}

	key := DeriveIdempotencyKey(env.SessionID, op)
	env.Journal.RecordIntent(key, op, env.CreatedAt)

	x := &JournaledExecutor{Sink: gh, Reconciler: gh, Persister: &killablePersister{}}
	if _, err := x.Do(ctx, &env, op); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if got := gh.calls[effectID(op)]; got != 1 {
		t.Errorf("effect performed %d times, want exactly 1 — an unlanded effect must be performed on re-entry", got)
	}
	entry, _ := env.Journal.Lookup(key)
	if !entry.Done() {
		t.Errorf("entry not settled: %+v", entry)
	}
}

// TestReplayPersistedArtifactCarriesNoCredential is acceptance criterion (c):
// the durable artifact that a re-entering process (or another spoke) reads
// must contain no plaintext credential, including in the journal fields added
// by stage 2.
func TestReplayPersistedArtifactCarriesNoCredential(t *testing.T) {
	ctx := context.Background()
	gh := newFakeGitHub()
	p := &killablePersister{}
	env := newEnv()

	// Seed the transcript and the journal with credential-shaped strings in
	// every field that stage 2 added to the persisted surface.
	env.AddMessage(RoleUser, "clone with "+fakeToken)
	env.Journal.Entries = append(env.Journal.Entries, JournalEntry{
		IdempotencyKey: "v1-deadbeef",
		Kind:           OpPush,
		Status:         OpFailed,
		Repo:           testRepo,
		Target:         "branch-" + fakeToken,
		ExternalRef:    "https://x-access-token:" + fakeToken + "@github.com/o/r",
		Error:          "remote rejected: bad credentials for " + fakeToken,
	})

	x := &JournaledExecutor{Sink: gh, Reconciler: gh, Persister: p}
	if _, err := runWorkload(ctx, x, &env); err != nil {
		t.Fatalf("workload: %v", err)
	}

	if p.saved == nil {
		t.Fatal("nothing persisted")
	}
	artifact := string(p.saved)

	// Positive control: the in-memory envelope really does carry the secret,
	// so a passing assertion below is meaningful.
	inMemory := env.Clone()
	foundInMemory := false
	for _, e := range inMemory.Journal.Entries {
		if strings.Contains(e.Error, fakeToken) || strings.Contains(e.ExternalRef, fakeToken) {
			foundInMemory = true
		}
	}
	if !foundInMemory {
		t.Fatal("positive control failed: fixture lost the secret before persistence")
	}

	if strings.Contains(artifact, fakeToken) {
		t.Error("persisted artifact contains a plaintext credential")
	}
	if !strings.Contains(artifact, "[REDACTED]") {
		t.Error("expected redaction markers in the persisted artifact")
	}

	// The scrubbed artifact must still be a usable resume point: keys must
	// survive scrubbing intact, or re-entry would fail to match and replay.
	restored, err := ParseEnvelope(p.saved)
	if err != nil {
		t.Fatalf("parse scrubbed artifact: %v", err)
	}
	for _, op := range workloadOps() {
		key := DeriveIdempotencyKey(testSession, op)
		entry, ok := restored.Journal.Lookup(key)
		if !ok {
			t.Errorf("idempotency key %s missing after scrub — re-entry would replay %s", key, op.Kind)
			continue
		}
		if !entry.Done() {
			t.Errorf("entry for %s not settled in the scrubbed artifact", op.Kind)
		}
	}
}

// TestIdempotencyKeyStability locks the two properties the whole design rests
// on: keys are stable across processes, and distinct effects get distinct keys.
func TestIdempotencyKeyStability(t *testing.T) {
	base := OpIntent{Kind: OpComment, Repo: testRepo, Target: testIssue, Body: "hello", ToolCallID: "tc-1"}

	// Stable: same semantic effect, recomputed, yields the same key.
	if a, b := DeriveIdempotencyKey(testSession, base), DeriveIdempotencyKey(testSession, base); a != b {
		t.Errorf("key not stable across recomputation: %s vs %s", a, b)
	}

	// Stable across a DIFFERENT tool call ID: a re-entry that re-runs
	// inference gets a fresh call ID for the same semantic effect, and that
	// must not change the key.
	reentered := base
	reentered.ToolCallID = "tc-99-fresh-after-reentry"
	if a, b := DeriveIdempotencyKey(testSession, base), DeriveIdempotencyKey(testSession, reentered); a != b {
		t.Error("key changed with ToolCallID; re-entry after re-inference would replay the effect")
	}

	// Distinct: every field that locates or defines the effect must move it.
	variants := map[string]OpIntent{
		"kind":   {Kind: OpLabel, Repo: base.Repo, Target: base.Target, Body: base.Body},
		"repo":   {Kind: base.Kind, Repo: "other/repo", Target: base.Target, Body: base.Body},
		"target": {Kind: base.Kind, Repo: base.Repo, Target: "9999", Body: base.Body},
		"body":   {Kind: base.Kind, Repo: base.Repo, Target: base.Target, Body: "different"},
	}
	baseKey := DeriveIdempotencyKey(testSession, base)
	for name, v := range variants {
		if DeriveIdempotencyKey(testSession, v) == baseKey {
			t.Errorf("key collision: %s change did not alter the key", name)
		}
	}

	// Session-scoped: two agents doing the identical thing are distinct ops.
	if DeriveIdempotencyKey("other-session", base) == baseKey {
		t.Error("key collision across sessions; one agent's effect would suppress another's")
	}

	// Field-boundary safety: ("ab","c") must not hash like ("a","bc").
	x := DeriveIdempotencyKey(testSession, OpIntent{Kind: OpComment, Repo: "ab", Target: "c"})
	y := DeriveIdempotencyKey(testSession, OpIntent{Kind: OpComment, Repo: "a", Target: "bc"})
	if x == y {
		t.Error("field-boundary collision: concatenation is ambiguous")
	}
}

// TestIntentIsPersistedBeforeEffect locks the ordering that the entire
// guarantee rests on: the OpIntended record must reach durable storage BEFORE
// the effect is attempted. If the effect is performed first, a crash in that
// window leaves no journal trace at all, and re-entry replays blind.
//
// Reconciliation would normally mask this — the remote lookup finds the orphan
// effect and adopts it — so this test runs WITHOUT a Reconciler, which is also
// the honest configuration for any effect that has no queryable surrogate
// (a push, a bare comment on a busy thread). Without the ordering, that class
// of effect duplicates.
func TestIntentIsPersistedBeforeEffect(t *testing.T) {
	ctx := context.Background()
	gh := newFakeGitHub()
	op := OpIntent{Kind: OpPush, Repo: testRepo, Target: testBranch, Body: "abc123"}

	// Kill at the first boundary. With correct ordering that boundary is the
	// intent write, so the effect is never performed on attempt 1.
	p := &killablePersister{killAt: 1}
	env := newEnv()
	x := &JournaledExecutor{Sink: gh, Persister: p} // no Reconciler, deliberately

	if _, err := x.Do(ctx, &env, op); !errors.Is(err, ErrKilled) {
		t.Fatalf("attempt 1: want ErrKilled, got %v", err)
	}
	if got := gh.calls[effectID(op)]; got != 0 {
		t.Fatalf("effect performed %d times before its intent was durable; "+
			"a crash here is an unjournaled write and re-entry cannot detect it", got)
	}

	// Re-enter and complete.
	p.killAt = 0
	recovered := newEnv()
	if p.saved != nil {
		var err error
		if recovered, err = ParseEnvelope(p.saved); err != nil {
			t.Fatalf("parse: %v", err)
		}
	}
	if _, err := x.Do(ctx, &recovered, op); err != nil {
		t.Fatalf("re-entry: %v", err)
	}
	if got := gh.calls[effectID(op)]; got != 1 {
		t.Errorf("effect performed %d times across both attempts, want exactly 1", got)
	}
}

// TestReplayWithoutReconcilerStillNeverDuplicatesSettledEffects runs the full
// kill table with NO reconciler, isolating what the local journal alone
// guarantees. Settled effects are still protected by the journal; the
// post-effect/pre-settle window is the one that genuinely needs the remote
// (proven by TestReplayReconcilesAmbiguousIntent), so here we assert the
// weaker but still essential property: no effect is performed more than twice,
// and every already-settled effect is skipped outright.
func TestReplayWithoutReconcilerStillNeverDuplicatesSettledEffects(t *testing.T) {
	ctx := context.Background()
	for boundary := 1; boundary <= 8; boundary++ {
		t.Run(fmt.Sprintf("boundary_%02d", boundary), func(t *testing.T) {
			gh := newFakeGitHub()
			p := &killablePersister{killAt: boundary}
			env := newEnv()
			x := &JournaledExecutor{Sink: gh, Persister: p} // no Reconciler

			if _, err := runWorkload(ctx, x, &env); !errors.Is(err, ErrKilled) {
				t.Fatalf("want ErrKilled, got %v", err)
			}
			recovered := newEnv()
			if p.saved != nil {
				var err error
				if recovered, err = ParseEnvelope(p.saved); err != nil {
					t.Fatalf("parse: %v", err)
				}
			}
			p.killAt = 0
			if _, err := runWorkload(ctx, x, &recovered); err != nil {
				t.Fatalf("re-entry: %v", err)
			}

			// Odd boundaries are intent writes: nothing landed, so re-entry is
			// clean and exactly-once holds even without a reconciler.
			if boundary%2 == 1 {
				if dups := gh.duplicates(); len(dups) > 0 {
					t.Errorf("kill on an intent boundary must never duplicate, got %v", dups)
				}
			}
			// No effect may ever be performed more than twice: at most one
			// pre-crash attempt plus one re-entry attempt. More than that
			// means the journal is not suppressing settled effects at all.
			for id, n := range gh.calls {
				if n > 2 {
					t.Errorf("effect %s performed %d times; settled effects are not being suppressed", id, n)
				}
			}
		})
	}
}

// TestPendingApprovalOpShape asserts that a suspended operator-approval wait
// is representable as a journaled, re-enterable position (#4000). Shape only:
// no approvals UI, inbox, or routing is built here.
func TestPendingApprovalOpShape(t *testing.T) {
	ctx := context.Background()
	env := newEnv()
	x := &JournaledExecutor{Sink: newFakeGitHub(), Persister: &killablePersister{}}

	in := OpIntent{Repo: testRepo, Target: testIssue, Body: "delete the production namespace", ToolCallID: "tc-danger"}
	entry, err := x.SuspendForApproval(ctx, &env, in)
	if err != nil {
		t.Fatalf("SuspendForApproval: %v", err)
	}
	if entry.Kind != OpApprovalWait {
		t.Errorf("kind = %q, want %q", entry.Kind, OpApprovalWait)
	}
	if !entry.Ambiguous() {
		t.Error("a pending approval must sit in the unsettled state until an operator decides")
	}

	// Re-entering while still pending must not duplicate the wait.
	again, err := x.SuspendForApproval(ctx, &env, in)
	if err != nil {
		t.Fatalf("re-entry SuspendForApproval: %v", err)
	}
	if again.IdempotencyKey != entry.IdempotencyKey {
		t.Error("re-entry minted a second approval wait for the same request")
	}
	if n := len(env.Journal.Entries); n != 1 {
		t.Errorf("journal has %d entries, want 1", n)
	}

	// An approval wait performs no external effect.
	if env.Journal.EffectCount() != 0 {
		t.Error("an approval wait must not count as a landed effect")
	}

	// It is not routable through Do, which is for side-effectful ops only.
	if _, err := x.Do(ctx, &env, OpIntent{Kind: OpApprovalWait}); err == nil {
		t.Error("Do must reject a non-side-effectful op kind")
	}
}

package turn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN"

type fakeRemote struct {
	calls          map[string]int
	refs           map[string]string
	reconcileCalls int
	fail           map[string]error
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		calls: make(map[string]int),
		refs:  make(map[string]string),
		fail:  make(map[string]error),
	}
}

func effectID(in OpIntent) string {
	return fmt.Sprintf("%s|%s|%s|%s", in.Kind, in.Repo, in.Target, in.Body)
}

func (f *fakeRemote) Perform(_ context.Context, in OpIntent) (EffectResult, error) {
	id := effectID(in)
	f.calls[id]++
	if err := f.fail[id]; err != nil {
		return EffectResult{}, err
	}
	ref := fmt.Sprintf("remote-%d", len(f.refs)+1)
	f.refs[id] = ref
	return EffectResult{ExternalRef: ref}, nil
}

func (f *fakeRemote) Reconcile(_ context.Context, in OpIntent) (string, bool, error) {
	f.reconcileCalls++
	ref, ok := f.refs[effectID(in)]
	return ref, ok, nil
}

type killStore struct {
	killAt   int
	boundary int
	saved    []byte
}

var errKilled = errors.New("test process killed")

func (s *killStore) Persist(_ context.Context, env SessionEnvelope) error {
	s.boundary++
	if s.boundary == s.killAt {
		return errKilled
	}
	data, err := env.ToJSON()
	if err != nil {
		return err
	}
	s.saved = data
	return nil
}

func testEnvelope() SessionEnvelope {
	return SessionEnvelope{
		Version:   EnvelopeVersion,
		SessionID: "session-4002",
		Agent:     "contributor",
		TaskRef:   "kubestellar/hive#4002",
		Status:    StatusActive,
		Messages: []Message{{
			Role:      RoleUser,
			Content:   "implement the re-entrant turn spike",
			Timestamp: time.Unix(1, 0).UTC(),
		}},
	}
}

func testPlan() *TurnPlan {
	return &TurnPlan{
		Operations: []PlannedOperation{
			{Intent: OpIntent{Kind: OpComment, Repo: "kubestellar/hive", Target: "4002", Body: "starting"}},
			{Intent: OpIntent{Kind: OpPush, Repo: "kubestellar/hive", Target: "feat/4002", Body: "abc123"}},
			{Intent: OpIntent{Kind: OpPRCreate, Repo: "kubestellar/hive", Target: "feat/4002", Body: "re-entrant turn spike"}},
			{Intent: OpIntent{Kind: OpLabel, Repo: "kubestellar/hive", Target: "4002", Body: "needs-review"}},
		},
		Verdict:   VerdictShipped,
		Rationale: "prototype completed",
	}
}

func newRunner(remote *fakeRemote, store Persister) *Runner {
	now := time.Unix(100, 0).UTC()
	return &Runner{
		Executor: &JournaledExecutor{
			Sink:       remote,
			Reconciler: remote,
			Persister:  store,
			Now:        func() time.Time { return now },
		},
		Now: func() time.Time { return now },
	}
}

func TestStepReentersAtEveryPersistenceBoundary(t *testing.T) {
	ctx := context.Background()
	controlRemote := newFakeRemote()
	controlStore := &killStore{}
	controlEnv, controlOut, err := newRunner(controlRemote, controlStore).Step(ctx, testEnvelope(), testPlan())
	if err != nil {
		t.Fatalf("control Step: %v", err)
	}
	if !controlOut.Done || controlOut.Verdict != VerdictShipped || controlEnv.Status != StatusCompleted {
		t.Fatalf("control output = %+v, status = %q", controlOut, controlEnv.Status)
	}
	if len(controlRemote.calls) != len(testPlan().Operations) {
		t.Fatalf("control performed %d effects", len(controlRemote.calls))
	}
	wantSummary := controlEnv.Journal.Summary()

	for boundary := 1; boundary <= controlStore.boundary; boundary++ {
		t.Run(fmt.Sprintf("boundary_%02d", boundary), func(t *testing.T) {
			remote := newFakeRemote()
			store := &killStore{killAt: boundary}
			runner := newRunner(remote, store)
			_, _, err := runner.Step(ctx, testEnvelope(), testPlan())
			if !errors.Is(err, errKilled) {
				t.Fatalf("first Step error = %v, want process kill", err)
			}

			recovered := testEnvelope()
			var input = testPlan()
			if store.saved != nil {
				recovered, err = ParseEnvelope(store.saved)
				if err != nil {
					t.Fatalf("parse persisted envelope: %v", err)
				}
				input = nil // all resume state must come from the envelope
			}
			store.killAt = 0
			gotEnv, gotOut, err := runner.Step(ctx, recovered, input)
			if err != nil {
				t.Fatalf("re-enter Step: %v", err)
			}
			if !gotOut.Done || gotOut.Verdict != controlOut.Verdict {
				t.Errorf("output after re-entry = %+v", gotOut)
			}
			if got := gotEnv.Journal.Summary(); got != wantSummary {
				t.Errorf("summary after re-entry:\n%s\nwant:\n%s", got, wantSummary)
			}
			if ambiguous := gotEnv.Journal.Ambiguous(); len(ambiguous) != 0 {
				t.Errorf("ambiguous entries after re-entry: %+v", ambiguous)
			}
			for id, calls := range remote.calls {
				if calls != 1 {
					t.Errorf("effect %s performed %d times", id, calls)
				}
			}
			if len(remote.calls) != len(testPlan().Operations) {
				t.Errorf("performed %d distinct effects, want %d", len(remote.calls), len(testPlan().Operations))
			}
		})
	}
}

func TestAmbiguousOperationFailsClosedWithoutReconciler(t *testing.T) {
	env := testEnvelope()
	plan := bindPlan(env.SessionID, testPlan())
	op := plan.Operations[0]
	env.Journal.recordIntent(op.IdempotencyKey, op.Intent, time.Now())
	remote := newFakeRemote()
	executor := &JournaledExecutor{Sink: remote, Persister: &killStore{}}

	_, err := executor.Do(context.Background(), &env, op)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Do error = %v, want ErrAmbiguous", err)
	}
	if len(remote.calls) != 0 {
		t.Fatal("ambiguous effect was replayed without reconciliation")
	}
}

func TestReconcileNotFoundPerformsEffect(t *testing.T) {
	env := testEnvelope()
	op := bindPlan(env.SessionID, testPlan()).Operations[0]
	env.Journal.recordIntent(op.IdempotencyKey, op.Intent, time.Now())
	remote := newFakeRemote()
	store := &killStore{}
	executor := newRunner(remote, store).Executor

	if _, err := executor.Do(context.Background(), &env, op); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if remote.reconcileCalls != 1 || remote.calls[effectID(op.Intent)] != 1 {
		t.Fatalf("reconcile calls = %d, effect calls = %d", remote.reconcileCalls, remote.calls[effectID(op.Intent)])
	}
}

func TestFailedEffectCanRetry(t *testing.T) {
	env := testEnvelope()
	op := bindPlan(env.SessionID, testPlan()).Operations[0]
	remote := newFakeRemote()
	remote.fail[effectID(op.Intent)] = errors.New("remote unavailable")
	store := &killStore{}
	executor := newRunner(remote, store).Executor

	if _, err := executor.Do(context.Background(), &env, op); err == nil {
		t.Fatal("first Do unexpectedly succeeded")
	}
	entry, _ := env.Journal.Lookup(op.IdempotencyKey)
	if entry.Status != OpFailed {
		t.Fatalf("entry status = %q", entry.Status)
	}
	delete(remote.fail, effectID(op.Intent))
	if _, err := executor.Do(context.Background(), &env, op); err != nil {
		t.Fatalf("retry Do: %v", err)
	}
	entry, _ = env.Journal.Lookup(op.IdempotencyKey)
	if entry.Status != OpSucceeded || entry.Attempts != 2 {
		t.Fatalf("entry after retry = %+v", entry)
	}
}

func TestFileStoreRoundTripIsAtomicPrivateAndScrubbed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "turn.json")
	store := FileStore{Path: path}
	env := testEnvelope()
	env.Messages[0].Content += " token=" + testToken
	env.Messages[0].Metadata = map[string]string{"failure": "credential " + testToken}
	env.Plan = bindPlan(env.SessionID, &TurnPlan{
		Operations: []PlannedOperation{{Intent: OpIntent{Kind: OpPush, Repo: "kubestellar/hive", Target: "branch-" + testToken, Body: "push " + testToken}}},
		Verdict:    VerdictShipped,
		Rationale:  "used " + testToken,
	})
	env.Journal.Entries = []JournalEntry{{
		IdempotencyKey: env.Plan.Operations[0].IdempotencyKey,
		Kind:           OpPush,
		Status:         OpFailed,
		Error:          "remote rejected " + testToken,
		ExternalRef:    "https://x-access-token:" + testToken + "@github.com/o/r",
	}}

	if err := store.Persist(context.Background(), env); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), testToken) || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("persisted artifact was not scrubbed: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".turn-envelope-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Errorf("temporary files after commit = %v, err = %v", matches, err)
	}
	restored, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if restored.Plan.Operations[0].IdempotencyKey != env.Plan.Operations[0].IdempotencyKey {
		t.Fatal("scrubbing changed the bound idempotency key")
	}
	if strings.Contains(env.Messages[0].Content, "[REDACTED]") {
		t.Fatal("persistence mutated the in-memory envelope")
	}
}

func TestValidationAndConfigurationErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := ParseEnvelope([]byte(`{"version":1}`)); err == nil {
		t.Fatal("ParseEnvelope accepted an empty session ID")
	}
	if _, err := ParseEnvelope([]byte(`not-json`)); err == nil {
		t.Fatal("ParseEnvelope accepted invalid JSON")
	}
	if err := (FileStore{}).Persist(ctx, testEnvelope()); err == nil {
		t.Fatal("FileStore accepted an empty path")
	}
	if _, err := (FileStore{}).Load(); err == nil {
		t.Fatal("FileStore Load accepted an empty path")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (FileStore{Path: "unused"}).Persist(cancelled, testEnvelope()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Persist error = %v", err)
	}

	env := testEnvelope()
	if _, _, err := (&Runner{}).Step(ctx, env, testPlan()); err == nil {
		t.Fatal("Runner accepted no executor")
	}
	remote := newFakeRemote()
	if _, _, err := newRunner(remote, &killStore{}).Step(ctx, env, nil); err == nil {
		t.Fatal("Runner accepted no initial plan")
	}
	if _, _, err := (&Runner{Executor: &JournaledExecutor{}}).Step(ctx, env, &TurnPlan{Verdict: VerdictNoWorkNeeded}); err == nil {
		t.Fatal("Runner accepted no persister for an empty plan")
	}
	bound := env.Clone()
	bound.Plan = bindPlan(env.SessionID, testPlan())
	different := testPlan()
	different.Rationale = "changed"
	if _, _, err := newRunner(remote, &killStore{}).Step(ctx, bound, different); err == nil {
		t.Fatal("Runner accepted a changed persisted plan")
	}

	op := bindPlan(env.SessionID, testPlan()).Operations[0]
	for name, executor := range map[string]*JournaledExecutor{
		"nil envelope": {Sink: remote, Persister: &killStore{}},
		"no sink":      {Persister: &killStore{}},
		"no persister": {Sink: remote},
	} {
		t.Run(name, func(t *testing.T) {
			var target *SessionEnvelope
			if name != "nil envelope" {
				copy := env.Clone()
				target = &copy
			}
			if _, err := executor.Do(ctx, target, op); err == nil {
				t.Fatal("Do unexpectedly succeeded")
			}
		})
	}
	badOp := op
	badOp.IdempotencyKey = ""
	if _, err := newRunner(remote, &killStore{}).Executor.Do(ctx, &env, badOp); err == nil {
		t.Fatal("Do accepted an unbound operation")
	}
}

func TestIdempotencyKeySemanticIdentity(t *testing.T) {
	base := OpIntent{Kind: OpComment, Repo: "kubestellar/hive", Target: "4002", Body: "hello", ToolCallID: "call-1"}
	key := DeriveIdempotencyKey("session", base)
	reentered := base
	reentered.ToolCallID = "new-model-call"
	if got := DeriveIdempotencyKey("session", reentered); got != key {
		t.Fatal("tool call ID changed the semantic idempotency key")
	}
	variants := []OpIntent{
		{Kind: OpLabel, Repo: base.Repo, Target: base.Target, Body: base.Body},
		{Kind: base.Kind, Repo: "other/repo", Target: base.Target, Body: base.Body},
		{Kind: base.Kind, Repo: base.Repo, Target: "4003", Body: base.Body},
		{Kind: base.Kind, Repo: base.Repo, Target: base.Target, Body: "goodbye"},
	}
	for _, variant := range variants {
		if DeriveIdempotencyKey("session", variant) == key {
			t.Errorf("variant collided with base: %+v", variant)
		}
	}
	if DeriveIdempotencyKey("other-session", base) == key {
		t.Fatal("different sessions collided")
	}
	input := &TurnPlan{Operations: []PlannedOperation{{IdempotencyKey: "caller-chosen", Intent: base}}}
	if got := bindPlan("session", input).Operations[0].IdempotencyKey; got != key {
		t.Fatalf("bound key = %q, want derived key %q", got, key)
	}
}

package turn

// Error-branch coverage for FileStore, Journal accounting, and the
// JournaledExecutor protocol. These pin the failure paths the happy-path
// suites never enter: input validation, persistence failures at each
// operation boundary, and the settled/unsettled filters in the journal's
// accounting helpers.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- FileStore error branches -----------------------------------------------

func TestFileStorePersistRejectsMissingDirAndSessionID(t *testing.T) {
	ctx := context.Background()

	if err := (FileStore{}).Persist(ctx, SessionEnvelope{SessionID: "s"}); err == nil ||
		!strings.Contains(err.Error(), "FileStore.Dir is required") {
		t.Fatalf("Persist with empty Dir: err = %v, want Dir-required error", err)
	}
	if err := (FileStore{Dir: t.TempDir()}).Persist(ctx, SessionEnvelope{SessionID: "  "}); err == nil ||
		!strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("Persist with blank session id: err = %v, want session_id-required error", err)
	}
}

func TestFileStorePersistAndLoadHonorContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := FileStore{Dir: t.TempDir()}

	if err := store.Persist(ctx, SessionEnvelope{SessionID: "s"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Persist with canceled ctx: err = %v, want context.Canceled", err)
	}
	if _, err := store.Load(ctx, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load with canceled ctx: err = %v, want context.Canceled", err)
	}
}

func TestFileStoreLoadErrorBranches(t *testing.T) {
	ctx := context.Background()

	if _, err := (FileStore{}).Load(ctx, "s"); err == nil ||
		!strings.Contains(err.Error(), "FileStore.Dir is required") {
		t.Fatalf("Load with empty Dir: err = %v, want Dir-required error", err)
	}
	if _, err := (FileStore{Dir: t.TempDir()}).Load(ctx, ""); err == nil ||
		!strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("Load with empty session id: err = %v, want session_id-required error", err)
	}

	store := FileStore{Dir: t.TempDir()}
	if _, err := store.Load(ctx, "absent"); err == nil ||
		!strings.Contains(err.Error(), "reading turn envelope") {
		t.Fatalf("Load of missing envelope: err = %v, want reading error", err)
	}

	corrupt := filepath.Join(store.Dir, "bad.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o660); err != nil {
		t.Fatalf("seeding corrupt envelope: %v", err)
	}
	if _, err := store.Load(ctx, "bad"); err == nil ||
		!strings.Contains(err.Error(), "parsing turn envelope") {
		t.Fatalf("Load of corrupt envelope: err = %v, want parsing error", err)
	}
}

func TestFileStorePersistReportsUnwritableDirectory(t *testing.T) {
	// Dir nested under a regular file: MkdirAll must fail.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o660); err != nil {
		t.Fatalf("seeding blocker file: %v", err)
	}
	store := FileStore{Dir: filepath.Join(blocker, "sub")}
	if err := store.Persist(context.Background(), SessionEnvelope{SessionID: "s"}); err == nil ||
		!strings.Contains(err.Error(), "creating turn envelope directory") {
		t.Fatalf("Persist under file-blocked dir: err = %v, want mkdir error", err)
	}
}

func TestFileStorePersistReportsTempFileCreationFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directory is not enforceable for root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o550); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o770) })
	store := FileStore{Dir: dir}
	if err := store.Persist(context.Background(), SessionEnvelope{SessionID: "s"}); err == nil ||
		!strings.Contains(err.Error(), "creating tmp turn envelope") {
		t.Fatalf("Persist into read-only dir: err = %v, want tmp-create error", err)
	}
}

func TestFileStorePersistReportsRenameFailure(t *testing.T) {
	// A directory squatting on the destination path makes the final
	// atomic rename fail after the tmp file was written successfully.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "s.json"), 0o770); err != nil {
		t.Fatalf("seeding squatter dir: %v", err)
	}
	store := FileStore{Dir: dir}
	if err := store.Persist(context.Background(), SessionEnvelope{SessionID: "s"}); err == nil ||
		!strings.Contains(err.Error(), "renaming tmp turn envelope") {
		t.Fatalf("Persist over squatting dir: err = %v, want rename error", err)
	}
	// The failed tmp file must have been cleaned up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("tmp envelope %q left behind after rename failure", e.Name())
		}
	}
}

// --- Journal accounting filters ---------------------------------------------

func TestJournalEffectCountFiltersByKindAndSettlement(t *testing.T) {
	j := Journal{Entries: []JournalEntry{
		{IdempotencyKey: "a", Kind: OpComment, Status: OpSucceeded},
		{IdempotencyKey: "b", Kind: OpComment, Status: OpIntended},
		{IdempotencyKey: "c", Kind: OpPRCreate, Status: OpSucceeded},
		{IdempotencyKey: "d", Kind: OpPush, Status: OpFailed},
	}}
	if got := j.EffectCount(); got != 2 {
		t.Fatalf("EffectCount() = %d, want 2 (only succeeded entries)", got)
	}
	if got := j.EffectCount(OpComment); got != 1 {
		t.Fatalf("EffectCount(OpComment) = %d, want 1", got)
	}
	if got := j.EffectCount(OpPush); got != 0 {
		t.Fatalf("EffectCount(OpPush) = %d, want 0 (failed entry must not count)", got)
	}
	if got := j.EffectCount(OpComment, OpPRCreate); got != 2 {
		t.Fatalf("EffectCount(OpComment, OpPRCreate) = %d, want 2", got)
	}
}

func TestJournalSummaryListsOnlySettledEffectsSorted(t *testing.T) {
	j := Journal{Entries: []JournalEntry{
		{Kind: OpPush, Repo: "o/r", Target: "main", ExternalRef: "sha2", Status: OpSucceeded},
		{Kind: OpComment, Repo: "o/r", Target: "7", ExternalRef: "c1", Status: OpSucceeded},
		{Kind: OpComment, Repo: "o/r", Target: "8", ExternalRef: "", Status: OpIntended},
		{Kind: OpPRCreate, Repo: "o/r", Target: "b", ExternalRef: "", Status: OpFailed},
	}}
	got := j.Summary()
	want := "comment o/r/7 -> c1\npush o/r/main -> sha2"
	if got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// --- JournaledExecutor error branches ----------------------------------------

type staticSink struct {
	res   EffectResult
	err   error
	calls int
}

func (s *staticSink) Perform(ctx context.Context, in OpIntent) (EffectResult, error) {
	s.calls++
	return s.res, s.err
}

type staticReconciler struct {
	ref   string
	found bool
	err   error
}

func (r staticReconciler) Reconcile(ctx context.Context, in OpIntent) (string, bool, error) {
	return r.ref, r.found, r.err
}

// failAfterPersister succeeds for the first n Persist calls, then fails.
type failAfterPersister struct {
	n     int
	calls int
}

func (p *failAfterPersister) Persist(ctx context.Context, env SessionEnvelope) error {
	p.calls++
	if p.calls > p.n {
		return errors.New("persist boundary failure")
	}
	return nil
}

func intent() OpIntent {
	return OpIntent{Kind: OpComment, Repo: "o/r", Target: "1", Body: "b"}
}

func TestJournaledExecutorDoValidatesInput(t *testing.T) {
	x := &JournaledExecutor{Sink: &staticSink{}}
	ctx := context.Background()

	if _, err := x.Do(ctx, nil, intent()); err == nil ||
		!strings.Contains(err.Error(), "nil envelope") {
		t.Fatalf("Do(nil env): err = %v, want nil-envelope error", err)
	}
	env := SessionEnvelope{SessionID: "s"}
	if _, err := x.Do(ctx, &env, OpIntent{Kind: OpApprovalWait}); err == nil ||
		!strings.Contains(err.Error(), "not a side-effectful op") {
		t.Fatalf("Do(approval_wait): err = %v, want non-side-effectful error", err)
	}
}

func TestJournaledExecutorDoWithoutSinkFailsAfterJournalingIntent(t *testing.T) {
	// No Persister: the nil-Persister persist() short circuit is also pinned.
	x := &JournaledExecutor{}
	env := SessionEnvelope{SessionID: "s"}
	if _, err := x.Do(context.Background(), &env, intent()); err == nil ||
		!strings.Contains(err.Error(), "no EffectSink configured") {
		t.Fatalf("Do without sink: err = %v, want no-sink error", err)
	}
	key := DeriveIdempotencyKey("s", intent())
	if e, ok := env.Journal.Lookup(key); !ok || !e.Ambiguous() {
		t.Fatalf("intent record after no-sink failure = %+v (found %v), want ambiguous entry", e, ok)
	}
}

func TestJournaledExecutorDoReconcileError(t *testing.T) {
	in := intent()
	env := SessionEnvelope{SessionID: "s"}
	env.Journal.RecordIntent(DeriveIdempotencyKey("s", in), in, time.Unix(1, 0))

	x := &JournaledExecutor{
		Sink:       &staticSink{},
		Reconciler: staticReconciler{err: errors.New("remote unavailable")},
	}
	if _, err := x.Do(context.Background(), &env, in); err == nil ||
		!strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("Do with failing reconciler: err = %v, want reconcile error", err)
	}
}

func TestJournaledExecutorDoReconcileFoundPersistFailure(t *testing.T) {
	in := intent()
	env := SessionEnvelope{SessionID: "s"}
	env.Journal.RecordIntent(DeriveIdempotencyKey("s", in), in, time.Unix(1, 0))

	sink := &staticSink{}
	x := &JournaledExecutor{
		Sink:       sink,
		Reconciler: staticReconciler{ref: "existing-ref", found: true},
		Persister:  &failAfterPersister{n: 0},
	}
	if _, err := x.Do(context.Background(), &env, in); err == nil ||
		!strings.Contains(err.Error(), "persist boundary failure") {
		t.Fatalf("Do with reconcile-found + failing persist: err = %v, want persist error", err)
	}
	if sink.calls != 0 {
		t.Fatalf("sink performed %d times after reconcile found the effect, want 0", sink.calls)
	}
}

func TestJournaledExecutorDoIntentPersistFailureBlocksEffect(t *testing.T) {
	sink := &staticSink{res: EffectResult{ExternalRef: "ref"}}
	x := &JournaledExecutor{Sink: sink, Persister: &failAfterPersister{n: 0}}
	env := SessionEnvelope{SessionID: "s"}
	if _, err := x.Do(context.Background(), &env, intent()); err == nil ||
		!strings.Contains(err.Error(), "persist boundary failure") {
		t.Fatalf("Do with failing intent persist: err = %v, want persist error", err)
	}
	if sink.calls != 0 {
		t.Fatalf("effect performed %d times despite unpersisted intent, want 0", sink.calls)
	}
}

func TestJournaledExecutorDoSinkFailureSettlesFailed(t *testing.T) {
	in := intent()
	env := SessionEnvelope{SessionID: "s"}
	x := &JournaledExecutor{
		Sink:      &staticSink{err: errors.New("remote write failed")},
		Persister: &failAfterPersister{n: 99},
	}
	if _, err := x.Do(context.Background(), &env, in); err == nil ||
		!strings.Contains(err.Error(), "remote write failed") {
		t.Fatalf("Do with failing sink: err = %v, want sink error", err)
	}
	e, ok := env.Journal.Lookup(DeriveIdempotencyKey("s", in))
	if !ok || e.Status != OpFailed || e.Error != "remote write failed" {
		t.Fatalf("journal after sink failure = %+v (found %v), want OpFailed with error recorded", e, ok)
	}
}

func TestJournaledExecutorDoSinkFailurePersistFailureSurfacesPersistError(t *testing.T) {
	// Intent persist succeeds (call 1), the settle persist after the sink
	// failure does not (call 2): the persistence error must win.
	x := &JournaledExecutor{
		Sink:      &staticSink{err: errors.New("remote write failed")},
		Persister: &failAfterPersister{n: 1},
	}
	env := SessionEnvelope{SessionID: "s"}
	if _, err := x.Do(context.Background(), &env, intent()); err == nil ||
		!strings.Contains(err.Error(), "persist boundary failure") {
		t.Fatalf("Do with sink+persist failure: err = %v, want persist error", err)
	}
}

func TestJournaledExecutorDoSettlePersistFailureAfterSuccess(t *testing.T) {
	x := &JournaledExecutor{
		Sink:      &staticSink{res: EffectResult{ExternalRef: "ref"}},
		Persister: &failAfterPersister{n: 1},
	}
	env := SessionEnvelope{SessionID: "s"}
	if _, err := x.Do(context.Background(), &env, intent()); err == nil ||
		!strings.Contains(err.Error(), "persist boundary failure") {
		t.Fatalf("Do with post-success persist failure: err = %v, want persist error", err)
	}
}

func TestSuspendForApprovalErrorAndIdempotentBranches(t *testing.T) {
	ctx := context.Background()
	x := &JournaledExecutor{}

	if _, err := x.SuspendForApproval(ctx, nil, OpIntent{}); err == nil ||
		!strings.Contains(err.Error(), "nil envelope") {
		t.Fatalf("SuspendForApproval(nil env): err = %v, want nil-envelope error", err)
	}

	// Persist failure while journaling the wait.
	failing := &JournaledExecutor{Persister: &failAfterPersister{n: 0}}
	env := SessionEnvelope{SessionID: "s"}
	if _, err := failing.SuspendForApproval(ctx, &env, OpIntent{Repo: "o/r", Target: "1"}); err == nil ||
		!strings.Contains(err.Error(), "persist boundary failure") {
		t.Fatalf("SuspendForApproval with failing persist: err = %v, want persist error", err)
	}

	// Second suspend for the same position returns the prior entry untouched.
	env2 := SessionEnvelope{SessionID: "s"}
	first, err := x.SuspendForApproval(ctx, &env2, OpIntent{Repo: "o/r", Target: "1"})
	if err != nil {
		t.Fatalf("first SuspendForApproval: %v", err)
	}
	second, err := x.SuspendForApproval(ctx, &env2, OpIntent{Repo: "o/r", Target: "1"})
	if err != nil {
		t.Fatalf("second SuspendForApproval: %v", err)
	}
	if second.IdempotencyKey != first.IdempotencyKey || second.Attempts != first.Attempts {
		t.Fatalf("repeat suspend = %+v, want prior entry %+v returned unchanged", second, first)
	}
	if n := len(env2.Journal.Entries); n != 1 {
		t.Fatalf("journal has %d entries after repeated suspend, want 1", n)
	}
}

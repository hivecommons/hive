package outcome

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

const maintainer = "clubanderson"

func testLedger(t *testing.T, opts Options) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outcomes.json")
	if opts.Principals == nil {
		opts.Principals = []string{maintainer}
	}
	l, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l, path
}

func ref(project, repo, name string) Ref {
	return Ref{Project: project, Repo: repo, Outcome: name}
}

// ── Identity guard rows ─────────────────────────────────────────────────────

// Two outcomes sharing one repository stay distinct.
func TestRefKey_TwoOutcomesOneRepoDistinct(t *testing.T) {
	a := ref("default", "kubestellar/hive", "nightly-green").Key()
	b := ref("default", "kubestellar/hive", "docs-current").Key()
	if a == b || a == "" || b == "" {
		t.Fatalf("keys collided or empty: %q vs %q", a, b)
	}
	if a != "default/kubestellar/hive@nightly-green" {
		t.Fatalf("canonical key form changed: %q", a)
	}
}

// The same outcome-local name in two accepted scopes stays distinct.
func TestRefKey_SameNameTwoScopesDistinct(t *testing.T) {
	byRepo := ref("default", "kubestellar/hive", "nightly-green").Key()
	otherRepo := ref("default", "kubestellar/console", "nightly-green").Key()
	otherProject := ref("staging", "kubestellar/hive", "nightly-green").Key()
	if byRepo == otherRepo || byRepo == otherProject {
		t.Fatalf("scope did not disambiguate: %q %q %q", byRepo, otherRepo, otherProject)
	}
}

// Outcome keys can never be mistaken for persisted work keys: "@" is not a
// worksource separator, so ParseKey refuses every canonical outcome key, and
// legacy GitHub work keys are untouched by construction.
func TestRefKey_CollisionFreeAgainstWorkKeys(t *testing.T) {
	key := ref("default", "kubestellar/hive", "nightly-green").Key()
	if _, ok := worksource.ParseKey(key); ok {
		t.Fatalf("worksource.ParseKey accepted an outcome key %q — namespace collision", key)
	}
	// And the reverse: a work key is not a valid outcome ref dimension set.
	if got := (Ref{Project: "default", Repo: "kubestellar/hive#42", Outcome: "x"}).Key(); got != "" {
		t.Fatalf("a '#'-bearing repo produced an outcome key %q", got)
	}
}

func TestRefValidate_RefusesAmbiguousDimensions(t *testing.T) {
	bad := []Ref{
		{},
		{Project: "default", Repo: "o/r"}, // missing outcome
		{Project: "a/b", Repo: "o/r", Outcome: "x"},            // '/' in project
		{Project: "default", Repo: "o/r", Outcome: "Bad Slug"}, // not a slug
		{Project: "default", Repo: "o/r", Outcome: "x@y"},      // '@' in slug
		{Project: "default", Repo: "o@r", Outcome: "x"},        // '@' in repo
		{Project: "def ault", Repo: "o/r", Outcome: "x"},       // whitespace
		{Project: "default", Repo: "o!r", Outcome: "x"},        // '!' in repo
		{Project: "default", Repo: "o/r", Outcome: "-leading"}, // slug edge
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Fatalf("Validate accepted %+v", r)
		}
		if r.Key() != "" {
			t.Fatalf("invalid ref produced key %q", r.Key())
		}
	}
}

// ── Lifecycle and mutator guard rows ────────────────────────────────────────

func TestLedger_CreateAcceptSupersedeLifecycle(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "kubestellar/hive", "nightly-green")

	rec, err := l.Create(r, "nightly workflow green 7 days", []string{"kubestellar/hive#42"}, maintainer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.Generation != 1 || rec.State != StateProposed {
		t.Fatalf("create produced %+v, want generation 1 proposed", rec)
	}
	if rec.WorkRefs[0] != "kubestellar/hive#42" {
		t.Fatalf("legacy work key rewritten: %v", rec.WorkRefs)
	}

	rec, err = l.Accept(r, 1, maintainer)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if rec.State != StateAccepted || rec.Generation != 1 {
		t.Fatalf("accept produced %+v", rec)
	}
	last := rec.History[len(rec.History)-1]
	if last.Verb != "accept" || last.Actor != maintainer || last.Date.IsZero() {
		t.Fatalf("acceptance receipt missing actor/date: %+v", last)
	}

	rec, err = l.Supersede(r, 1, "green 14 days", nil, maintainer)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if rec.Generation != 2 || rec.State != StateProposed {
		t.Fatalf("supersede produced %+v, want generation 2 proposed", rec)
	}
	// The prior generation stays visible in history with its spec snapshot.
	var sawGen1 bool
	for _, tr := range rec.History {
		if tr.Generation == 1 && tr.Spec == "nightly workflow green 7 days" {
			sawGen1 = true
		}
	}
	if !sawGen1 {
		t.Fatalf("superseded generation 1 lost from history: %+v", rec.History)
	}

	if _, err := l.Retire(r, 2, maintainer); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if _, err := l.Supersede(r, 2, "x", nil, maintainer); !errors.Is(err, ErrOutcomeRetired) {
		t.Fatalf("mutating a retired outcome: err = %v, want ErrOutcomeRetired", err)
	}
}

// Only the accepted mutator principals may advance desired state. An agent,
// observer, or lifecycle actor is refused before any state is read.
func TestLedger_UnauthorizedActorRefused(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "o/r", "x")
	if _, err := l.Create(r, "s", nil, "some-agent"); !errors.Is(err, ErrUnauthorizedActor) {
		t.Fatalf("create by agent: %v", err)
	}
	if _, err := l.Create(r, "s", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, err := range []error{
		func() error { _, e := l.Accept(r, 1, "bead-lifecycle"); return e }(),
		func() error { _, e := l.Supersede(r, 1, "s2", nil, "webhook"); return e }(),
		func() error { _, e := l.Retire(r, 1, "admission-sweep"); return e }(),
	} {
		if !errors.Is(err, ErrUnauthorizedActor) {
			t.Fatalf("unauthorized mutation allowed: %v", err)
		}
	}
	// No principals configured means no mutation at all — safe default.
	empty, _ := testLedger(t, Options{Principals: []string{}})
	empty.principals = map[string]bool{}
	if _, err := empty.Create(r, "s", nil, maintainer); !errors.Is(err, ErrUnauthorizedActor) {
		t.Fatalf("empty principal set allowed a mutation: %v", err)
	}
}

// Two acceptance writers race: CAS on the expected generation serializes
// them; the stale writer is refused/fenced, never merged.
func TestLedger_CASConflictRejected(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "o/r", "x")
	if _, err := l.Create(r, "g1", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Supersede(r, 1, "g2", nil, maintainer); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	// A writer still holding generation 1 is fenced on every verb.
	if _, err := l.Accept(r, 1, maintainer); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale accept: %v", err)
	}
	if _, err := l.Supersede(r, 1, "g3", nil, maintainer); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale supersede: %v", err)
	}
	if _, err := l.Retire(r, 1, maintainer); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale retire: %v", err)
	}
	rec, _ := l.Get(r)
	if rec.Generation != 2 || rec.State != StateProposed {
		t.Fatalf("fenced writers mutated state: %+v", rec)
	}
}

// Concurrent racers: exactly one of N supersedes naming the same expected
// generation wins; run under -race this also proves the lock discipline.
func TestLedger_ConcurrentCASExactlyOneWinner(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "o/r", "x")
	if _, err := l.Create(r, "g1", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.Supersede(r, 1, "g2", nil, maintainer)
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("unexpected racer error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
	rec, _ := l.Get(r)
	if rec.Generation != 2 {
		t.Fatalf("generation = %d after race, want 2 (monotonic, no double advance)", rec.Generation)
	}
}

// ── Durability guard rows ───────────────────────────────────────────────────

// Hive restarts: the same desired generation reconstructs from the file; no
// process-only counter is truth.
func TestLedger_RestartReconstructsDesiredGeneration(t *testing.T) {
	l, path := testLedger(t, Options{})
	r := ref("default", "kubestellar/hive", "nightly-green")
	if _, err := l.Create(r, "g1", []string{"kubestellar/hive#42"}, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Accept(r, 1, maintainer); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := l.Supersede(r, 1, "g2", nil, maintainer); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	reloaded, err := Open(path, Options{Principals: []string{maintainer}})
	if err != nil {
		t.Fatalf("reload Open: %v", err)
	}
	rec, ok := reloaded.Get(r)
	if !ok {
		t.Fatalf("record lost across restart")
	}
	if rec.Generation != 2 || rec.State != StateProposed || rec.Spec != "g2" {
		t.Fatalf("restart reconstructed %+v, want generation 2 proposed g2", rec)
	}
	if len(rec.History) != 3 {
		t.Fatalf("history lost across restart: %+v", rec.History)
	}
	if rec.WorkRefs[0] != "kubestellar/hive#42" {
		t.Fatalf("work refs lost across restart: %v", rec.WorkRefs)
	}
	// And CAS discipline survives: the pre-restart generation still fences.
	if _, err := reloaded.Accept(r, 1, maintainer); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("restart forgot the current generation: %v", err)
	}
}

// A missing file is a fresh ledger; a CORRUPT file is a visible refusal that
// leaves the bytes untouched and can never invent state.
func TestLedger_CorruptFileRefusedAndPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outcomes.json")
	corrupt := []byte("{ not json")
	if err := os.WriteFile(path, corrupt, 0o660); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Open(path, Options{Principals: []string{maintainer}}); err == nil {
		t.Fatalf("Open accepted a corrupt ledger")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(corrupt) {
		t.Fatalf("corrupt bytes were not preserved: %q %v", got, err)
	}

	// Conflicting records (duplicate keys) are equally refused.
	dup := `{"version":1,"records":[
	  {"ref":{"project":"default","repo":"o/r","outcome":"x"},"generation":1,"state":"proposed","spec":"a","history":[]},
	  {"ref":{"project":"default","repo":"o/r","outcome":"x"},"generation":2,"state":"accepted","spec":"b","history":[]}]}`
	dupPath := filepath.Join(dir, "dup.json")
	if err := os.WriteFile(dupPath, []byte(dup), 0o660); err != nil {
		t.Fatalf("seed dup: %v", err)
	}
	if _, err := Open(dupPath, Options{}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting records not refused: %v", err)
	}

	// An impossible generation is refused rather than trusted.
	zero := `{"version":1,"records":[{"ref":{"project":"default","repo":"o/r","outcome":"x"},"generation":0,"state":"proposed","spec":"a","history":[]}]}`
	zeroPath := filepath.Join(dir, "zero.json")
	if err := os.WriteFile(zeroPath, []byte(zero), 0o660); err != nil {
		t.Fatalf("seed zero: %v", err)
	}
	if _, err := Open(zeroPath, Options{}); err == nil {
		t.Fatalf("generation 0 record not refused")
	}
}

// The persisted write is atomic: after any mutation no *.tmp remains, and the
// file parses as the deliberate versioned format.
func TestLedger_AtomicPersistShape(t *testing.T) {
	l, path := testLedger(t, Options{})
	if _, err := l.Create(ref("default", "o/r", "x"), "s", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp file left behind: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("persisted format missing version: %s", data)
	}
}

// ── Mutator-isolation guard rows ────────────────────────────────────────────

// An event, observation, or bead closing/retiring cannot change desired
// generation: the ledger exposes no verb for them, readers get detached
// copies, and mutating a returned copy or listed slice touches nothing.
func TestLedger_ReadersHoldDetachedCopies(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "o/r", "x")
	if _, err := l.Create(r, "s", []string{"o/r#1"}, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _ := l.Get(r)
	got.Generation = 99
	got.State = StateRetired
	got.WorkRefs[0] = "o/r#999"
	got.History[0].Actor = "attacker"

	fresh, _ := l.Get(r)
	if fresh.Generation != 1 || fresh.State != StateProposed ||
		fresh.WorkRefs[0] != "o/r#1" || fresh.History[0].Actor != maintainer {
		t.Fatalf("reader mutation leaked into the ledger: %+v", fresh)
	}
	list := l.List()
	if len(list) != 1 || list[0].Generation != 1 {
		t.Fatalf("List = %+v", list)
	}
}

// Invalid work refs (including the fabricated "#0" identity) are refused.
func TestLedger_InvalidWorkRefsRefused(t *testing.T) {
	l, _ := testLedger(t, Options{})
	for _, bad := range []string{"", "o/r#0", "not-a-key", "#5"} {
		if _, err := l.Create(ref("default", "o/r", "x"), "s", []string{bad}, maintainer); err == nil {
			t.Fatalf("work ref %q accepted", bad)
		}
	}
}

// The GitHub mirror is write-only exhaust: every durable mutation emits one
// receipt AFTER persistence, nothing is ever read back, and no mirror is
// consulted on Open/Get.
func TestLedger_MirrorIsWriteOnlyExhaust(t *testing.T) {
	var receipts []Receipt
	fixed := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	l, path := testLedger(t, Options{
		Mirror: func(rcpt Receipt) { receipts = append(receipts, rcpt) },
		Now:    func() time.Time { return fixed },
	})
	r := ref("default", "kubestellar/hive", "nightly-green")
	if _, err := l.Create(r, "g1", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Accept(r, 1, maintainer); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := l.Accept(r, 5, maintainer); err == nil {
		t.Fatalf("stale accept succeeded")
	}
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2 (refused mutations emit nothing)", len(receipts))
	}
	if receipts[1].Verb != "accept" || receipts[1].Generation != 1 ||
		receipts[1].Actor != maintainer || !receipts[1].Date.Equal(fixed) ||
		receipts[1].Key != r.Key() {
		t.Fatalf("receipt = %+v", receipts[1])
	}
	// Reload without any mirror: state is identical, proving nothing was ever
	// read back from the mirror side.
	reloaded, err := Open(path, Options{Principals: []string{maintainer}})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	rec, _ := reloaded.Get(r)
	if rec.State != StateAccepted || rec.Generation != 1 {
		t.Fatalf("mirror-free reload diverged: %+v", rec)
	}
}

func TestLedger_VerbsOnUndeclaredOutcome(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "o/r", "ghost")
	if _, err := l.Accept(r, 1, maintainer); !errors.Is(err, ErrOutcomeNotFound) {
		t.Fatalf("accept ghost: %v", err)
	}
	if _, ok := l.Get(r); ok {
		t.Fatalf("ghost outcome materialised")
	}
	if _, ok := l.GetByKey(""); ok {
		t.Fatalf("empty key resolved")
	}
	if _, err := l.Create(r, "s", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Create(r, "s", nil, maintainer); !errors.Is(err, ErrOutcomeExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	if _, err := l.Accept(r, 1, maintainer); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := l.Accept(r, 1, maintainer); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double accept: %v", err)
	}
}

// A failed persist must leave memory and disk agreeing: the in-memory install
// is rolled back, so a later reader cannot see a generation the file never
// held (no process-only counter becomes truth even transiently).
func TestLedger_PersistFailureRollsBackMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outcomes.json")
	l, err := Open(path, Options{Principals: []string{maintainer}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := ref("default", "o/r", "x")
	if _, err := l.Create(r, "g1", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Make the directory unwritable so the tmp write fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := l.Supersede(r, 1, "g2", nil, maintainer); err == nil {
		t.Fatalf("supersede persisted into an unwritable directory")
	}
	rec, _ := l.Get(r)
	if rec.Generation != 1 || rec.Spec != "g1" {
		t.Fatalf("failed persist leaked into memory: %+v", rec)
	}

	// And a failed CREATE must not leave a phantom record behind.
	if _, err := l.Create(ref("default", "o/r", "y"), "s", nil, maintainer); err == nil {
		t.Fatalf("create persisted into an unwritable directory")
	}
	if _, ok := l.Get(ref("default", "o/r", "y")); ok {
		t.Fatalf("failed create left a phantom record")
	}

	// Recovery: once writable again, the fenced state advances normally.
	_ = os.Chmod(dir, 0o700)
	if _, err := l.Supersede(r, 1, "g2", nil, maintainer); err != nil {
		t.Fatalf("recovered supersede: %v", err)
	}
}

func TestLedger_SupersedeValidatesAndReplacesWorkRefs(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "o/r", "x")
	if _, err := l.Create(r, "g1", []string{"o/r#1"}, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Supersede(r, 1, "g2", []string{"o/r#0"}, maintainer); err == nil {
		t.Fatalf("supersede accepted a fabricated work ref")
	}
	rec, err := l.Supersede(r, 1, "g2", []string{"o/r#2", "o/r!ENG-9"}, maintainer)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if len(rec.WorkRefs) != 2 || rec.WorkRefs[0] != "o/r#2" {
		t.Fatalf("work refs = %v", rec.WorkRefs)
	}
	// Superseding with no refs keeps the existing links.
	rec, err = l.Supersede(r, 2, "g3", nil, maintainer)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if len(rec.WorkRefs) != 2 {
		t.Fatalf("empty supersede dropped work refs: %v", rec.WorkRefs)
	}
}

func TestLedger_MutationWithInvalidRefRefused(t *testing.T) {
	l, _ := testLedger(t, Options{})
	if _, err := l.Accept(Ref{}, 1, maintainer); err == nil {
		t.Fatalf("accept on an invalid ref succeeded")
	}
}

func TestLedger_ListIsSortedByKey(t *testing.T) {
	l, _ := testLedger(t, Options{})
	if _, err := l.Create(ref("default", "o/r", "zeta"), "s", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Create(ref("default", "o/r", "alpha"), "s", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	list := l.List()
	if len(list) != 2 || list[0].Ref.Outcome != "alpha" || list[1].Ref.Outcome != "zeta" {
		t.Fatalf("List order = %+v", list)
	}
}

// A record persisted with an invalid ref is refused at Open — the file cannot
// smuggle an unidentifiable outcome past declaration-time validation.
func TestLedger_OpenRefusesInvalidPersistedRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	bad := `{"version":1,"records":[{"ref":{"project":"","repo":"o/r","outcome":"x"},"generation":1,"state":"proposed","spec":"a","history":[]}]}`
	if err := os.WriteFile(path, []byte(bad), 0o660); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatalf("invalid persisted ref accepted")
	}
}

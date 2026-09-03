package proof

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence"
)

const (
	headX = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headY = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	baseA = "cccccccccccccccccccccccccccccccccccccccc"
	baseB = "dddddddddddddddddddddddddddddddddddddddd"
)

var testObservedAt = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func testFingerprint() Fingerprint {
	return Fingerprint{
		OutcomeKey:        "default/hivecommons/hive@ship-proof-vertical",
		PredicateID:       PredicateExactHeadGreen,
		DesiredGeneration: 3,
		Repo:              "hivecommons/hive",
		PRNumber:          4253,
		HeadSHA:           headX,
		BaseSHA:           baseA,
		CheckPolicyID:     CheckPolicyID("meta-v1", []string{"build", "test"}),
		Producer:          ProducerGitHubChecksAPI,
	}
}

func testRecord() Record {
	return Record{
		Fingerprint: testFingerprint(),
		Result:      ResultSuccess,
		Provenance:  Provenance{CheckRunIDs: []int64{11, 22}, Query: "ListCheckRunsForRef@" + headX},
		ObservedAt:  testObservedAt,
	}
}

func testContext() Context {
	return Context{Required: testFingerprint(), Now: testObservedAt.Add(time.Minute), MaxAge: time.Hour}
}

// ─── Fingerprint identity ───────────────────────────────────────────────────

func TestFingerprintKeyBindsLoadBearingFields(t *testing.T) {
	fp := testFingerprint()
	key := fp.Key()
	want := fp.OutcomeKey + "|" + PredicateExactHeadGreen + "|3|" + headX
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
	if rec := testRecord(); rec.Key() != key {
		t.Fatalf("record key %q != fingerprint key %q", rec.Key(), key)
	}
}

func TestFingerprintValidateRejectsEveryMissingOrBogusField(t *testing.T) {
	mutations := map[string]func(*Fingerprint){
		"empty outcome key":     func(f *Fingerprint) { f.OutcomeKey = "" },
		"separator in outcome":  func(f *Fingerprint) { f.OutcomeKey = "a|b" },
		"foreign predicate":     func(f *Fingerprint) { f.PredicateID = "github.review.aggregate/v1" },
		"zero generation":       func(f *Fingerprint) { f.DesiredGeneration = 0 },
		"empty repo":            func(f *Fingerprint) { f.Repo = "" },
		"repo without owner":    func(f *Fingerprint) { f.Repo = "hive" },
		"zero pr":               func(f *Fingerprint) { f.PRNumber = 0 },
		"abbreviated head sha":  func(f *Fingerprint) { f.HeadSHA = "abc123" },
		"uppercase head sha":    func(f *Fingerprint) { f.HeadSHA = strings.ToUpper(headX) },
		"abbreviated base sha":  func(f *Fingerprint) { f.BaseSHA = "def456" },
		"empty check policy":    func(f *Fingerprint) { f.CheckPolicyID = "" },
		"separator in policy":   func(f *Fingerprint) { f.CheckPolicyID = "sha256:x|y" },
		"unaccepted producer":   func(f *Fingerprint) { f.Producer = "other-vendor-checks" },
		"whitespace in outcome": func(f *Fingerprint) { f.OutcomeKey = "a b" },
		"reserved char in repo": func(f *Fingerprint) { f.Repo = "kubestellar/hi@ve" },
		"negative generation":   func(f *Fingerprint) { f.DesiredGeneration = -1 },
		"empty head sha":        func(f *Fingerprint) { f.HeadSHA = "" },
		"empty base sha":        func(f *Fingerprint) { f.BaseSHA = "" },
	}
	for name, mutate := range mutations {
		fp := testFingerprint()
		mutate(&fp)
		if err := fp.Validate(); err == nil {
			t.Errorf("%s: Validate accepted a malformed fingerprint", name)
		}
		if key := fp.Key(); key != "" {
			t.Errorf("%s: malformed fingerprint produced key %q; must be \"\"", name, key)
		}
	}
}

func TestCheckPolicyIDIsOrderInsensitiveButNameAndVersionSensitive(t *testing.T) {
	a := CheckPolicyID("meta-v1", []string{"build", "test"})
	b := CheckPolicyID("meta-v1", []string{"test", "build"})
	if a != b {
		t.Fatalf("policy id must not depend on name order: %s vs %s", a, b)
	}
	if CheckPolicyID("meta-v2", []string{"build", "test"}) == a {
		t.Fatal("classifier version change must change the policy id")
	}
	if CheckPolicyID("meta-v1", []string{"build", "test", "lint"}) == a {
		t.Fatal("check-set change must change the policy id")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("policy id %q is not a bounded sha256 handle", a)
	}
	// The digest boundary is unambiguous: moving a byte between the two
	// inputs must not collide.
	if CheckPolicyID("meta-v1x", []string{"build"}) == CheckPolicyID("meta-v1", []string{"xbuild"}) {
		t.Fatal("version/name boundary must be delimited")
	}
}

// ─── Evidence normalization ─────────────────────────────────────────────────

func TestNormalizeCheckEvidenceClassification(t *testing.T) {
	fp := testFingerprint()
	cases := []struct {
		name   string
		checks []CheckConclusion
		want   Result
	}{
		{"all green", []CheckConclusion{
			{ID: 2, Name: "test", Completed: true, Conclusion: "success"},
			{ID: 1, Name: "build", Completed: true, Conclusion: "success"},
		}, ResultSuccess},
		{"one failure", []CheckConclusion{
			{ID: 1, Name: "build", Completed: true, Conclusion: "success"},
			{ID: 2, Name: "test", Completed: true, Conclusion: "failure"},
		}, ResultFailure},
		{"action_required is failure", []CheckConclusion{
			{ID: 1, Name: "build", Completed: true, Conclusion: "action_required"},
		}, ResultFailure},
		{"incomplete is pending", []CheckConclusion{
			{ID: 1, Name: "build", Completed: true, Conclusion: "success"},
			{ID: 2, Name: "test", Completed: false},
		}, ResultPending},
		{"failure beats pending", []CheckConclusion{
			{ID: 1, Name: "build", Completed: true, Conclusion: "failure"},
			{ID: 2, Name: "test", Completed: false},
		}, ResultFailure},
		{"no evidence is never success", nil, ResultPending},
	}
	for _, tc := range cases {
		rec, err := NormalizeCheckEvidence(fp, tc.checks, testObservedAt)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rec.Result != tc.want {
			t.Errorf("%s: result = %s, want %s", tc.name, rec.Result, tc.want)
		}
		if rec.Provenance.Query != "ListCheckRunsForRef@"+headX {
			t.Errorf("%s: provenance query %q does not name the observation call", tc.name, rec.Provenance.Query)
		}
	}
	// Provenance IDs are sorted for determinism.
	rec, err := NormalizeCheckEvidence(fp, cases[0].checks, testObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Provenance.CheckRunIDs[0] != 1 || rec.Provenance.CheckRunIDs[1] != 2 {
		t.Fatalf("provenance ids %v are not sorted", rec.Provenance.CheckRunIDs)
	}
}

func TestNormalizeCheckEvidenceRefusesMalformedFingerprint(t *testing.T) {
	fp := testFingerprint()
	fp.HeadSHA = "short"
	if _, err := NormalizeCheckEvidence(fp, nil, testObservedAt); err == nil {
		t.Fatal("malformed fingerprint must never normalize into a receipt")
	}
	if _, err := NormalizeCheckEvidence(testFingerprint(), nil, time.Time{}); err == nil {
		t.Fatal("a receipt without an observation instant must be refused")
	}
}

// ─── Verification: the fingerprint-match truth table ────────────────────────

func TestVerifyMatchingCurrentProofSatisfies(t *testing.T) {
	v := testRecord().VerifyAgainst(testContext())
	if !v.Satisfied() || v.Status != convergence.ConditionTrue || v.Reason != ReasonProofCurrent {
		t.Fatalf("complete fresh matching success must satisfy: %+v", v)
	}
}

// TestVerifyEachFieldMismatchRejects is the core RED matrix: every single
// load-bearing fingerprint field, when it differs from the current context,
// must independently prevent satisfaction — removing any one of these checks
// fails this test.
func TestVerifyEachFieldMismatchRejects(t *testing.T) {
	mutations := map[string]func(*Fingerprint){
		"outcome key":        func(f *Fingerprint) { f.OutcomeKey = "default/hivecommons/hive@other-outcome" },
		"desired generation": func(f *Fingerprint) { f.DesiredGeneration = 4 },
		"repo":               func(f *Fingerprint) { f.Repo = "kubestellar/other" },
		"pr number":          func(f *Fingerprint) { f.PRNumber = 4254 },
		"head sha (X vs Y)":  func(f *Fingerprint) { f.HeadSHA = headY },
		"base sha":           func(f *Fingerprint) { f.BaseSHA = baseB },
		"check policy":       func(f *Fingerprint) { f.CheckPolicyID = CheckPolicyID("meta-v2", []string{"build", "test"}) },
	}
	for name, mutate := range mutations {
		ctx := testContext()
		mutate(&ctx.Required) // the WORLD moved; the receipt did not
		v := testRecord().VerifyAgainst(ctx)
		if v.Satisfied() {
			t.Errorf("%s mismatch still satisfied: %+v", name, v)
			continue
		}
		if v.Status != convergence.ConditionUnknown || v.Reason != ReasonFingerprintMismatch {
			t.Errorf("%s mismatch must be Unknown/FingerprintMismatch, got %+v", name, v)
		}
	}
}

func TestVerifyRepairedHeadNeverSatisfiedByOldProof(t *testing.T) {
	// Proof was recorded for head X; the PR was repaired to head Y. The
	// current context now requires Y.
	ctx := testContext()
	ctx.Required.HeadSHA = headY
	v := testRecord().VerifyAgainst(ctx)
	if v.Satisfied() {
		t.Fatal("evidence for head X authorized repaired head Y")
	}
	if !strings.Contains(v.Detail, headY) || !strings.Contains(v.Detail, headX) {
		t.Fatalf("mismatch detail must name both heads: %q", v.Detail)
	}
}

func TestVerifyGenerationSupersessionInvalidates(t *testing.T) {
	ctx := testContext()
	ctx.Required.DesiredGeneration = 4 // G → G+1
	if v := testRecord().VerifyAgainst(ctx); v.Satisfied() {
		t.Fatal("generation-3 receipt satisfied generation 4")
	}
}

func TestVerifyStaleProofExpires(t *testing.T) {
	ctx := testContext()
	ctx.Now = testObservedAt.Add(2 * time.Hour) // MaxAge is 1h
	v := testRecord().VerifyAgainst(ctx)
	if v.Satisfied() || v.Reason != ReasonProofExpired || v.Status != convergence.ConditionUnknown {
		t.Fatalf("stale proof must be Unknown/ProofExpired: %+v", v)
	}
	// Zero MaxAge is the conservative direction: nothing is ever fresh.
	ctx = testContext()
	ctx.MaxAge = 0
	if v := testRecord().VerifyAgainst(ctx); v.Satisfied() {
		t.Fatal("zero freshness window still satisfied")
	}
}

func TestVerifyResultMapping(t *testing.T) {
	rec := testRecord()
	rec.Result = ResultFailure
	if v := rec.VerifyAgainst(testContext()); v.Status != convergence.ConditionFalse || v.Reason != ReasonPredicateFailed {
		t.Fatalf("failure evidence must be a definite False: %+v", v)
	}
	rec.Result = ResultPending
	if v := rec.VerifyAgainst(testContext()); v.Status != convergence.ConditionUnknown || v.Reason != ReasonEvidencePending {
		t.Fatalf("pending evidence must be Unknown: %+v", v)
	}
}

func TestVerifyMalformedEvidenceNeverSatisfies(t *testing.T) {
	rec := testRecord()
	rec.Result = "greenish" // conflicting/nonsense classification
	if v := rec.VerifyAgainst(testContext()); v.Satisfied() || v.Reason != ReasonProofMalformed {
		t.Fatalf("malformed evidence must be Unknown/ProofMalformed: %+v", v)
	}
	rec = testRecord()
	rec.ObservedAt = time.Time{}
	if v := rec.VerifyAgainst(testContext()); v.Satisfied() {
		t.Fatal("a receipt without an observation instant satisfied")
	}
	// A malformed REQUIRED context also establishes nothing.
	ctx := testContext()
	ctx.Required.HeadSHA = "not-a-sha"
	if v := testRecord().VerifyAgainst(ctx); v.Satisfied() {
		t.Fatal("a malformed required fingerprint satisfied")
	}
}

func TestVerifyLookupMissesAndNilStore(t *testing.T) {
	if v := Verify(nil, testContext()); v.Satisfied() || v.Reason != ReasonProofMissing {
		t.Fatalf("nil store must be Unknown/ProofMissing: %+v", v)
	}
	s := openTestStore(t)
	if v := Verify(s, testContext()); v.Satisfied() || v.Reason != ReasonProofMissing {
		t.Fatalf("empty store must be Unknown/ProofMissing: %+v", v)
	}
	if _, err := s.Put(testRecord()); err != nil {
		t.Fatal(err)
	}
	if v := Verify(s, testContext()); !v.Satisfied() {
		t.Fatalf("stored matching proof must satisfy through Verify: %+v", v)
	}
}

// ─── Production-path admission gating through convergence.Evaluate ──────────

// observationWithProof is the production shape: the one candidate explicitly
// declared to depend on the proof carries the projected Dependency edge.
func observationWithProof(dep *convergence.Dependency) convergence.Observation {
	obs := convergence.Observation{
		Subject:  convergence.Subject{Repo: "hivecommons/hive", Number: 4253},
		Found:    true,
		RecordID: "bead-4253",
	}
	if dep != nil {
		obs.Dependencies = append(obs.Dependencies, *dep)
	}
	return obs
}

func TestEnforceModeGatesTheDeclaredTransitionOnly(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Put(testRecord()); err != nil {
		t.Fatal(err)
	}
	ctx := testContext()

	// Matching current proof: the declared dependent transition is admitted.
	v := Verify(s, ctx)
	dec := convergence.Evaluate(observationWithProof(v.AdmissionDependency(ModeEnforce, ctx)))
	if !dec.Admitted {
		t.Fatalf("matching proof must admit the dependent transition: %+v", dec)
	}

	// Repaired head Y: X's receipt never admits Y's transition.
	ctxY := ctx
	ctxY.Required.HeadSHA = headY
	vy := Verify(s, ctxY)
	decY := convergence.Evaluate(observationWithProof(vy.AdmissionDependency(ModeEnforce, ctxY)))
	if decY.Admitted {
		t.Fatalf("repaired head admitted on head-X proof: %+v", decY)
	}
	if decY.Reason != convergence.ReasonDependencyUnknown {
		t.Fatalf("mismatched proof must block as Unknown, got %q", decY.Reason)
	}

	// Definite red CI: blocks as an established-unsatisfied dependency.
	red := testRecord()
	red.Result = ResultFailure
	red.ObservedAt = testObservedAt.Add(time.Second)
	if _, err := s.Put(red); err != nil {
		t.Fatal(err)
	}
	vr := Verify(s, ctx)
	decR := convergence.Evaluate(observationWithProof(vr.AdmissionDependency(ModeEnforce, ctx)))
	if decR.Admitted || decR.Reason != convergence.ReasonWaitingForDependency {
		t.Fatalf("failed predicate must block definitively: %+v", decR)
	}

	// Positive control: a candidate that declares NO proof dependency is
	// evaluated by the ordinary rules and stays admissible throughout.
	control := convergence.Evaluate(observationWithProof(nil))
	if !control.Admitted {
		t.Fatalf("unrelated transition lost admissibility: %+v", control)
	}
}

func TestOffAndShadowModesNeverEnforce(t *testing.T) {
	s := openTestStore(t)
	red := testRecord()
	red.Result = ResultFailure
	if _, err := s.Put(red); err != nil {
		t.Fatal(err)
	}
	ctx := testContext()
	v := Verify(s, ctx)
	// The verdict itself is reportable in shadow (recorded/reported…)
	if v.Status != convergence.ConditionFalse {
		t.Fatalf("shadow verdict must still be computed for reporting: %+v", v)
	}
	// …but no mode other than enforce ever yields a gating edge.
	for _, mode := range []string{ModeOff, ModeShadow, "", "SHADOW", "banana", "enforce "} {
		if dep := v.AdmissionDependency(mode, ctx); dep != nil {
			t.Errorf("mode %q produced a gating dependency; only exact %q may", mode, ModeEnforce)
		}
	}
	dec := convergence.Evaluate(observationWithProof(v.AdmissionDependency(ModeOff, ctx)))
	if !dec.Admitted {
		t.Fatalf("off mode changed admission behavior: %+v", dec)
	}
}

func TestModeResolutionIsDefaultOff(t *testing.T) {
	if RecordingEnabled(ModeOff) || RecordingEnabled("") || RecordingEnabled("bogus") {
		t.Fatal("recording must be default-off")
	}
	if !RecordingEnabled(ModeShadow) || !RecordingEnabled(ModeEnforce) {
		t.Fatal("shadow and enforce must enable recording")
	}
	if EnforcementEnabled(ModeShadow) || EnforcementEnabled(ModeOff) || EnforcementEnabled("Enforce") {
		t.Fatal("only exact enforce may enforce")
	}
	if !EnforcementEnabled(ModeEnforce) {
		t.Fatal("enforce must enforce")
	}
}

// ─── Durable store ──────────────────────────────────────────────────────────

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "proofs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreRestartReconstructsSameReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(testRecord()); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("restart failed to reconstruct: %v", err)
	}
	got, ok := reopened.Get(testRecord().Key())
	if !ok {
		t.Fatal("receipt lost across restart")
	}
	if !got.equivalent(testRecord()) || !got.ObservedAt.Equal(testObservedAt) {
		t.Fatalf("restart reconstructed a different receipt: %+v", got)
	}
	if v := Verify(reopened, testContext()); !v.Satisfied() {
		t.Fatalf("reconstructed receipt must satisfy identically: %+v", v)
	}
}

func TestStoreDuplicateEvidenceDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(testRecord()); err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same evidence re-observed later: dedupe, no state duplication, and the
	// stored receipt (including its original instant) is what comes back.
	dup := testRecord()
	dup.ObservedAt = testObservedAt.Add(time.Minute)
	got, err := s.Put(dup)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ObservedAt.Equal(testObservedAt) {
		t.Fatalf("dedup must return the stored receipt, got instant %s", got.ObservedAt)
	}
	if got.Key() != testRecord().Key() || len(s.List()) != 1 {
		t.Fatalf("duplicate evidence duplicated state: %d records", len(s.List()))
	}
	secondBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("dedup re-persisted the file; a duplicate must have no effects")
	}
}

func TestStoreOutOfOrderEvidenceCannotRegress(t *testing.T) {
	s := openTestStore(t)
	newer := testRecord()
	newer.Result = ResultFailure
	newer.ObservedAt = testObservedAt.Add(time.Hour)
	if _, err := s.Put(newer); err != nil {
		t.Fatal(err)
	}
	// An OLDER green receipt arrives late; it must not overwrite current truth.
	older := testRecord() // success at testObservedAt
	if _, err := s.Put(older); !errors.Is(err, ErrEvidenceRegression) {
		t.Fatalf("late old evidence must be refused, got %v", err)
	}
	got, _ := s.Get(newer.Key())
	if got.Result != ResultFailure {
		t.Fatalf("current truth regressed to %s by arrival order", got.Result)
	}
	// A genuinely newer re-observation supersedes deterministically.
	newest := testRecord()
	newest.ObservedAt = testObservedAt.Add(2 * time.Hour)
	if _, err := s.Put(newest); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(newest.Key())
	if got.Result != ResultSuccess {
		t.Fatal("newer re-observation failed to supersede")
	}
}

func TestStoreRefusesMalformedRecord(t *testing.T) {
	s := openTestStore(t)
	bad := testRecord()
	bad.Fingerprint.HeadSHA = "nope"
	if _, err := s.Put(bad); !errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("malformed record must be refused: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("malformed record was stored")
	}
}

func TestStoreCorruptFileIsARefusalNeverSatisfaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proofs.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt store opened silently")
	}
	// The bytes are left exactly as found for inspection.
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{corrupt" {
		t.Fatalf("corrupt bytes were altered: %q %v", data, err)
	}

	// A parseable file holding a malformed or duplicate receipt likewise
	// refuses to open rather than inventing truth.
	bad := testRecord()
	bad.Fingerprint.HeadSHA = "zzz"
	writeStoreFile(t, path, []Record{bad})
	if _, err := Open(path); err == nil {
		t.Fatal("malformed persisted receipt opened silently")
	}
	writeStoreFile(t, path, []Record{testRecord(), testRecord()})
	if _, err := Open(path); err == nil {
		t.Fatal("conflicting duplicate receipts opened silently")
	}
}

func writeStoreFile(t *testing.T, path string, recs []Record) {
	t.Helper()
	data, err := json.Marshal(persistedStore{Version: storeFormatVersion, Records: recs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o660); err != nil {
		t.Fatal(err)
	}
}

func TestStoreConcurrentWritersSerializeToOneDeterministicResult(t *testing.T) {
	s := openTestStore(t)
	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := testRecord()
			rec.ObservedAt = testObservedAt.Add(time.Duration(i) * time.Second)
			_, _ = s.Put(rec) // regressions among the racers are legitimately refused
		}(i)
	}
	wg.Wait()
	all := s.List()
	if len(all) != 1 {
		t.Fatalf("racing writers produced %d records for one key", len(all))
	}
	// Whatever won, the store and its file agree (restart check).
	reopened, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get(all[0].Key())
	if !ok || !got.ObservedAt.Equal(all[0].ObservedAt) {
		t.Fatalf("disk and memory disagree after race: %+v vs %+v", got, all[0])
	}
}

func TestStoreGetUnknownAndEmptyKey(t *testing.T) {
	s := openTestStore(t)
	if _, ok := s.Get(""); ok {
		t.Fatal("empty key returned a record")
	}
	if _, ok := s.Get("default/x/y@z|" + PredicateExactHeadGreen + "|1|" + headX); ok {
		t.Fatal("unknown key returned a record")
	}
}

func TestStoreDistinctSubjectsCoexist(t *testing.T) {
	// An unrelated proof (different head = different key) remains valid as a
	// positive control while another subject's receipt churns.
	s := openTestStore(t)
	if _, err := s.Put(testRecord()); err != nil {
		t.Fatal(err)
	}
	other := testRecord()
	other.Fingerprint.HeadSHA = headY
	other.Provenance.Query = "ListCheckRunsForRef@" + headY
	if _, err := s.Put(other); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 2 {
		t.Fatalf("distinct subjects collided: %d records", len(s.List()))
	}
	if v := Verify(s, testContext()); !v.Satisfied() {
		t.Fatalf("unrelated write invalidated a valid receipt: %+v", v)
	}
}

func TestStorePersistFailureRollsBackMemory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "proofs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(testRecord()); err != nil {
		t.Fatal(err)
	}
	// Make the tmp write fail: the store's directory disappears.
	s.path = filepath.Join(dir, "gone", "proofs.json")
	newer := testRecord()
	newer.Result = ResultFailure
	newer.ObservedAt = testObservedAt.Add(time.Hour)
	if _, err := s.Put(newer); err == nil {
		t.Fatal("persist into a missing directory succeeded")
	}
	got, ok := s.Get(testRecord().Key())
	if !ok || got.Result != ResultSuccess {
		t.Fatalf("failed persist left memory and disk disagreeing: %+v", got)
	}
	// A failed FIRST write for a new key likewise leaves no phantom record.
	fresh := testRecord()
	fresh.Fingerprint.HeadSHA = headY
	if _, err := s.Put(fresh); err == nil {
		t.Fatal("persist into a missing directory succeeded")
	}
	if _, ok := s.Get(fresh.Key()); ok {
		t.Fatal("failed first persist left a phantom in-memory receipt")
	}
}

func TestRecordEquivalenceIsEvidenceIdentity(t *testing.T) {
	base := testRecord()
	same := base.clone()
	same.ObservedAt = testObservedAt.Add(time.Minute) // instant excluded
	if !base.equivalent(same) {
		t.Fatal("identical evidence at a later instant must be equivalent")
	}
	differentQuery := base.clone()
	differentQuery.Provenance.Query = "other"
	differentIDs := base.clone()
	differentIDs.Provenance.CheckRunIDs = []int64{11, 99}
	fewerIDs := base.clone()
	fewerIDs.Provenance.CheckRunIDs = []int64{11}
	differentResult := base.clone()
	differentResult.Result = ResultFailure
	for name, other := range map[string]Record{
		"query": differentQuery, "run ids": differentIDs,
		"run count": fewerIDs, "result": differentResult,
	} {
		if base.equivalent(other) {
			t.Errorf("%s difference treated as duplicate evidence", name)
		}
	}
}

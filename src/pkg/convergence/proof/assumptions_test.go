package proof

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test fixtures shared by the #4254 selective-invalidation suite. Two
// unrelated subjects in two repositories: proofA is the candidate under test,
// proofB is the standing positive control that must survive every
// invalidation aimed at A's inputs.

const (
	shaHeadA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaHeadA2 = "a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
	shaBaseA  = "1111111111111111111111111111111111111111"
	shaBaseA2 = "2222222222222222222222222222222222222222"
	shaHeadB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaBaseB  = "3333333333333333333333333333333333333333"
)

func fpA() Fingerprint {
	return Fingerprint{
		OutcomeKey:        "default/acme/widgets@ship-green",
		PredicateID:       PredicateExactHeadGreen,
		DesiredGeneration: 1,
		Repo:              "acme/widgets",
		PRNumber:          7,
		HeadSHA:           shaHeadA,
		BaseSHA:           shaBaseA,
		CheckPolicyID:     CheckPolicyID("v1", []string{"build", "test"}),
		Producer:          ProducerGitHubChecksAPI,
	}
}

func fpB() Fingerprint {
	return Fingerprint{
		OutcomeKey:        "default/acme/gadgets@ship-green",
		PredicateID:       PredicateExactHeadGreen,
		DesiredGeneration: 1,
		Repo:              "acme/gadgets",
		PRNumber:          9,
		HeadSHA:           shaHeadB,
		BaseSHA:           shaBaseB,
		CheckPolicyID:     CheckPolicyID("v1", []string{"build"}),
		Producer:          ProducerGitHubChecksAPI,
	}
}

func greenRecord(t *testing.T, fp Fingerprint, at time.Time) Record {
	t.Helper()
	rec, err := NormalizeCheckEvidence(fp, []CheckConclusion{
		{ID: 1, Name: "build", Completed: true, Conclusion: "success"},
		{ID: 2, Name: "test", Completed: true, Conclusion: "success"},
	}, at)
	if err != nil {
		t.Fatalf("NormalizeCheckEvidence: %v", err)
	}
	return rec
}

// currentInputs is the healthy snapshot in which every declared input of both
// subjects still holds the value the receipts rest on.
func currentInputs() InputObservation {
	a, b := fpA(), fpB()
	return InputObservation{
		Current: map[string]string{
			AssumptionName(AssumptionBaseSHA, a.Repo):     a.BaseSHA,
			AssumptionName(AssumptionCheckPolicy, a.Repo): a.CheckPolicyID,
			AssumptionName(AssumptionBaseSHA, b.Repo):     b.BaseSHA,
			AssumptionName(AssumptionCheckPolicy, b.Repo): b.CheckPolicyID,
		},
		Observed: map[string]bool{
			AssumptionName(AssumptionBaseSHA, a.Repo):     true,
			AssumptionName(AssumptionCheckPolicy, a.Repo): true,
			AssumptionName(AssumptionBaseSHA, b.Repo):     true,
			AssumptionName(AssumptionCheckPolicy, b.Repo): true,
		},
	}
}

func TestDeclaredAssumptions_DerivedFromFingerprintInputs(t *testing.T) {
	got := fpA().DeclaredAssumptions()
	if len(got) != 2 {
		t.Fatalf("v1 predicate declares exactly base+policy, got %v", got)
	}
	wantBase := AssumptionName(AssumptionBaseSHA, "acme/widgets")
	wantPolicy := AssumptionName(AssumptionCheckPolicy, "acme/widgets")
	found := map[string]string{}
	for _, a := range got {
		found[a.Name] = a.Value
	}
	if found[wantBase] != shaBaseA {
		t.Fatalf("base assumption = %q, want %q", found[wantBase], shaBaseA)
	}
	if found[wantPolicy] != fpA().CheckPolicyID {
		t.Fatalf("policy assumption = %q, want fingerprint policy", found[wantPolicy])
	}
}

func TestDeclaredAssumptions_InvalidFingerprintDeclaresNothing(t *testing.T) {
	bad := fpA()
	bad.HeadSHA = "short"
	if got := bad.DeclaredAssumptions(); got != nil {
		t.Fatalf("unidentifiable subject must declare nothing, got %v", got)
	}
}

func TestNormalizeCheckEvidence_PersistsDeclarations(t *testing.T) {
	rec := greenRecord(t, fpA(), time.Now())
	if len(rec.Assumptions) != 2 {
		t.Fatalf("normalized receipt must carry its declarations, got %v", rec.Assumptions)
	}
}

// RED guard: a receipt whose persisted declarations disagree with its own
// fingerprint is malformed — it can neither dodge invalidation by
// under-declaring nor invalidate unrelated proofs by over-declaring.
func TestValidate_RefusesForgedDeclarations(t *testing.T) {
	rec := greenRecord(t, fpA(), time.Now())

	under := rec.clone()
	under.Assumptions = under.Assumptions[:1]
	if err := under.Validate(); err == nil {
		t.Fatal("under-declared receipt must be malformed")
	}

	forged := rec.clone()
	forged.Assumptions[0].Value = shaBaseA2
	if err := forged.Validate(); err == nil {
		t.Fatal("receipt declaring a value its fingerprint never rested on must be malformed")
	}

	alien := rec.clone()
	alien.Assumptions[0].Name = AssumptionName(AssumptionBaseSHA, "acme/gadgets")
	if err := alien.Validate(); err == nil {
		t.Fatal("receipt declaring another subject's input must be malformed")
	}
}

// Legacy receipts persisted before the assumptions field existed fall back to
// the fingerprint derivation: additive migration can never exempt old
// evidence from invalidation.
func TestLegacyReceipt_StillDeclaresViaFingerprint(t *testing.T) {
	rec := greenRecord(t, fpA(), time.Now())
	rec.Assumptions = nil // as read back from a pre-#4254 file
	if err := rec.Validate(); err != nil {
		t.Fatalf("legacy receipt must remain valid: %v", err)
	}
	got := rec.declaredAssumptions()
	if len(got) != 2 {
		t.Fatalf("legacy receipt must still declare base+policy, got %v", got)
	}
	moved := currentInputs()
	moved.Current[AssumptionName(AssumptionBaseSHA, "acme/widgets")] = shaBaseA2
	if v, demoted := rec.JudgeAssumptions(moved); !demoted || v.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("legacy receipt must invalidate on declared input movement, got %+v demoted=%v", v, demoted)
	}
}

func TestStore_RoundTripsDeclarations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Put(greenRecord(t, fpA(), time.Now())); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec, ok := reopened.Get(fpA().Key())
	if !ok {
		t.Fatal("receipt lost across restart")
	}
	if len(rec.Assumptions) != 2 {
		t.Fatalf("declarations must reconstruct from durable state, got %v", rec.Assumptions)
	}
}

func TestStore_DeclaringAssumption_EnumeratesSelectively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	if _, err := s.Put(greenRecord(t, fpA(), now)); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if _, err := s.Put(greenRecord(t, fpB(), now)); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	baseA := AssumptionName(AssumptionBaseSHA, "acme/widgets")
	keys := s.DeclaringAssumption(baseA)
	if len(keys) != 1 || !strings.Contains(keys[0], "acme/widgets") {
		t.Fatalf("only A declares %s, got %v", baseA, keys)
	}
	if got := s.DeclaringAssumption(""); got != nil {
		t.Fatalf("empty name enumerates nothing, got %v", got)
	}
	if got := s.DeclaringAssumption("base-sha:acme/unrelated"); got != nil {
		t.Fatalf("undeclared input enumerates nothing, got %v", got)
	}
}

package proof

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence"
)

func inspectionFingerprint() Fingerprint {
	return Fingerprint{
		OutcomeKey:        "hivecommons/hive@audit-campaign",
		PredicateID:       PredicateInspectionRecorded,
		DesiredGeneration: 7,
		Producer:          ProducerHiveAuditLane,
		InspectionBeadID:  "inspection-auth",
		ReceiptDigest:     "sha256-auth-receipt",
	}
}

func TestInspectionPredicateBindsBeadAndReceiptDigest(t *testing.T) {
	fp := inspectionFingerprint()
	if err := fp.Validate(); err != nil {
		t.Fatalf("inspection fingerprint must validate: %v", err)
	}
	wantKey := "hivecommons/hive@audit-campaign|" + PredicateInspectionRecorded + "|7|inspection-auth"
	if got := fp.Key(); got != wantKey {
		t.Fatalf("inspection key = %q, want %q", got, wantKey)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	rec := Record{Fingerprint: fp, Result: ResultSuccess, Provenance: Provenance{Query: "audit-inspection@auth"}, ObservedAt: now}
	verdict := rec.VerifyAgainst(Context{Required: fp, Now: now.Add(time.Minute), MaxAge: time.Hour})
	if verdict.Status != convergence.ConditionTrue || !verdict.Satisfied() {
		t.Fatalf("inspection record should satisfy current context: %+v", verdict)
	}
	ctx := Context{Required: fp, Now: now.Add(time.Minute), MaxAge: time.Hour}
	ctx.Required.ReceiptDigest = "different"
	if verdict := rec.VerifyAgainst(ctx); verdict.Status != convergence.ConditionUnknown || verdict.Reason != ReasonFingerprintMismatch {
		t.Fatalf("receipt digest mismatch must be unknown fingerprint mismatch: %+v", verdict)
	}
}

func TestInspectionPredicateRejectsMissingEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*Fingerprint){
		"missing bead":      func(f *Fingerprint) { f.InspectionBeadID = "" },
		"missing digest":    func(f *Fingerprint) { f.ReceiptDigest = "" },
		"wrong producer":    func(f *Fingerprint) { f.Producer = ProducerGitHubChecksAPI },
		"separator in bead": func(f *Fingerprint) { f.InspectionBeadID = "a|b" },
	} {
		fp := inspectionFingerprint()
		mutate(&fp)
		if err := fp.Validate(); err == nil {
			t.Fatalf("%s: malformed inspection predicate validated", name)
		}
	}
}

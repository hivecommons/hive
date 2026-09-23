package proof

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence"
)

func publicationFingerprint() Fingerprint {
	return Fingerprint{
		OutcomeKey:        "hivecommons/hive@audit-campaign",
		PredicateID:       PredicateFindingPublished,
		DesiredGeneration: 3,
		Producer:          ProducerHivePublisher,
		IssueNumber:       42,
		FindingHash:       "sha256-finding-auth",
	}
}

func TestPublicationPredicateBindsIssueAndFindingHash(t *testing.T) {
	fp := publicationFingerprint()
	if err := fp.Validate(); err != nil {
		t.Fatalf("publication fingerprint must validate: %v", err)
	}
	wantKey := "hivecommons/hive@audit-campaign|" + PredicateFindingPublished + "|3|sha256-finding-auth"
	if got := fp.Key(); got != wantKey {
		t.Fatalf("publication key = %q, want %q", got, wantKey)
	}
	if got := fp.DeclaredAssumptions(); got != nil {
		t.Fatalf("publication predicate declares no base/check assumptions, got %v", got)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	rec := Record{Fingerprint: fp, Result: ResultSuccess, Provenance: Provenance{Query: "publish@sha256-finding-auth"}, ObservedAt: now}
	ctx := Context{Required: fp, Now: now.Add(time.Minute), MaxAge: time.Hour}
	if verdict := rec.VerifyAgainst(ctx); !verdict.Satisfied() {
		t.Fatalf("publication record should satisfy the current context: %+v", verdict)
	}
	other := ctx
	other.Required.IssueNumber = 43
	if verdict := rec.VerifyAgainst(other); verdict.Status != convergence.ConditionUnknown || verdict.Reason != ReasonFingerprintMismatch {
		t.Fatalf("issue number mismatch must be an unknown fingerprint mismatch: %+v", verdict)
	}
	other = ctx
	other.Required.FindingHash = "sha256-other"
	if verdict := rec.VerifyAgainst(other); verdict.Status != convergence.ConditionUnknown || verdict.Reason != ReasonFingerprintMismatch {
		t.Fatalf("finding hash mismatch must be an unknown fingerprint mismatch: %+v", verdict)
	}
}

func TestPublicationPredicateRejectsMissingEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*Fingerprint){
		"missing hash":      func(f *Fingerprint) { f.FindingHash = "" },
		"separator in hash": func(f *Fingerprint) { f.FindingHash = "a|b" },
		"missing issue":     func(f *Fingerprint) { f.IssueNumber = 0 },
		"wrong producer":    func(f *Fingerprint) { f.Producer = ProducerHiveAuditLane },
	} {
		fp := publicationFingerprint()
		mutate(&fp)
		if err := fp.Validate(); err == nil {
			t.Fatalf("%s: malformed publication predicate validated", name)
		}
	}
}

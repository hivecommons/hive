package github

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func TestMergeReviewEvidenceShadow(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("o/r", 7, "abc"),
		Repo:          "o/r", Number: 7,
		Author:  evidence.Author{Login: "a", Kind: evidence.AuthorHuman},
		BaseSHA: "base", HeadSHA: "abc",
		CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Hour),
	}
	path := ReviewEvidencePath(t.TempDir(), "o/r", 7, "abc")
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(path, b); err != nil {
		t.Fatal(err)
	}

	if err := MergeReviewEvidenceShadow(path, evidence.Shadow{
		ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &yes, Reason: "no approval", Tier: "T2", Confidence: 0.8, EvaluatedAt: at},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := MergeReviewEvidenceShadow(path, evidence.Shadow{
		Intent: &evidence.ShadowGate{WouldHaveBlocked: &no, Confidence: 0.6, EvaluatedAt: at},
	}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := LoadReviewEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Shadow == nil || got.Shadow.ReviewGate == nil || got.Shadow.Intent == nil {
		t.Fatalf("shadow = %+v, want both gates kept", got.Shadow)
	}
	if !*got.Shadow.ReviewGate.WouldHaveHeld || *got.Shadow.Intent.WouldHaveBlocked {
		t.Fatalf("shadow values lost: %+v %+v", got.Shadow.ReviewGate, got.Shadow.Intent)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Fatalf("updated_at = %v, want %v", got.UpdatedAt, at)
	}
	if want, err := evidence.Hash(got); err != nil || got.Hash != want {
		t.Fatalf("hash = %q, want %q (err %v)", got.Hash, want, err)
	}
}

func TestMergeReviewEvidenceShadowErrors(t *testing.T) {
	if err := MergeReviewEvidenceShadow(filepath.Join(t.TempDir(), "missing.json"), evidence.Shadow{
		Intent: &evidence.ShadowGate{},
	}, nil); err == nil {
		t.Fatal("missing bundle must error")
	}
	if err := MergeReviewEvidenceShadow(filepath.Join(t.TempDir(), "unused.json"), evidence.Shadow{}, nil); err != nil {
		t.Fatalf("empty observation must be a no-op, got %v", err)
	}
}

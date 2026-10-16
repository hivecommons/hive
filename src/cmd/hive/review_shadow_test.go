package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

func TestReviewShadowGate(t *testing.T) {
	yes := github.MergeableYes
	pr := github.PullRequest{Number: 8, Mergeable: yes, MergeableState: "clean", CIStatus: "success"}
	now := time.Now()
	tests := []struct {
		name       string
		gates      mergeGates
		wantBucket mergeBucket
		wantRecord bool
		wantHeld   bool
	}{
		{"shadow on, no approval: still eligible, would have held", mergeGates{shadowReview: true, reviewLoaded: true}, mergeBucketEligible, true, true},
		{"shadow on, artifact unavailable: still eligible, would have held", mergeGates{shadowReview: true}, mergeBucketEligible, true, true},
		{"shadow off: no record", mergeGates{reviewLoaded: true}, mergeBucketEligible, false, false},
		{"require_approval on: still blocked, no shadow record", mergeGates{requireReviewApproval: true, reviewLoaded: true}, mergeBucketSkip, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bucket, _, _ := classifyMergeEligibility(pr, false, "org/repo", tc.gates)
			if bucket != tc.wantBucket {
				t.Fatalf("bucket = %v, want %v", bucket, tc.wantBucket)
			}
			gate := reviewShadowGate(tc.gates, "org/repo", pr, now)
			if (gate != nil) != tc.wantRecord {
				t.Fatalf("recorded = %v, want %v", gate != nil, tc.wantRecord)
			}
			if gate != nil && *gate.WouldHaveHeld != tc.wantHeld {
				t.Fatalf("would_have_held = %v, want %v", *gate.WouldHaveHeld, tc.wantHeld)
			}
		})
	}
}

func TestRecordReviewShadowRoundTrip(t *testing.T) {
	root := t.TempDir()
	prev := github.ReviewEvidenceRoot
	github.ReviewEvidenceRoot = root
	t.Cleanup(func() { github.ReviewEvidenceRoot = prev })

	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	pr := github.PullRequest{Number: 8, HeadSHA: "abc"}
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("org/repo", pr.Number, pr.HeadSHA),
		Repo:          "org/repo", Number: pr.Number,
		Author:  evidence.Author{Login: "a", Kind: evidence.AuthorHuman},
		BaseSHA: "base", HeadSHA: pr.HeadSHA,
		CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Hour),
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	path := github.ReviewEvidencePath("", "org/repo", pr.Number, pr.HeadSHA)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Review: config.ReviewConfig{Shadow: true}}

	recordReviewShadow(cfg, logger, pr, "org/repo", mergeGates{shadowReview: true, reviewLoaded: true}, at)

	got, err := github.LoadReviewEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Shadow == nil || got.Shadow.ReviewGate == nil {
		t.Fatalf("shadow review gate not recorded: %+v", got.Shadow)
	}
	g := got.Shadow.ReviewGate
	if g.WouldHaveHeld == nil || !*g.WouldHaveHeld || g.Reason != "awaiting review approval" {
		t.Fatalf("review gate = %+v, want would_have_held=true, reason=awaiting review approval", g)
	}
	if !g.EvaluatedAt.Equal(at) || !got.UpdatedAt.Equal(at) {
		t.Fatalf("evaluated_at = %v, updated_at = %v, want %v", g.EvaluatedAt, got.UpdatedAt, at)
	}
	if want, err := evidence.Hash(got); err != nil || got.Hash != want {
		t.Fatalf("hash = %q, want %q (err %v)", got.Hash, want, err)
	}

	// Evidence disabled: the bundle is left untouched.
	off := false
	cfg.Evidence.Enabled = &off
	later := at.Add(time.Hour)
	recordReviewShadow(cfg, logger, pr, "org/repo", mergeGates{shadowReview: true}, later)
	again, err := github.LoadReviewEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Shadow.ReviewGate.EvaluatedAt.Equal(at) {
		t.Fatalf("evidence disabled but bundle rewritten: evaluated_at = %v", again.Shadow.ReviewGate.EvaluatedAt)
	}

	// Shadow off or nil config: nothing happens, no panic.
	recordReviewShadow(cfg, logger, pr, "org/repo", mergeGates{}, later)
	recordReviewShadow(nil, logger, pr, "org/repo", mergeGates{shadowReview: true}, later)
}

// A configured but unreadable signing key must not downgrade the bundle: the
// shadow write is skipped and a Warn line says so, instead of Seal(b, nil)
// rewriting a signed bundle as unsigned.
func TestRecordReviewShadowSigningKeyUnusable(t *testing.T) {
	root := t.TempDir()
	prev := github.ReviewEvidenceRoot
	github.ReviewEvidenceRoot = root
	t.Cleanup(func() { github.ReviewEvidenceRoot = prev })

	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	pr := github.PullRequest{Number: 9, HeadSHA: "def"}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("org/repo", pr.Number, pr.HeadSHA),
		Repo:          "org/repo", Number: pr.Number,
		Author:  evidence.Author{Login: "a", Kind: evidence.AuthorHuman},
		BaseSHA: "base", HeadSHA: pr.HeadSHA,
		CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Hour),
	}
	if err := evidence.Seal(b, priv); err != nil {
		t.Fatal(err)
	}
	path := github.ReviewEvidencePath("", "org/repo", pr.Number, pr.HeadSHA)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{Review: config.ReviewConfig{Shadow: true}}
	// A missing key file means "unsigned" by design; the file must exist and
	// be malformed for LoadSigningKey to report the key as unusable.
	cfg.Evidence.SigningKeyFile = filepath.Join(root, "bad-key.pem")
	if err := os.WriteFile(cfg.Evidence.SigningKeyFile, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	recordReviewShadow(cfg, logger, pr, "org/repo", mergeGates{shadowReview: true, reviewLoaded: true}, at)

	got, err := github.LoadReviewEvidence(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Signed || got.Signature == "" || got.Shadow != nil {
		t.Fatalf("signed bundle rewritten with unusable key: signed=%v shadow=%+v", got.Signed, got.Shadow)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "signing key unusable") {
		t.Fatalf("expected Warn about unusable signing key, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "confidence=1") {
		t.Fatalf("audit log line missing confidence: %q", buf.String())
	}
}

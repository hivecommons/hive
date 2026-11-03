package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

func TestIntentShadowGate(t *testing.T) {
	now := time.Now()
	unauthorized := intent.Verdict{Tier: intent.Tier1, AgentPR: true, Reason: intent.ReasonTier1NeedsIssue}
	misaligned := intent.Verdict{Tier: intent.Tier1, AgentPR: true, Authorized: true, Reason: intent.ReasonLinkedIssue,
		Alignment: &intent.AlignmentVerdict{Status: intent.AlignmentStatusMisaligned, Rationale: "drift",
			Model: &intent.ModelAlignmentVerdict{Status: intent.AlignmentStatusMisaligned, Confidence: 0.7}}}
	authorized := intent.Verdict{Tier: intent.Tier0, AgentPR: true, Authorized: true, Reason: intent.ReasonTier0Additive}
	human := intent.Verdict{Tier: intent.Tier0, Authorized: false, Reason: "non-agent PR"}
	tests := []struct {
		name       string
		v          intent.Verdict
		wantBlock  bool
		wantReason string
		wantTier   string
		wantConf   float64
	}{
		{"unauthorized agent PR", unauthorized, true, intent.ReasonTier1NeedsIssue, "tier1", 1},
		{"misaligned agent PR", misaligned, true, intent.ReasonAlignmentMisaligned + ": drift", "tier1", 0.7},
		{"authorized agent PR", authorized, false, intent.ReasonTier0Additive, "tier0", 1},
		{"human PR never blocked", human, false, "non-agent PR", "tier0", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := intentShadowGate(tc.v, now)
			if *g.WouldHaveBlocked != tc.wantBlock || g.Reason != tc.wantReason || g.Tier != tc.wantTier || g.Confidence != tc.wantConf {
				t.Fatalf("gate = %+v (blocked %v), want blocked %v reason %q tier %q conf %v", g, *g.WouldHaveBlocked, tc.wantBlock, tc.wantReason, tc.wantTier, tc.wantConf)
			}
			if g.WouldHaveHeld != nil || !g.EvaluatedAt.Equal(now) {
				t.Fatalf("gate = %+v, want only would_have_blocked and evaluated_at", g)
			}
		})
	}
}

// Shadow on: the PR the enforce gate would withhold stays merge-eligible and
// a would_have_blocked observation is recorded. Enforce on: still blocked,
// and shadow records nothing.
func TestIntentShadowMergeEligibleLane(t *testing.T) {
	yes := github.MergeableYes
	pr := github.PullRequest{Number: 9, Mergeable: yes, MergeableState: "clean", CIStatus: "success", HeadSHA: "abc"}
	verdicts := map[string]intent.Verdict{"org/repo/9": {Tier: intent.Tier1, AgentPR: true, Reason: intent.ReasonTier1NeedsIssue}}
	off := false

	tests := []struct {
		name       string
		enforce    bool
		shadow     bool
		wantBucket mergeBucket
		wantRecord bool
	}{
		{"shadow on: eligible and recorded", false, true, mergeBucketEligible, true},
		{"shadow off: eligible, no record", false, false, mergeBucketEligible, false},
		{"enforce on: blocked, no record", true, true, mergeBucketSkip, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Intent.Enforce, cfg.Intent.Shadow = tc.enforce, tc.shadow
			cfg.Evidence.Enabled = &off
			bucket, _, _ := classifyMergeEligibility(pr, false, "org/repo", mergeGates{enforceIntent: tc.enforce, intentVerdicts: verdicts})
			if bucket != tc.wantBucket {
				t.Fatalf("bucket = %v, want %v", bucket, tc.wantBucket)
			}
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			recordIntentShadow(cfg, logger, tc.enforce, verdicts, pr, "org/repo", time.Now())
			got := strings.Contains(buf.String(), "intent shadow gate")
			if got != tc.wantRecord {
				t.Fatalf("recorded = %v, want %v (log %q)", got, tc.wantRecord, buf.String())
			}
			if got && !strings.Contains(buf.String(), "would_have_blocked=true") {
				t.Fatalf("log %q missing would_have_blocked=true", buf.String())
			}
		})
	}
}

func TestRecordIntentShadowNoVerdictOrConfig(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := &config.Config{}
	cfg.Intent.Shadow = true
	recordIntentShadow(cfg, logger, false, nil, github.PullRequest{Number: 1}, "org/repo", time.Now())
	writeIntentShadow(nil, logger, "self-merge", "org/repo", 1, "abc", nil)
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %q", buf.String())
	}
	if intentShadowEnabled(nil) {
		t.Fatal("nil config must not enable intent shadow")
	}
}

// With evidence enabled but no bundle on disk the write fails; recording
// must swallow that (logged only) and never panic or block.
func TestWriteIntentShadowEvidenceMissingBundle(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{}
	cfg.Intent.Shadow = true
	gate := intentShadowGate(intent.Verdict{Tier: intent.Tier1, AgentPR: true, Reason: intent.ReasonTier1NeedsIssue}, time.Now())
	writeIntentShadow(cfg, logger, "self-merge", "org/repo", 1, "missing-head-for-test", gate)
	if !strings.Contains(buf.String(), "intent shadow gate") {
		t.Fatalf("audit log line missing: %q", buf.String())
	}
	sg := selfMergeIntentGate(cfg, nil)
	if !sg.Shadow() || sg.RecordShadow == nil {
		t.Fatal("selfMergeIntentGate must wire intent.shadow")
	}
	sg.RecordShadow("org/repo", 1, "missing-head-for-test", *gate)
}

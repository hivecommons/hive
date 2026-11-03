package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/review"
)

func TestSelfMergeReviewGateVerdictFromArtifact(t *testing.T) {
	old := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	artifact := review.Artifact{Items: []review.Aggregate{
		{Repo: "acme/widget", Number: 7, HeadSHA: "approved", Verdict: review.VerdictApprove, MergeEligible: true,
			Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictApprove}},
		{Repo: "acme/widget", Number: 7, HeadSHA: "changes", Verdict: review.VerdictReject,
			Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictReject, "style": review.VerdictChangesRequested, "docs": review.VerdictApprove}},
		{Repo: "acme/widget", Number: 7, HeadSHA: "incomplete", Verdict: review.VerdictIncomplete,
			Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictApprove}},
		{Repo: "acme/widget", Number: 7, HeadSHA: "legacy-human", Verdict: review.VerdictRequiresHuman, RequiresHuman: true,
			HumanSummaries: []string{"correctness requires human: field is plumbed but never populated; add the producer."},
			Perspectives:   map[review.Perspective]review.Verdict{"correctness": review.VerdictRequiresHuman}},
		// An older changes_requested superseded by a newer approve on the same head.
		{Repo: "acme/widget", Number: 7, HeadSHA: "rereviewed", Verdict: review.VerdictChangesRequested, RecordedAt: old,
			Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictChangesRequested}},
		{Repo: "acme/widget", Number: 7, HeadSHA: "rereviewed", Verdict: review.VerdictApprove, MergeEligible: true, RecordedAt: old.Add(time.Hour),
			Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictApprove}},
		// rationguard#296: a hive-authored permissions change parked as
		// requires_human (decision risk class) while style asked for changes.
		{Repo: "acme/widget", Number: 7, HeadSHA: "decision", Verdict: review.VerdictRequiresHuman, RequiresHuman: true, DecisionRequested: true,
			HiveAuthoredKnown: true, HiveAuthored: true,
			DecisionQuestion: "@maintainer: confirm this CI permissions change may land.",
			Perspectives:     map[review.Perspective]review.Verdict{"security": review.VerdictRequiresHuman, "style": review.VerdictChangesRequested}},
	}}
	tests := []struct {
		name string
		head string
		want automerge.ReviewGateVerdict
	}{
		{name: "no aggregate for head", head: "other", want: automerge.ReviewGateVerdict{Loaded: true}},
		{name: "approved", head: "approved", want: automerge.ReviewGateVerdict{Loaded: true, Found: true, Approved: true, Verdict: "approve"}},
		{name: "changes requested", head: "changes", want: automerge.ReviewGateVerdict{Loaded: true, Found: true, Verdict: "reject", ChangesRequested: []string{"security", "style"}}},
		{name: "awaiting", head: "incomplete", want: automerge.ReviewGateVerdict{Loaded: true, Found: true, Verdict: "incomplete"}},
		{name: "legacy normalized", head: "legacy-human", want: automerge.ReviewGateVerdict{Loaded: true, Found: true, Verdict: "changes_requested", ChangesRequested: []string{"correctness"}}},
		{name: "newest aggregate wins", head: "rereviewed", want: automerge.ReviewGateVerdict{Loaded: true, Found: true, Approved: true, Verdict: "approve"}},
		// The aggregate is the one source of truth the contributor lane and
		// the 085 stage already read; the gate must not reclassify it from
		// the perspective map.
		{name: "requires human aggregate keeps its verdict", head: "decision", want: automerge.ReviewGateVerdict{Loaded: true, Found: true, Verdict: "requires_human", ChangesRequested: []string{"style"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selfMergeReviewGateVerdictFromArtifact(artifact, "acme/widget", 7, tt.head); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("verdict = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSelfMergeReviewGateVerdictFuncReadsArtifact(t *testing.T) {
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(t.TempDir(), "verdicts", review.ReviewVerdictsFile)
	defer func() { review.ReviewVerdictsPath = orig }()

	verdict := selfMergeReviewGateVerdictFunc(nil)
	if got := verdict("acme/widget", 7, "sha7"); got.Loaded || got.Found {
		t.Fatalf("missing artifact = %+v, want not loaded", got)
	}
	artifact := review.Artifact{Items: []review.Aggregate{{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Verdict: review.VerdictApprove, MergeEligible: true}}}
	if err := review.WriteArtifact(review.ReviewVerdictsPath, artifact); err != nil {
		t.Fatal(err)
	}
	if got := verdict("acme/widget", 7, "sha7"); !got.Loaded || !got.Found || !got.Approved {
		t.Fatalf("approved head = %+v", got)
	}
}

// The knobs are read through cfg on every call, so a reload applies.
func TestSelfMergeReviewGateReadsConfigLive(t *testing.T) {
	cfg := &config.Config{}
	var audited []automerge.ReviewGateEvaluation
	g := selfMergeReviewGate(cfg, nil, func(e automerge.ReviewGateEvaluation) { audited = append(audited, e) })
	if g.Mode() != config.ReviewGateModeShadow || g.RequireApproval() || g.Shadow() || g.RecordShadow == nil || g.Verdict == nil {
		t.Fatalf("default gate: mode %q require %v shadow %v", g.Mode(), g.RequireApproval(), g.Shadow())
	}
	cfg.Governor.ReviewGate.Mode = config.ReviewGateModeEnforce
	cfg.Review.RequireApproval = true
	cfg.Review.Shadow = true
	if g.Mode() != config.ReviewGateModeEnforce || !g.RequireApproval() || !g.Shadow() {
		t.Fatal("gate did not follow a config reload")
	}
	e := automerge.ReviewGateEvaluation{Lane: "self-merge", Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Outcome: "no-review-bundle", WouldHold: true}
	g.RecordEvaluation(e)
	if len(audited) != 1 || audited[0] != e {
		t.Fatalf("audited = %+v", audited)
	}
	if got, want := reviewEvaluatedAuditDetail(e), "lane=self-merge would_hold=true reason=no-review-bundle enforce=false head=sha7"; got != want {
		t.Fatalf("audit detail = %q, want %q", got, want)
	}
	overridden := automerge.ReviewGateEvaluation{Lane: "self-merge", Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Outcome: automerge.ReviewGateOutcomeHumanApproved, Enforced: true, HumanApprover: "bob", Overrode: "changes-requested:style"}
	if got, want := reviewEvaluatedAuditDetail(overridden), "lane=self-merge would_hold=false reason=human-approved enforce=true head=sha7 human_approver=bob overrode=changes-requested:style"; got != want {
		t.Fatalf("audit detail = %q, want %q", got, want)
	}
	nilGate := selfMergeReviewGate(nil, nil, nil)
	if nilGate.Mode() != config.ReviewGateModeShadow || nilGate.RequireApproval() || nilGate.Shadow() {
		t.Fatal("nil config gate must be plain shadow")
	}
	nilGate.RecordEvaluation(e)
}

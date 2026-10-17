package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewEvaluatedAuditAction is the audit-log action written once per review
// gate evaluation on the self-merge lane (hivecommons/hive#12592).
// Observation only.
const reviewEvaluatedAuditAction = "review_evaluated"

// reviewEvaluatedAuditDetail renders the audit-log detail for one evaluation:
// lane, would-hold, outcome code and enforce state.
func reviewEvaluatedAuditDetail(e automerge.ReviewGateEvaluation) string {
	detail := fmt.Sprintf("lane=%s would_hold=%t reason=%s enforce=%t head=%s", e.Lane, e.WouldHold, e.Outcome, e.Enforced, e.HeadSHA)
	if strings.TrimSpace(e.HumanApprover) != "" {
		detail += fmt.Sprintf(" human_approver=%s", e.HumanApprover)
	}
	return detail
}

// selfMergeReviewGateVerdictFunc answers the self-merge review gate from the
// same durable review-verdicts.json the merge-eligibility gate reads
// (review.LoadArtifact + Artifact.HasAggregateApproval). An unreadable
// artifact reports Loaded=false, which the gate treats as no review bundle.
func selfMergeReviewGateVerdictFunc(logger *slog.Logger) automerge.ReviewGateVerdictFunc {
	return func(repo string, number int, headSHA string) automerge.ReviewGateVerdict {
		artifact, err := review.LoadArtifact("")
		if err != nil {
			if logger != nil {
				logger.Warn("self-merge review gate: review-verdicts.json unavailable; reading as no review bundle", "error", err)
			}
			return automerge.ReviewGateVerdict{}
		}
		return selfMergeReviewGateVerdictFromArtifact(artifact, repo, number, headSHA)
	}
}

// selfMergeReviewGateVerdictFromArtifact picks the newest aggregate for the
// head with HasAggregateApproval's matching rules and lists the perspectives
// that requested changes or rejected it.
func selfMergeReviewGateVerdictFromArtifact(artifact review.Artifact, repo string, number int, headSHA string) automerge.ReviewGateVerdict {
	wantRepo, wantSHA := strings.TrimSpace(repo), strings.TrimSpace(headSHA)
	var match review.Aggregate
	found := false
	for _, item := range artifact.Items {
		if item.Number != number || strings.TrimSpace(item.Repo) != wantRepo || strings.TrimSpace(item.HeadSHA) != wantSHA {
			continue
		}
		if !found || item.RecordedAt.After(match.RecordedAt) {
			match = item
			found = true
		}
	}
	v := automerge.ReviewGateVerdict{Loaded: true, Found: found}
	if !found {
		return v
	}
	v.Approved = artifact.HasAggregateApproval(repo, number, headSHA)
	for p, verdict := range match.Perspectives {
		if verdict == review.VerdictChangesRequested || verdict == review.VerdictReject {
			v.ChangesRequested = append(v.ChangesRequested, string(p))
		}
	}
	sort.Strings(v.ChangesRequested)
	return v
}

// selfMergeReviewGate builds the swarm-review gate the App self-merge sweep
// evaluates immediately before merging its own PR (#12592). Every knob is
// read through cfg on every evaluation so a config reload applies without a
// restart: governor.review_gate.mode, review.require_approval and
// review.shadow. Shadow observations go into the head's evidence bundle;
// audit receives one review_evaluated line per evaluation.
func selfMergeReviewGate(cfg *config.Config, logger *slog.Logger, audit func(automerge.ReviewGateEvaluation)) *automerge.ReviewGate {
	if logger == nil {
		logger = slog.Default()
	}
	return &automerge.ReviewGate{
		Mode: func() string {
			if cfg == nil {
				return config.ReviewGateModeShadow
			}
			return cfg.Governor.ReviewGate.ModeOrDefault()
		},
		RequireApproval: func() bool { return cfg != nil && cfg.Review.RequireApproval },
		Shadow:          func() bool { return cfg != nil && cfg.Review.Shadow },
		Verdict:         selfMergeReviewGateVerdictFunc(logger),
		RecordShadow: func(fullRepo string, number int, head string, gate evidence.ShadowGate) {
			logger.Info("review shadow gate", "lane", gate.Lane, "repo", fullRepo, "number", number, "head", head,
				"would_have_held", *gate.WouldHaveHeld, "reason", gate.Reason, "confidence", gate.Confidence)
			writeShadowEvidence(cfg, logger, "review", fullRepo, number, head, evidence.Shadow{ReviewGate: &gate})
		},
		RecordEvaluation: func(e automerge.ReviewGateEvaluation) {
			logger.Info("self-merge review gate evaluated", "repo", e.Repo, "number", e.Number, "head", e.HeadSHA,
				"outcome", e.Outcome, "would_hold", e.WouldHold, "enforce", e.Enforced, "human_approver", e.HumanApprover)
			if audit != nil {
				audit(e)
			}
		},
	}
}

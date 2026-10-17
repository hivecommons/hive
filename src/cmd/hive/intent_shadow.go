package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// intentShadowEnabled reports whether intent.shadow applies: shadow on and
// enforce off. With enforce on the real gate acts, so there is nothing to
// shadow.
func intentShadowEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.Intent.Shadow && !cfg.Intent.Enforce
}

// intentShadowGate evaluates v the way intent.enforce: true would
// (BlocksMerge(true)) without acting on it. It must never influence a merge.
func intentShadowGate(v intent.Verdict, now time.Time) *evidence.ShadowGate {
	blocked := v.BlocksMerge(true)
	reason := v.Reason
	if blocked {
		reason = v.BlockReason()
	}
	return &evidence.ShadowGate{
		WouldHaveBlocked: &blocked,
		Reason:           reason,
		Tier:             fmt.Sprintf("tier%d", v.Tier),
		Confidence:       v.ShadowConfidence(),
		EvaluatedAt:      now,
	}
}

// writeIntentShadow logs the intent.shadow observation (audit log) and folds
// it into the PR head's evidence bundle. Recording only: failures are logged
// and never block a merge.
func writeIntentShadow(cfg *config.Config, logger *slog.Logger, lane, fullRepo string, number int, head string, gate *evidence.ShadowGate) {
	if cfg == nil || gate == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("intent shadow gate", "lane", lane, "repo", fullRepo, "number", number, "head", head,
		"would_have_blocked", *gate.WouldHaveBlocked, "tier", gate.Tier, "reason", gate.Reason, "confidence", gate.Confidence)
	writeShadowEvidence(cfg, logger, "intent", fullRepo, number, head, evidence.Shadow{Intent: gate})
}

// recordIntentShadow records intent.shadow for a PR the merge-eligible lane
// kept eligible. It is a no-op unless intent.shadow is on and enforce off, or
// when the PR has no intent verdict.
func recordIntentShadow(cfg *config.Config, logger *slog.Logger, enforceIntent bool, verdicts map[string]intent.Verdict, pr github.PullRequest, fullRepo string, now time.Time) {
	if enforceIntent || !intentShadowEnabled(cfg) {
		return
	}
	v, ok := verdicts[fmt.Sprintf("%s/%d", fullRepo, pr.Number)]
	if !ok {
		return
	}
	writeIntentShadow(cfg, logger, "merge-eligible", fullRepo, pr.Number, pr.HeadSHA, intentShadowGate(v, now))
}

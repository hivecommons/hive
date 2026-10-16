package main

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// writeShadowEvidence folds a shadow-gate observation (intent.shadow,
// review.shadow) into the PR head's evidence bundle. Recording only: failures
// are logged and never block a merge. kind names the gate in log lines.
//
// When a signing key is configured but unusable the write is skipped rather
// than performed unsigned: evidence.Seal with a nil key would rewrite a
// previously signed bundle as "signed": false, and a shadow observation is
// not worth downgrading the bundle for.
func writeShadowEvidence(cfg *config.Config, logger *slog.Logger, kind, fullRepo string, number int, head string, obs evidence.Shadow) {
	if cfg == nil || !cfg.Evidence.IsEnabled() {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	key, err := evidence.LoadSigningKey(cfg.Evidence.SigningKeyPath())
	if err != nil {
		logger.Warn(kind+" shadow: signing key unusable, evidence not recorded", "repo", fullRepo, "number", number, "error", err)
		return
	}
	path := github.ReviewEvidencePath("", fullRepo, number, head)
	if err := github.MergeReviewEvidenceShadow(path, obs, key); err != nil {
		logger.Debug(kind+" shadow: evidence not recorded", "repo", fullRepo, "number", number, "error", err)
	}
}

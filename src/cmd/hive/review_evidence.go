package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// reviewEvidenceSettings resolves the evidence.* config and the policy
// snapshot a new review evidence bundle for repo records
// (hivecommons/hive#11060). Perspectives are left empty so the relay fills
// them from the perspective set it validates verdicts against.
func reviewEvidenceSettings(cfg *config.Config, repo string) github.ReviewEvidenceSettings {
	if cfg == nil {
		return github.ReviewEvidenceSettings{}
	}
	return github.ReviewEvidenceSettings{
		Enabled:        cfg.Evidence.IsEnabled(),
		SigningKeyFile: cfg.Evidence.SigningKeyPath(),
		Policy: evidence.Policy{
			ACMMLevel:          cfg.ACMMLevelOrZero(),
			RequireApproval:    cfg.Review.RequireApproval,
			HumanMergePaths:    cfg.AutoMerge.HumanMergePathsFor(repo),
			SentinelConfigHash: sentinelConfigHash(cfg.Sentinel),
		},
	}
}

// sentinelConfigHash is the hex SHA-256 of the sentinel block's JSON, so a
// later sentinel change is visible in the bundle without copying the config.
func sentinelConfigHash(s config.SentinelConfig) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

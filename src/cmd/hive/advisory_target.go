package main

import (
	"context"
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

// resolveAdvisoryDigestRoute decides where this cycle's advisory digest goes
// from governor.advisory.target. It returns the resolved target and, for the
// Linear target, the issue identifier the digest is posted to.
//
// The GitHub route is the default and is exactly what every hive did before
// the key existed. Anything else FAILS CLOSED with an error naming the
// offending key: a Linear target with no linear_issue, or a target nobody
// recognizes, is an operator mistake that must surface as a digest-post
// error (so the hub's staleness pill trips) rather than a silent fallback to
// the GitHub issue the operator explicitly chose not to use.
func resolveAdvisoryDigestRoute(cfg *config.Config) (target, linearIssue string, err error) {
	if cfg == nil {
		return config.AdvisoryTargetGitHub, "", nil
	}
	adv := cfg.Governor.Advisory
	switch t := adv.ResolvedTarget(); t {
	case config.AdvisoryTargetGitHub:
		return t, "", nil
	case config.AdvisoryTargetLinear:
		if adv.LinearIssue == "" {
			return t, "", worksource.ErrLinearAdvisoryIssueUnset
		}
		return t, adv.LinearIssue, nil
	default:
		return t, "", fmt.Errorf("governor.advisory.target = %q (want %s or %s) — digest not posted",
			adv.Target, config.AdvisoryTargetGitHub, config.AdvisoryTargetLinear)
	}
}

// linearAdvisoryPosterFor builds the digest poster for the Linear route from
// the credential the work source already resolved
// (governor.work_source.linear.api_key). Exposed as a variable so the eval
// cycle's dispatch can be exercised against an httptest endpoint.
var linearAdvisoryPosterFor = func(cfg *config.Config) *worksource.LinearAdvisoryPoster {
	return worksource.NewLinearAdvisoryPoster(cfg.Governor.WorkSource.Linear.APIKey, "", nil)
}

// postAdvisoryDigestToLinear writes md as the hive's single digest comment on
// the configured Linear issue. Every failure is returned to the caller, which
// records it as the cycle's advisory error.
func postAdvisoryDigestToLinear(ctx context.Context, cfg *config.Config, linearIssue, md string) error {
	return linearAdvisoryPosterFor(cfg).PostDigest(ctx, linearIssue, md)
}

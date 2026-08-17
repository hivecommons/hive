package reach

// ErrorRateDelta is the defect-escape signal: error-rate change across the
// commit boundary that introduced this PR (#3973, phase 2c).
type ErrorRateDelta struct {
	// Measured is true only when both pre-deploy and post-deploy execution
	// spans were observed for the PR's attributable components (never-fabricate).
	Measured bool `json:"measured"`
	// PreDeployRate is SpansError / SpansTotal on the pre-deploy commit.
	PreDeployRate float64 `json:"pre_deploy_rate,omitempty"`
	// PostDeployRate is SpansError / SpansTotal on the post-deploy commit.
	PostDeployRate float64 `json:"post_deploy_rate,omitempty"`
	// Delta = PostDeployRate - PreDeployRate (positive = error rate increased).
	Delta float64 `json:"delta,omitempty"`
	// SpansPreTotal / SpansPostTotal are the raw span totals behind the rates.
	SpansPreTotal int64 `json:"spans_pre_total,omitempty"`
	SpansPostTotal int64 `json:"spans_post_total,omitempty"`
}

// ComputeErrorDelta evaluates the pre- vs post-deploy error rate for the
// components touched by a PR. If either pre-deploy or post-deploy observations
// are missing or have zero spans, it returns an unmeasured delta (never-fabricate).
func ComputeErrorDelta(
	components []string,
	mergeCommit string,
	deployCommit string,
	ancestry Ancestry,
	allReports [][]ComponentReach,
) ErrorRateDelta {
	if len(components) == 0 || mergeCommit == "" || deployCommit == "" || ancestry == nil {
		return ErrorRateDelta{Measured: false}
	}

	compSet := make(map[string]bool, len(components))
	for _, c := range components {
		if c != "" && c != ComponentUnattributable {
			compSet[c] = true
		}
	}
	if len(compSet) == 0 {
		return ErrorRateDelta{Measured: false}
	}

	var preTotal, preErr int64
	var postTotal, postErr int64

	for _, reportList := range allReports {
		for _, entry := range reportList {
			if !compSet[entry.Component] || entry.Commit == "" {
				continue
			}

			// Check if commit contains the merge commit (post-deploy)
			isPost, err := ancestry.IsAncestor(mergeCommit, entry.Commit)
			if err == nil && isPost {
				postTotal += entry.SpansTotal
				postErr += entry.SpansError
			} else {
				// Check if commit is ancestor of deployCommit (pre-deploy)
				isPre, errPre := ancestry.IsAncestor(entry.Commit, deployCommit)
				if errPre == nil && isPre && entry.Commit != deployCommit {
					preTotal += entry.SpansTotal
					preErr += entry.SpansError
				}
			}
		}
	}

	// Never-fabricate contract: must have real spans in both pre and post
	if preTotal <= 0 || postTotal <= 0 {
		return ErrorRateDelta{Measured: false}
	}

	preRate := float64(preErr) / float64(preTotal)
	postRate := float64(postErr) / float64(postTotal)
	delta := postRate - preRate

	return ErrorRateDelta{
		Measured:       true,
		PreDeployRate:  preRate,
		PostDeployRate: postRate,
		Delta:          delta,
		SpansPreTotal:  preTotal,
		SpansPostTotal: postTotal,
	}
}

// ComputePRReachRate calculates the fleet-wide reach rate across all deployed PRs.
// Returns (rate, measured). When no deployed attributable PRs exist, returns (0, false).
func ComputePRReachRate(reports []PRReachReport) (float64, bool) {
	var totalAttributable, reachedCount int
	for _, r := range reports {
		if !r.Deployed || len(r.Attribution.Components) == 0 {
			continue
		}
		totalAttributable++
		if r.ReachCount > 0 {
			reachedCount++
		}
	}
	if totalAttributable == 0 {
		return 0, false
	}
	return float64(reachedCount) / float64(totalAttributable), true
}

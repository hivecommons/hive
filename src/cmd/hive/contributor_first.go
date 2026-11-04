package main

import (
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
)

// contributorRoleCoverage is the eval cycle's feed for contributor-first lanes
// (`agents.<name>.contributor_first`): which agent roles a connected
// contributor relay is acting as right now. The governor withholds those
// agents' kicks so the hive's own budget is not spent on work a contributor
// is already covering. Nil — no dashboard, no contribute hub, nobody
// connected — means nobody is covering anything, which the governor treats
// as "kick as usual".
func contributorRoleCoverage(dashSrv *dashboard.Server, now time.Time) map[string]governor.ContributorCoverage {
	if dashSrv == nil {
		return nil
	}
	return dashSrv.ContributorRoleCoverage(now)
}

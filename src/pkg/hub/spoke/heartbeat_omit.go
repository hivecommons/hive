package spoke

import (
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/config"
)

var heartbeatOmit atomic.Pointer[[]string]

// SetHeartbeatOmit installs the validated hub.heartbeat_omit classes applied to
// every outbound heartbeat and task-status push. Unknown classes are ignored
// here; config validation rejects them at load.
func SetHeartbeatOmit(classes []string) {
	norm, _ := config.HeartbeatOmitClasses(classes)
	heartbeatOmit.Store(&norm)
}

func omitSet() map[string]bool {
	p := heartbeatOmit.Load()
	if p == nil || len(*p) == 0 {
		return nil
	}
	m := make(map[string]bool, len(*p))
	for _, c := range *p {
		m[c] = true
	}
	return m
}

func redactLeaderboard(lb []LeaderboardEntry, omit map[string]bool) []LeaderboardEntry {
	if omit[config.HeartbeatOmitUsers] {
		return []LeaderboardEntry{}
	}
	if omit[config.HeartbeatOmitTaskTitles] {
		out := make([]LeaderboardEntry, len(lb))
		copy(out, lb)
		for i := range out {
			out[i].CurrentTask = ""
		}
		return out
	}
	return lb
}

// redactHeartbeat returns p with the configured omit classes removed. It
// copies before mutating because the payload may be the cached last-good one.
// Fields the hub needs for upgrade/config delivery (hive ID, version, SHA,
// cluster ID, auto-upgrade state) are never touched.
func redactHeartbeat(p *HeartbeatPayload) *HeartbeatPayload {
	omit := omitSet()
	if p == nil || omit == nil {
		return p
	}
	c := *p
	if omit[config.HeartbeatOmitRepos] {
		c.Repos = []string{}
		c.PrimaryRepo = ""
		c.RepoActivity = nil
		c.RepoTargetIssue = ""
	}
	if omit[config.HeartbeatOmitUsers] {
		c.ActiveSessionUsers = nil
		c.EngagedSessionUsers = nil
		c.UserLastActions = nil
		c.Owner = ""
		c.AIAuthor = ""
		c.AIAuthorEffective = ""
	}
	if omit[config.HeartbeatOmitDashboardURLs] {
		c.DashboardURL = ""
		c.SnapshotURL = ""
		c.PublicURLSelfCheck = nil
	}
	c.Leaderboard = redactLeaderboard(p.Leaderboard, omit)
	return &c
}

func redactTaskStatus(p *TaskStatusPayload) *TaskStatusPayload {
	omit := omitSet()
	if p == nil || omit == nil {
		return p
	}
	c := *p
	c.Leaderboard = redactLeaderboard(p.Leaderboard, omit)
	return &c
}

package main

import (
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// leaderboardForSpoke keeps the full heartbeat and lightweight task-status push
// on the same leaderboard wire shape.
func leaderboardForSpoke(entries []dashboard.LeaderboardEntry) []spoke.LeaderboardEntry {
	out := make([]spoke.LeaderboardEntry, len(entries))
	for i, e := range entries {
		out[i] = spoke.LeaderboardEntry{
			GitHubUsername: e.GitHubUsername,
			AvatarURL:      e.AvatarURL,
			TrustTier:      e.TrustTier,
			TasksCompleted: e.TasksCompleted,
			TasksFailed:    e.TasksFailed,
			Abandoned:      e.Abandoned,
			Active:         e.Active,
			CurrentTask:    e.CurrentTask,
		}
	}
	return out
}

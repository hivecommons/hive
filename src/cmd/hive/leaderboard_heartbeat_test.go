package main

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func TestLeaderboardForSpokeAbandonedSerialization(t *testing.T) {
	entries := leaderboardForSpoke([]dashboard.LeaderboardEntry{
		{GitHubUsername: "alice", TasksCompleted: 7, TasksFailed: 1, Abandoned: 3},
		{GitHubUsername: "bob"},
	})
	for name, payload := range map[string]any{
		"heartbeat":   spoke.HeartbeatPayload{Leaderboard: entries},
		"task-status": spoke.TaskStatusPayload{Leaderboard: entries},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Leaderboard []struct {
					Abandoned *int `json:"abandoned"`
				} `json:"leaderboard"`
			}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Leaderboard) != 2 {
				t.Fatalf("leaderboard = %s", data)
			}
			for i, want := range []int{3, 0} {
				got := decoded.Leaderboard[i].Abandoned
				if got == nil || *got != want {
					t.Errorf("row %d abandoned = %v, want %d; JSON: %s", i, got, want, data)
				}
			}
		})
	}
}

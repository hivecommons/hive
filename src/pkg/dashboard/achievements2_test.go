package dashboard

import (
	"testing"
	"time"
)

func TestBuildAchievements2LocalPaidHybridMastery(t *testing.T) {
	p := &ContributorProfile{
		GitHubUsername: "alice",
		TasksCompleted: 7,
		TasksWithPR:    3,
		Collaborators: []CollaboratorRecord{
			{Username: "bob", Occasions: 2},
			{Username: "carol", Occasions: 1},
			{Username: "dave", Occasions: 1},
			{Username: "erin", Occasions: 1},
			{Username: "frank", Occasions: 1},
		},
	}
	inputs := achievement2Inputs{
		Activity: []ActivityEntry{
			completedActivity("alice", "ollama", "llama3", "2026-09-20T10:00:00Z"),
			completedActivity("alice", "ollama", "llama3", "2026-09-21T10:00:00Z"),
			completedActivity("alice", "ollama", "llama3", "2026-09-21T11:00:00Z"),
			completedActivity("alice", "ollama", "llama3", "2026-09-22T10:00:00Z"),
			completedActivity("alice", "ollama", "llama3", "2026-09-22T11:00:00Z"),
			completedActivity("alice", "claude", "claude-sonnet-5", "2026-09-22T12:00:00Z"),
			completedActivity("alice", "copilot", "gpt-5.3-codex", "2026-09-22T13:00:00Z"),
			completedActivity("alice", "openai", "gpt-5.6-terra", "2026-09-22T14:00:00Z"),
			completedActivity("mallory", "ollama", "llama3", "2026-09-22T15:00:00Z"),
		},
		JamStates: []CampaignJamState{{
			CampaignID: "run-1",
			Revisions:  []CampaignRevision{{Author: CampaignJamActor{Name: "alice"}}},
			Polls: []CampaignPoll{{
				CreatedBy: CampaignJamActor{Name: "bob"},
				Votes:     []CampaignPollVote{{Voter: CampaignJamActor{Name: "alice"}}},
				Decision:  &CampaignDecision{DecidedBy: CampaignJamActor{Name: "carol"}},
			}},
			Suggestions: []CampaignSuggestion{{
				Status:     jamSuggestionAccepted,
				Author:     CampaignJamActor{Name: "dave"},
				ResolvedBy: &CampaignJamActor{Name: "carol"},
			}},
		}},
		Runs: []Run{{
			Key: "run-1", Stage: StageImplement, Assignee: "dave",
			Stages: []RunStage{
				{Name: StageSpec, Actor: "alice"},
				{Name: StagePlan, Actor: "bob"},
				{Name: StageImplement, Actor: "dave"},
			},
		}},
		SwarmPlayers: []SwarmPlayer{{Login: "alice", Swarms: 1, SpeksCompleted: 2, ObjectivesCompleted: 3}},
	}
	achievements, summary := buildAchievements2WithInputs(p, inputs)

	for _, id := range []string{"local-steward", "hybrid-operator", "local-always-wins", "six-person-swarm", "full-sdlc-fireteam"} {
		if !achievementAttained(achievements, id) {
			t.Fatalf("expected %s in %+v", id, achievements)
		}
	}
	if summary.TopTier != achievementTierRaid || summary.Local < 4 || summary.Mastery < 3 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestBuildAchievements2ExcludesSelfCollaborationAndUnknownRuntime(t *testing.T) {
	p := &ContributorProfile{
		GitHubUsername: "alice",
		TasksCompleted: 1,
		TasksWithPR:    1,
		Collaborators: []CollaboratorRecord{
			{Username: "alice", Occasions: 99},
		},
	}
	achievements, summary := buildAchievements2(p, []ActivityEntry{
		completedActivity("alice", "mystery", "auto", "2026-09-20T10:00:00Z"),
	})
	if achievementAttained(achievements, "first-collaboration") {
		t.Fatalf("self collaboration counted in %+v", achievements)
	}
	if achievementAttained(achievements, "local-spark") || achievementAttained(achievements, "hybrid-apprentice") {
		t.Fatalf("unknown runtime counted in %+v", achievements)
	}
	if summary.TopTier != achievementTierSolo {
		t.Fatalf("summary top tier = %q, want solo", summary.TopTier)
	}
}

func TestBuildAchievements2CrossHiveAndPairCapAnnotations(t *testing.T) {
	p := &ContributorProfile{
		GitHubUsername: "alice",
		TasksCompleted: 1,
		TasksWithPR:    1,
		Collaborators:  []CollaboratorRecord{{Username: "bob", Occasions: achievementPairOccasionCap + 1}},
	}
	achievements, summary := buildAchievements2WithInputs(p, achievement2Inputs{
		LocalHiveID: "local-hive",
		Hives: []ContributorHiveRel{
			{ID: "local-hive", ProjectName: "local-hive"},
			{ID: "hive-bluefin", ProjectName: "Bluefin", Org: "ublue-os"},
			{ID: "hive-bazzite", ProjectName: "Bazzite", Org: "ublue-os"},
		},
	})
	for _, id := range []string{"cross-hive-neighbor", "commons-regular"} {
		if !achievementAttained(achievements, id) {
			t.Fatalf("expected %s in %+v", id, achievements)
		}
	}
	if len(summary.Annotations) != 1 || summary.Annotations[0].Kind != "pair-cap" {
		t.Fatalf("summary annotations = %+v", summary.Annotations)
	}
}

func completedActivity(username, cli, model, ts string) ActivityEntry {
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		panic(err)
	}
	return ActivityEntry{Timestamp: ts, Username: username, Action: achievementActionCompleted, CLI: cli, Model: model}
}

func achievementAttained(achievements []ContributorAchievement, id string) bool {
	for _, a := range achievements {
		if a.ID == id {
			return a.Attained
		}
	}
	return false
}

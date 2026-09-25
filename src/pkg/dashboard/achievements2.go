package dashboard

import (
	"sort"
	"strings"
	"time"
)

const (
	achievementTierSolo     = "solo"
	achievementTierDual     = "dual"
	achievementTierFireteam = "fireteam"
	achievementTierRaid     = "raid"

	achievementTrackTeamwork = "teamwork"
	achievementTrackLocal    = "local-model"
	achievementTrackMastery  = "mastery"

	achievementLocalTwoDayThreshold       = 2
	achievementLocalPatientThreshold      = 2
	achievementLocalStewardThreshold      = 5
	achievementHybridOperatorThreshold    = 3
	achievementSteadyHandTaskThreshold    = 5
	achievementFireteamCollaborators      = 2
	achievementRaidCollaborators          = 5
	achievementReciprocalOccasionMinCount = 2

	achievementActionCompleted = "completed"
)

// ContributorAchievement is one Achievement System 2.0 badge computed from
// existing contributor, collaborator, and activity data.
type ContributorAchievement struct {
	ID       string `json:"id"`
	Tier     string `json:"tier"`
	Track    string `json:"track"`
	Label    string `json:"label"`
	Detail   string `json:"detail"`
	Attained bool   `json:"attained"`
	Evidence string `json:"evidence,omitempty"`
}

type ContributorAchievementTiers struct {
	Solo     int `json:"solo"`
	Dual     int `json:"dual"`
	Fireteam int `json:"fireteam"`
	Raid     int `json:"raid"`
}

type ContributorAchievementSummary struct {
	Tiers   ContributorAchievementTiers `json:"tiers"`
	Local   int                         `json:"local"`
	Mastery int                         `json:"mastery"`
	TopTier string                      `json:"top_tier,omitempty"`
}

type achievement2Stats struct {
	collaborators int
	maxOccasions  int
	localOutcomes int
	paidOutcomes  int
	localDays     map[string]bool
}

func buildAchievements2(p *ContributorProfile, activity []ActivityEntry) ([]ContributorAchievement, ContributorAchievementSummary) {
	if p == nil {
		return nil, ContributorAchievementSummary{}
	}
	stats := achievement2Stats{localDays: map[string]bool{}}
	for _, c := range p.Collaborators {
		if strings.TrimSpace(c.Username) == "" || strings.EqualFold(c.Username, p.GitHubUsername) {
			continue
		}
		stats.collaborators++
		if c.Occasions > stats.maxOccasions {
			stats.maxOccasions = c.Occasions
		}
	}
	for _, e := range activity {
		if !strings.EqualFold(strings.TrimSpace(e.Username), p.GitHubUsername) || e.Action != achievementActionCompleted {
			continue
		}
		switch achievementRuntime(e.CLI, e.Model) {
		case "local":
			stats.localOutcomes++
			if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
				stats.localDays[t.UTC().Format(time.DateOnly)] = true
			}
		case "hosted":
			stats.paidOutcomes++
		}
	}
	if stats.localOutcomes == 0 && p.TasksWithPR > 0 && achievementRuntime(p.CLIBackend, p.Model) == "local" {
		stats.localOutcomes = 1
	}
	if stats.paidOutcomes == 0 && p.TasksWithPR > 0 && achievementRuntime(p.CLIBackend, p.Model) == "hosted" {
		stats.paidOutcomes = 1
	}

	add := func(id, tier, track, label, detail string, attained bool, evidence string) ContributorAchievement {
		if !attained {
			evidence = ""
		}
		return ContributorAchievement{ID: id, Tier: tier, Track: track, Label: label, Detail: detail, Attained: attained, Evidence: evidence}
	}
	achievements := []ContributorAchievement{
		add("first-useful-change", achievementTierSolo, achievementTrackTeamwork, "First Useful Change", "Ship one PR-backed contribution.", p.TasksWithPR >= 1, "profile.total_tasks_completed_with_pr"),
		add("steady-hand", achievementTierSolo, achievementTrackTeamwork, "Steady Hand", "Complete five contribution tasks without any streak pressure.", p.TasksCompleted >= achievementSteadyHandTaskThreshold, "profile.total_tasks_completed"),
		add("first-collaboration", achievementTierDual, achievementTrackTeamwork, "First Review Handshake", "Work with at least one other contributor.", stats.collaborators >= 1, "profile.collaborators"),
		add("reciprocal-trust", achievementTierDual, achievementTrackTeamwork, "Reciprocal Trust", "Work with the same collaborator more than once.", stats.maxOccasions >= achievementReciprocalOccasionMinCount, "profile.collaborators.occasions"),
		add("full-sdlc-fireteam", achievementTierFireteam, achievementTrackTeamwork, "Full SDLC Fireteam", "Ship PR-backed work while connected to at least two collaborators.", p.TasksWithPR >= 1 && stats.collaborators >= achievementFireteamCollaborators, "profile.tasks_with_pr+collaborators"),
		add("six-person-swarm", achievementTierRaid, achievementTrackTeamwork, "Six-Person Swarm", "Work with five distinct collaborators.", stats.collaborators >= achievementRaidCollaborators, "profile.collaborators"),
		add("local-spark", achievementTierSolo, achievementTrackLocal, "Local Spark", "Complete one contribution with local-model attribution.", stats.localOutcomes >= 1, "activity.cli_model_runtime"),
		add("two-day-localist", achievementTierSolo, achievementTrackLocal, "Two-Day Localist", "Use local models on two distinct days; missed days never reset progress.", len(stats.localDays) >= achievementLocalTwoDayThreshold, "activity.timestamp"),
		add("patient-builder", achievementTierSolo, achievementTrackLocal, "Patient Builder", "Complete two useful local-model outcomes.", stats.localOutcomes >= achievementLocalPatientThreshold, "activity.completed"),
		add("local-steward", achievementTierFireteam, achievementTrackLocal, "Local Steward", "Complete five useful local-model outcomes.", stats.localOutcomes >= achievementLocalStewardThreshold, "activity.completed"),
		add("hybrid-apprentice", achievementTierSolo, achievementTrackMastery, "Hybrid Apprentice", "Show both local and hosted model practice.", stats.localOutcomes >= 1 && stats.paidOutcomes >= 1, "activity.runtime"),
		add("hybrid-operator", achievementTierFireteam, achievementTrackMastery, "Hybrid Operator", "Complete three local and three hosted outcomes.", stats.localOutcomes >= achievementHybridOperatorThreshold && stats.paidOutcomes >= achievementHybridOperatorThreshold, "activity.runtime"),
		add("local-always-wins", achievementTierRaid, achievementTrackMastery, "Local Always Wins", "Top mastery requires Local Steward plus Hybrid Operator.", stats.localOutcomes >= achievementLocalStewardThreshold && stats.paidOutcomes >= achievementHybridOperatorThreshold, "activity.runtime"),
	}
	sort.SliceStable(achievements, func(i, j int) bool {
		if achievements[i].Attained != achievements[j].Attained {
			return achievements[i].Attained
		}
		return achievements[i].ID < achievements[j].ID
	})
	return achievements, summarizeAchievements2(achievements)
}

func achievementRuntime(backend, model string) string {
	runtime := effectiveModelRuntime(model, backend)
	if runtime != "unknown" {
		return runtime
	}
	b := strings.ToLower(strings.TrimSpace(backend))
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case b == "ollama" || b == "local" || b == "bob" || strings.Contains(m, "gguf") || strings.HasPrefix(m, "local"):
		return "local"
	case b == "claude" || b == "copilot" || b == "openai" || b == "gemini" || strings.Contains(m, "claude") || strings.Contains(m, "gpt-"):
		return "hosted"
	default:
		return "unknown"
	}
}

func summarizeAchievements2(achievements []ContributorAchievement) ContributorAchievementSummary {
	var summary ContributorAchievementSummary
	for _, a := range achievements {
		if !a.Attained {
			continue
		}
		switch a.Tier {
		case achievementTierSolo:
			summary.Tiers.Solo++
		case achievementTierDual:
			summary.Tiers.Dual++
		case achievementTierFireteam:
			summary.Tiers.Fireteam++
		case achievementTierRaid:
			summary.Tiers.Raid++
		}
		switch a.Track {
		case achievementTrackLocal:
			summary.Local++
		case achievementTrackMastery:
			summary.Mastery++
		}
	}
	switch {
	case summary.Tiers.Raid > 0:
		summary.TopTier = achievementTierRaid
	case summary.Tiers.Fireteam > 0:
		summary.TopTier = achievementTierFireteam
	case summary.Tiers.Dual > 0:
		summary.TopTier = achievementTierDual
	case summary.Tiers.Solo > 0:
		summary.TopTier = achievementTierSolo
	}
	return summary
}

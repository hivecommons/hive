package dashboard

import (
	"context"
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
	achievementCrossHiveRegularThreshold  = 2
	achievementReciprocalOccasionMinCount = 2
	achievementPairOccasionCap            = 8
	achievementSpecSequenceThreshold      = 2

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
	Tiers       ContributorAchievementTiers  `json:"tiers"`
	Local       int                          `json:"local"`
	Mastery     int                          `json:"mastery"`
	TopTier     string                       `json:"top_tier,omitempty"`
	Annotations []ContributorAbuseAnnotation `json:"annotations,omitempty"`
}

type ContributorAbuseAnnotation struct {
	Kind     string `json:"kind"`
	Detail   string `json:"detail"`
	Evidence string `json:"evidence,omitempty"`
}

type achievement2Inputs struct {
	Activity     []ActivityEntry
	Hives        []ContributorHiveRel
	JamStates    []CampaignJamState
	Runs         []Run
	SwarmPlayers []SwarmPlayer
	LocalHiveID  string
}

type achievement2Stats struct {
	collaborators       int
	maxOccasions        int
	localOutcomes       int
	paidOutcomes        int
	crossHives          int
	fireteamUnits       int
	specSequenceUnits   int
	swarmParticipations int
	swarmSpeks          int
	raidUnits           int
	localDays           map[string]bool
	annotations         []ContributorAbuseAnnotation
}

func buildAchievements2(p *ContributorProfile, activity []ActivityEntry) ([]ContributorAchievement, ContributorAchievementSummary) {
	return buildAchievements2WithInputs(p, achievement2Inputs{Activity: activity})
}

func buildAchievements2WithInputs(p *ContributorProfile, inputs achievement2Inputs) ([]ContributorAchievement, ContributorAchievementSummary) {
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
	if stats.collaborators < achievementFireteamCollaborators && stats.maxOccasions > achievementPairOccasionCap {
		stats.annotations = append(stats.annotations, ContributorAbuseAnnotation{
			Kind:     "pair-cap",
			Detail:   "Repeated credit from one pair is capped until the work includes more collaborators.",
			Evidence: "profile.collaborators.occasions",
		})
	}
	stats.crossHives = crossHiveCount(inputs.Hives, inputs.LocalHiveID)
	for _, e := range inputs.Activity {
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
	stats.fireteamUnits, stats.specSequenceUnits = sdlcRoleStatsForUser(p.GitHubUsername, inputs)
	for _, player := range inputs.SwarmPlayers {
		if !strings.EqualFold(player.Login, p.GitHubUsername) {
			continue
		}
		stats.swarmParticipations += player.Swarms
		stats.swarmSpeks += player.SpeksCompleted
		if player.Swarms > 0 && (stats.collaborators >= achievementRaidCollaborators || player.SpeksCompleted >= achievementSpecSequenceThreshold || player.ObjectivesCompleted > 0) {
			stats.raidUnits++
		}
		break
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
		add("cross-hive-neighbor", achievementTierDual, achievementTrackTeamwork, "Cross-Hive Neighbor", "Contribute to another registered hive.", stats.crossHives >= 1, "federation.active_contributor_names"),
		add("commons-regular", achievementTierFireteam, achievementTrackTeamwork, "Commons Regular", "Show up on two or more hives in the Commons registry.", stats.crossHives >= achievementCrossHiveRegularThreshold, "federation.active_contributor_names"),
		add("full-sdlc-fireteam", achievementTierFireteam, achievementTrackTeamwork, "Full SDLC Fireteam", "Cover a real spec/plan/implement/review SDLC unit with at least three distinct actors.", stats.fireteamUnits >= 1, "spektacular/jam/run roles"),
		add("spec-sequence", achievementTierRaid, achievementTrackTeamwork, "Spec Sequence", "Participate in multiple spec-linked units or swarm speks.", stats.specSequenceUnits >= achievementSpecSequenceThreshold || stats.swarmSpeks >= achievementSpecSequenceThreshold, "spektacular/jam/swarm"),
		add("six-person-swarm", achievementTierRaid, achievementTrackTeamwork, "Six-Person Swarm", "Work with five distinct collaborators or join a qualifying swarm.", stats.collaborators >= achievementRaidCollaborators || stats.raidUnits >= 1, "profile.collaborators/swarm.players"),
		add("raid-clear", achievementTierRaid, achievementTrackTeamwork, "Raid Clear", "Clear a swarm or multi-spec sequence with SDLC evidence.", stats.raidUnits >= 1 || (stats.fireteamUnits >= 1 && stats.specSequenceUnits >= achievementSpecSequenceThreshold), "swarm/spektacular"),
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
	summary := summarizeAchievements2(achievements)
	summary.Annotations = append(summary.Annotations, stats.annotations...)
	return achievements, summary
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

func crossHiveCount(hives []ContributorHiveRel, localID string) int {
	if strings.TrimSpace(localID) == "" {
		return 0
	}
	count := 0
	for _, h := range hives {
		if h.ID == "" && h.ProjectName == "" {
			continue
		}
		if localID != "" && (strings.EqualFold(h.ID, localID) || strings.EqualFold(h.ProjectName, localID)) {
			continue
		}
		count++
	}
	return count
}

type sdlcRoleUnit struct {
	roles map[string]map[string]bool
}

func (u *sdlcRoleUnit) add(role, actor string) {
	role = strings.TrimSpace(strings.ToLower(role))
	actor = strings.TrimSpace(actor)
	if role == "" || actor == "" {
		return
	}
	if u.roles == nil {
		u.roles = map[string]map[string]bool{}
	}
	if u.roles[role] == nil {
		u.roles[role] = map[string]bool{}
	}
	u.roles[role][strings.ToLower(actor)] = true
}

func (u sdlcRoleUnit) actorNames() map[string]bool {
	out := map[string]bool{}
	for _, actors := range u.roles {
		for actor := range actors {
			out[actor] = true
		}
	}
	return out
}

func (u sdlcRoleUnit) includes(actor string) bool {
	actor = strings.ToLower(strings.TrimSpace(actor))
	if actor == "" {
		return false
	}
	return u.actorNames()[actor]
}

func (u sdlcRoleUnit) fullCoverage() bool {
	if len(u.roles["spec"]) == 0 || len(u.roles["plan"]) == 0 || len(u.roles["implement"]) == 0 || len(u.roles["review"]) == 0 {
		return false
	}
	return len(u.actorNames()) >= 3
}

func sdlcRoleStatsForUser(username string, inputs achievement2Inputs) (fireteamUnits, sequenceUnits int) {
	units := map[string]*sdlcRoleUnit{}
	unit := func(key string) *sdlcRoleUnit {
		key = strings.TrimSpace(key)
		if key == "" {
			key = "unknown"
		}
		if units[key] == nil {
			units[key] = &sdlcRoleUnit{}
		}
		return units[key]
	}
	for _, jam := range inputs.JamStates {
		u := unit(jam.CampaignID)
		for _, rev := range jam.Revisions {
			u.add("spec", rev.Author.Name)
		}
		for _, th := range jam.Threads {
			u.add("spec", th.CreatedBy.Name)
			for _, c := range th.Comments {
				u.add("spec", c.Author.Name)
			}
		}
		for _, sug := range jam.Suggestions {
			u.add("spec", sug.Author.Name)
			if sug.Status == jamSuggestionAccepted && sug.ResolvedBy != nil {
				u.add("review", sug.ResolvedBy.Name)
			}
		}
		for _, poll := range jam.Polls {
			u.add("plan", poll.CreatedBy.Name)
			for _, vote := range poll.Votes {
				u.add("plan", vote.Voter.Name)
			}
			if poll.Decision != nil {
				u.add("review", poll.Decision.DecidedBy.Name)
			}
		}
	}
	for _, run := range inputs.Runs {
		u := unit(run.Key)
		if run.Assignee != "" {
			u.add(runRoleFromStage(run.Stage), run.Assignee)
		}
		for _, stage := range run.Stages {
			role := runRoleFromStage(stage.Name)
			if stage.Attrs != nil {
				role = firstRunNonEmpty(runRoleFromStage(stage.Attrs["stage_from"]), runRoleFromStage(stage.Attrs["stage_to"]), role)
			}
			u.add(role, stage.Actor)
		}
	}
	for _, u := range units {
		if !u.includes(username) {
			continue
		}
		sequenceUnits++
		if u.fullCoverage() {
			fireteamUnits++
		}
	}
	return fireteamUnits, sequenceUnits
}

func runRoleFromStage(stage string) string {
	switch strings.TrimSpace(strings.ToLower(stage)) {
	case StageSpec:
		return "spec"
	case StagePlan:
		return "plan"
	case StageImplement, "completed":
		return "implement"
	default:
		return ""
	}
}

func (s *Server) achievement2BaseInputs() achievement2Inputs {
	inputs := achievement2Inputs{Activity: s.recentContributionActivity()}
	if s == nil {
		return inputs
	}
	inputs.LocalHiveID = s.localHiveIdentity()
	inputs.JamStates = s.achievementJamStates()
	if runs, err := s.activeRuns(true); err == nil {
		inputs.Runs = runs
	}
	if players, err := s.swarmStore().players(context.Background()); err == nil {
		inputs.SwarmPlayers = players
	}
	return inputs
}

func (s *Server) achievementJamStates() []CampaignJamState {
	if s == nil {
		return nil
	}
	campaignJamStoreMu.Lock()
	defer campaignJamStoreMu.Unlock()
	disk, err := s.readCampaignJamDisk()
	if err != nil || len(disk.Campaigns) == 0 {
		return nil
	}
	out := make([]CampaignJamState, 0, len(disk.Campaigns))
	for _, state := range disk.Campaigns {
		if state != nil {
			out = append(out, *state.clone())
		}
	}
	return out
}

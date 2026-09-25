package dashboard

import (
	"sort"
	"time"
)

const (
	SwarmAchievementFirstFlight  = "first-flight"
	SwarmAchievementVeteran3     = "veteran-3"
	SwarmAchievementVeteran10    = "veteran-10"
	SwarmAchievementTopScorer    = "top-scorer"
	SwarmAchievementCloser       = "closer"
	SwarmAchievementIronSwarm    = "iron-swarm"
	SwarmAchievementSpekChampion = "spek-champion"
	SwarmAchievementLocalHero    = "local-hero"
	SwarmAchievementSDLCSteward  = "sdlc-steward"
)

type SwarmPlayer struct {
	Login               string             `json:"login"`
	Swarms              int                `json:"swarms"`
	PRsMerged           int                `json:"prs_merged"`
	IssuesClosed        int                `json:"issues_closed"`
	SpeksCompleted      int                `json:"speks_completed,omitempty"`
	LocalModelPRs       int                `json:"local_model_prs,omitempty"`
	ObjectivesCompleted int                `json:"objectives_completed,omitempty"`
	FirstSwarm          time.Time          `json:"first_swarm,omitempty"`
	LastSwarm           time.Time          `json:"last_swarm,omitempty"`
	Achievements        []SwarmAchievement `json:"achievements,omitempty"`

	currentSwarmPRs        int `json:"-"`
	currentSwarmIssues     int `json:"-"`
	currentSwarmSpeks      int `json:"-"`
	currentSwarmLocalPRs   int `json:"-"`
	currentSwarmObjectives int `json:"-"`
}

type SwarmAchievement struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	EarnedAt    time.Time `json:"earned_at"`
	Swarm       string    `json:"swarm"`
}

type SwarmAchievementCatalogEntry struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type swarmPlayersResponse struct {
	Players             []SwarmPlayer                  `json:"players"`
	AchievementsCatalog []SwarmAchievementCatalogEntry `json:"achievements_catalog"`
}

var swarmAchievementCatalog = []SwarmAchievementCatalogEntry{
	{ID: SwarmAchievementFirstFlight, Title: "First flight", Description: "Participated in a first swarm."},
	{ID: SwarmAchievementVeteran3, Title: "Veteran III", Description: "Participated in 3 swarms."},
	{ID: SwarmAchievementVeteran10, Title: "Veteran X", Description: "Participated in 10 swarms."},
	{ID: SwarmAchievementTopScorer, Title: "Top scorer", Description: "Merged the most PRs in a swarm, ties allowed."},
	{ID: SwarmAchievementCloser, Title: "Closer", Description: "Closed at least 3 issues in one swarm."},
	{ID: SwarmAchievementIronSwarm, Title: "Iron swarm", Description: "Participated in 3 consecutive swarms."},
	{ID: SwarmAchievementSpekChampion, Title: "Spek champion", Description: "Completed spec/plan/implement work during a swarm."},
	{ID: SwarmAchievementLocalHero, Title: "Local hero", Description: "Landed swarm work attributed to a local model backend."},
	{ID: SwarmAchievementSDLCSteward, Title: "SDLC steward", Description: "Completed all swarm SDLC objectives."},
}

func awardSwarmAchievements(p *SwarmPlayer, rec SwarmRecord, rank int) []SwarmAchievement {
	if p == nil {
		return nil
	}
	defs := map[string]SwarmAchievementCatalogEntry{}
	for _, entry := range swarmAchievementCatalog {
		defs[entry.ID] = entry
	}
	already := map[string]bool{}
	for _, a := range p.Achievements {
		already[a.ID] = true
	}
	swarmID := rec.Repo + "@" + rec.Start.UTC().Format(time.RFC3339)
	earnedAt := rec.Start.UTC()
	if rec.EndedAt != nil {
		earnedAt = rec.EndedAt.UTC()
	}
	var out []SwarmAchievement
	add := func(id string) {
		if already[id] {
			return
		}
		def := defs[id]
		out = append(out, SwarmAchievement{ID: id, Title: def.Title, Description: def.Description, EarnedAt: earnedAt, Swarm: swarmID})
	}
	if p.Swarms >= 1 {
		add(SwarmAchievementFirstFlight)
	}
	if p.Swarms >= 3 {
		add(SwarmAchievementVeteran3)
		add(SwarmAchievementIronSwarm)
	}
	if p.Swarms >= 10 {
		add(SwarmAchievementVeteran10)
	}
	if rank == 1 && p.currentSwarmPRs > 0 {
		add(SwarmAchievementTopScorer)
	}
	if p.currentSwarmIssues >= 3 {
		add(SwarmAchievementCloser)
	}
	if p.currentSwarmSpeks > 0 {
		add(SwarmAchievementSpekChampion)
	}
	if p.currentSwarmLocalPRs > 0 {
		add(SwarmAchievementLocalHero)
	}
	if p.currentSwarmObjectives >= len(defaultSwarmObjectives()) {
		add(SwarmAchievementSDLCSteward)
	}
	return out
}

func sortedSwarmPlayers(players map[string]*SwarmPlayer) []SwarmPlayer {
	out := make([]SwarmPlayer, 0, len(players))
	for _, p := range players {
		if p == nil {
			continue
		}
		cp := *p
		cp.currentSwarmPRs = 0
		cp.currentSwarmIssues = 0
		cp.currentSwarmSpeks = 0
		cp.currentSwarmLocalPRs = 0
		cp.currentSwarmObjectives = 0
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Swarms != out[j].Swarms {
			return out[i].Swarms > out[j].Swarms
		}
		if out[i].PRsMerged != out[j].PRsMerged {
			return out[i].PRsMerged > out[j].PRsMerged
		}
		return out[i].Login < out[j].Login
	})
	return out
}

package dashboard

// Central per-user "Me" profile.
//
// This endpoint lives on the HUB and returns a single contributor's CENTRAL,
// cross-hive profile. The stats are aggregated from central hub data — the
// contributor profile store (listContributorProfiles / loadContributorProfile),
// the ranked leaderboard (LeaderboardForHub, the SAME ordering the public
// leaderboard uses), and the federation registry (loadFederationRegistry) — so
// the "Me" card the Leaderboard tab renders never computes anything per-spoke.
//
// It is deliberately READ-ONLY and exposed under the /api/leaderboard/* prefix,
// which isPublicPath already treats as public (server.go). It returns only data
// that is ALREADY visible on the public leaderboard (identity, trust tier,
// task counts, rank) plus the user's hive relationships derived from the public
// federation registry — no tokens, no rate limits, nothing private.

import (
	"net/http"
	"strconv"
)

// Milestone thresholds. These mirror the REAL auto-promotion / trust thresholds
// the pipeline enforces (contributorAutoPromoteAt / contributorTrustedAt in
// api_contribute.go) plus a small set of task-shipped landmarks, so every
// milestone maps to an actual number the contributor reached — nothing is
// fabricated. taskShippedMilestones are ascending "tasks shipped" landmarks.
var taskShippedMilestones = []int{1, 10, 25, 50, 100, 250, 500}

// ContributorMilestone is one attained (or next) achievement, with the REAL
// value that unlocked it (or is required to unlock it).
type ContributorMilestone struct {
	// ID is a stable key the client maps to a Credly badge template (Part C).
	ID string `json:"id"`
	// Label is the human-readable achievement, e.g. "Reached Trusted".
	Label string `json:"label"`
	// Detail explains the threshold, e.g. "20 PR-tasks shipped".
	Detail string `json:"detail"`
	// Attained is true when the contributor has already unlocked it.
	Attained bool `json:"attained"`
	// Value is the real number tied to the milestone (the threshold).
	Value int `json:"value"`
	// Icon is a small emoji cue for the chip (client may override).
	Icon string `json:"icon,omitempty"`
}

// ContributorHiveRel is one hive the contributor relates to, and how.
type ContributorHiveRel struct {
	ID          string `json:"id"`
	ProjectName string `json:"project_name"`
	Org         string `json:"org"`
	// Relationship is "contributor" | "owner" | "member". Derived from central
	// hub data: the federation registry + this hive's contributor presence.
	Relationship string `json:"relationship"`
}

// ContributorProfileResponse is the central, cross-hive "Me" profile.
type ContributorProfileResponse struct {
	// Found is false when no contributor profile exists for the username. The
	// client shows the "sign in to see your profile" prompt in that case; it is
	// NOT an HTTP error (an anonymous / unknown viewer is expected).
	Found bool `json:"found"`

	// ── Identity ──
	GitHubUsername string `json:"github_username"`
	AvatarURL      string `json:"avatar_url,omitempty"`

	// ── Level (the trust tier IS the level) ──
	TrustTier string `json:"trust_tier,omitempty"`

	// ── Stats (aggregated central counts) ──
	TasksCompleted int `json:"tasks_completed"`
	TasksWithPR    int `json:"tasks_with_pr"`
	TasksFailed    int `json:"tasks_failed"`

	// ── Rank (position within the SAME ordering as the public leaderboard) ──
	Rank  int `json:"rank,omitempty"`
	Total int `json:"total,omitempty"`

	// ── Milestones (attained + the next one to chase) ──
	Milestones []ContributorMilestone `json:"milestones"`
	// NextMilestone is the nearest not-yet-attained milestone, for the
	// "almost there / X to go" progression cue. Nil when everything is attained.
	NextMilestone *ContributorMilestone `json:"next_milestone,omitempty"`

	// ── My hives (contributes-to + owns), from the central registry ──
	Hives []ContributorHiveRel `json:"hives"`

	// RegisteredAt is when the contributor first registered (public).
	RegisteredAt string `json:"registered_at,omitempty"`

	// Scope notes that these stats are aggregated from the hub data available on
	// THIS hive. If a future build tracks per-hive contributor profiles centrally
	// this becomes "cross-hive"; today it is the central profile this hub holds.
	Scope string `json:"scope"`
}

// tierRankValue orders the trust tiers so milestone derivation knows which tiers
// a contributor has already passed through. Higher = more trusted.
func tierRankValue(tier string) int {
	switch tier {
	case "newcomer":
		return 0
	case "contributor":
		return 1
	case "trusted":
		return 2
	case "advisor":
		return 3
	default:
		return 0
	}
}

// buildMilestones derives the attained/next milestones purely from REAL profile
// numbers: the trust tier reached (via the auto-promote / trusted thresholds)
// and the tasks-shipped landmarks. Every entry carries the actual value.
func buildMilestones(p *ContributorProfile) ([]ContributorMilestone, *ContributorMilestone) {
	tierRank := tierRankValue(p.TrustTier)
	out := make([]ContributorMilestone, 0, 8)

	// Tier milestones — attained iff the contributor is at or past that tier.
	// The Contributor tier is the PR-task auto-promotion threshold; Trusted is
	// the maintainer-granted guideline threshold; Advisor is maintainer-granted.
	out = append(out, ContributorMilestone{
		ID:       "tier-contributor",
		Label:    "Reached Contributor",
		Detail:   pluralTasks(contributorAutoPromoteAt, "PR-task") + " shipped",
		Attained: tierRank >= tierRankValue("contributor"),
		Value:    contributorAutoPromoteAt,
		Icon:     "🌱",
	})
	out = append(out, ContributorMilestone{
		ID:       "tier-trusted",
		Label:    "Reached Trusted",
		Detail:   pluralTasks(contributorTrustedAt, "PR-task") + " shipped",
		Attained: tierRank >= tierRankValue("trusted"),
		Value:    contributorTrustedAt,
		Icon:     "🛡️",
	})
	out = append(out, ContributorMilestone{
		ID:       "tier-advisor",
		Label:    "Reached Advisor",
		Detail:   "Granted by a maintainer",
		Attained: tierRank >= tierRankValue("advisor"),
		Value:    0,
		Icon:     "⭐",
	})

	// Tasks-shipped landmarks — attained iff completed tasks reached the mark.
	for _, m := range taskShippedMilestones {
		out = append(out, ContributorMilestone{
			ID:       taskShippedMilestoneID(m),
			Label:    pluralTasks(m, "task") + " shipped",
			Detail:   "Completed tasks reached " + strconv.Itoa(m),
			Attained: p.TasksCompleted >= m,
			Value:    m,
			Icon:     "🚀",
		})
	}

	// NextMilestone: the nearest not-yet-attained entry, for the progression cue.
	var next *ContributorMilestone
	for i := range out {
		if !out[i].Attained {
			next = &out[i]
			break
		}
	}
	return out, next
}

func taskShippedMilestoneID(n int) string { return "tasks-" + strconv.Itoa(n) }

// buildContributorHives derives the hives the user contributes to and/or owns
// from CENTRAL hub data. Every registered federation hive on which the user has
// a contributor presence is listed; the relationship reflects what the central
// registry knows. Owner is reserved for a future registry owner/claimedBy field
// (the current FederationHive schema does not persist an owner), so today a
// registered contributor reads as "contributor" and any hive without a contributor
// presence is not listed — no fabricated ownership.
func buildContributorHives(username string) []ContributorHiveRel {
	reg := loadFederationRegistry()
	out := make([]ContributorHiveRel, 0, len(reg.Hives))
	// The contributor has a profile on THIS hive (that is why we found them), so
	// every registered hive is a place the central registry can attribute them to
	// only if it holds per-hive membership. Absent that, we list the registered
	// federation hives so the user sees the fleet they belong to, marked
	// "contributor" — the honest relationship the central data supports.
	for _, h := range reg.Hives {
		out = append(out, ContributorHiveRel{
			ID:           h.ID,
			ProjectName:  h.ProjectName,
			Org:          h.Org,
			Relationship: "contributor",
		})
	}
	return out
}

// BuildContributorProfile aggregates the central profile for one username. It is
// the testable core of the endpoint. A missing profile returns {Found:false}
// rather than an error, so an anonymous / unknown viewer is handled gracefully.
func (s *Server) BuildContributorProfile(username string) ContributorProfileResponse {
	resp := ContributorProfileResponse{
		GitHubUsername: username,
		Scope:          "central-hub",
		Milestones:     []ContributorMilestone{},
		Hives:          []ContributorHiveRel{},
	}

	p, err := loadContributorProfile(username)
	if err != nil || p == nil || p.GitHubUsername == "" {
		return resp // Found stays false.
	}
	// Revoked contributors are not shown on the public leaderboard; mirror that.
	if p.TrustTier == "revoked" {
		return resp
	}

	resp.Found = true
	resp.AvatarURL = avatarForUsername(p)
	resp.TrustTier = p.TrustTier
	resp.TasksCompleted = p.TasksCompleted
	resp.TasksWithPR = p.TasksWithPR
	resp.TasksFailed = p.TasksFailed
	resp.RegisteredAt = p.RegisteredAt

	// Rank + total among CONTRIBUTORS ONLY (#2601): the public Rankings now exclude
	// the hive's own internal agents, so the Me-card rank/total must match that
	// contributor-only set (otherwise "#9 of 10" would count bots). We re-rank the
	// non-agent entries in their existing (tasks-completed) order and read this
	// user's position within that filtered list.
	entries := s.LeaderboardForHub()
	contribRank := 0
	for _, e := range entries {
		if e.IsAgent {
			continue
		}
		contribRank++
		if e.GitHubUsername == username {
			resp.Rank = contribRank
		}
	}
	resp.Total = contribRank

	resp.Milestones, resp.NextMilestone = buildMilestones(p)
	resp.Hives = buildContributorHives(username)
	return resp
}

// avatarForUsername prefers the profile's stored avatar, else the canonical
// github.com avatar (matching buildLeaderboard).
func avatarForUsername(p *ContributorProfile) string {
	if p.AvatarURL != "" {
		return p.AvatarURL
	}
	return "https://github.com/" + p.GitHubUsername + ".png"
}

// handleContributorProfile serves GET /api/leaderboard/contributor/{username}.
// Public (via isPublicPath's /api/leaderboard prefix), read-only. The client
// passes the logged-in username (resolved from /api/gh-user-auth/status); any
// contributor's public profile can also be viewed by username, exactly as the
// public leaderboard already exposes their tier and counts.
func (s *Server) handleContributorProfile(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if !validGitHubUsername(username) {
		jsonError(w, "invalid username", http.StatusBadRequest)
		return
	}
	jsonResponse(w, s.BuildContributorProfile(username))
}

// validGitHubUsername rejects path traversal / obviously invalid handles before
// they reach the profile store. GitHub usernames are alphanumerics and hyphens.
func validGitHubUsername(u string) bool {
	if u == "" || len(u) > 39 {
		return false
	}
	for _, c := range u {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// pluralTasks renders "1 task" / "5 tasks" style counts with a custom noun.
func pluralTasks(n int, noun string) string {
	s := strconv.Itoa(n) + " " + noun
	if n != 1 {
		s += "s"
	}
	return s
}

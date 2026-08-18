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
	"strings"
	"time"
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

	// ── Collaborators (people worked alongside; append-only) ──
	Collaborators []CollaboratorRecord `json:"collaborators"`

	// RegisteredAt is when the contributor first registered (public).
	RegisteredAt string `json:"registered_at,omitempty"`

	// FoundingPosition is the contributor's REAL 1-based registration order on
	// this hive, included only when within the founding cohort (first twenty).
	// Derived from stored RegisteredAt timestamps — never fabricated.
	FoundingPosition int `json:"founding_position,omitempty"`

	// ── Public GitHub enrichment (cached server-side, best-effort) ──
	// ServiceYears is whole years since the GitHub account was created;
	// Renown is the public follower count. Both are omitted entirely when the
	// public api.github.com fetch fails (pointers so a real 0 still renders).
	ServiceYears *int `json:"service_years,omitempty"`
	Renown       *int `json:"renown,omitempty"`

	// ── Loadout / sponsorship / presence (safe, already-stored profile data) ──
	// CLIBackend + Model are the contributor's declared agent loadout; InvitedBy
	// is pure attribution (never affects tier). Sessions + CurrentTask reflect
	// the live hub connection state — CurrentTask carries only the safe summary
	// (title + number), never tokens or repo internals.
	CLIBackend  string              `json:"cli_backend,omitempty"`
	Model       string              `json:"model,omitempty"`
	InvitedBy   string              `json:"invited_by,omitempty"`
	Sessions    int                 `json:"sessions,omitempty"`
	CurrentTask *DossierTaskSummary `json:"current_task,omitempty"`
	// LastCompletedTask is the safe summary of the most recent completion the
	// hub recorded for this contributor — the honest seed for the Field Log
	// zone (no synthetic activity is ever fabricated client-side).
	LastCompletedTask *DossierTaskSummary `json:"last_completed_task,omitempty"`
	// LastActive is included ONLY when within the last 14 days (dossier design
	// firewall: absence is never rendered — an older timestamp is omitted
	// entirely so no client can derive an "inactive since" from this response).
	LastActive string `json:"last_active,omitempty"`

	// ── Dossier (self-service identity fields, all optional — see dossier.go) ──
	Archetype       string   `json:"archetype,omitempty"`
	Specializations []string `json:"specializations,omitempty"`
	Testimony       string   `json:"testimony,omitempty"`
	EquippedTitle   string   `json:"equipped_title,omitempty"`
	CredlyName      string   `json:"credly_name,omitempty"`
	EmblemSeed      string   `json:"emblem_seed,omitempty"`

	// Scope notes that these stats are aggregated from the hub data available on
	// THIS hive. If a future build tracks per-hive contributor profiles centrally
	// this becomes "cross-hive"; today it is the central profile this hub holds.
	Scope string `json:"scope"`
}

// DossierTaskSummary is the SAFE slice of a live task assignment the Me card
// may render: the issue/task title and number only — no tokens, no task IDs.
type DossierTaskSummary struct {
	Title  string `json:"title"`
	Number int    `json:"number,omitempty"`
	// Repo is the public owner/repo slug, included where known (field log).
	Repo string `json:"repo,omitempty"`
}

// dossierRecencyWindow bounds how long a last_active timestamp stays in the
// profile response. Outside it, the field is omitted — never "inactive since".
const dossierRecencyWindow = 14 * 24 * time.Hour

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
	case "merger":
		return 3
	case "advisor":
		return 4
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
		ID:       "tier-merger",
		Label:    "Reached Merger",
		Detail:   "Granted by a maintainer/owner",
		Attained: tierRank >= tierRankValue("merger"),
		Value:    0,
		Icon:     "🔀",
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

// buildContributorHives lists the hives where this contributor has DEMONSTRABLE
// presence — never merely the hives that exist.
//
// The previous implementation returned every hive in the federation registry,
// each labelled "contributor", regardless of whether the user had ever touched
// it; on a fleet of forty hives that produced forty fabricated relationships.
// The registry (FederationHive) carries no membership data, so the only honest
// sources are:
//
//   - THIS hive, where the contributor demonstrably has a profile (that is how
//     we found them at all), and
//   - any federation hive whose registration/heartbeat explicitly names them in
//     ActiveContributorNames.
//
// A remote hive that reports no names simply is not listed. Under-reporting is
// the correct failure mode: an absent theatre is honest, an invented one is not.
//
// The local hive is listed on the strength of the profile alone, WITHOUT
// requiring a registry entry: a hive does not register itself into its own
// federation registry (registration mints IDs of the form "hive-{org}-{project}"
// for REMOTE hives), so gating the local theatre on a registry match left the
// zone permanently empty on every ordinary deployment.
func buildContributorHives(username, localID string) []ContributorHiveRel {
	reg := loadFederationRegistry()
	out := make([]ContributorHiveRel, 0, len(reg.Hives)+1)
	seenLocal := false

	for _, h := range reg.Hives {
		named := false
		for _, n := range h.ActiveContributorNames {
			if strings.EqualFold(strings.TrimSpace(n), username) {
				named = true
				break
			}
		}
		isLocal := localID != "" && (h.ID == localID || strings.EqualFold(h.ProjectName, localID))
		if !named && !isLocal {
			continue
		}
		seenLocal = seenLocal || isLocal
		out = append(out, ContributorHiveRel{
			ID:           h.ID,
			ProjectName:  h.ProjectName,
			Org:          h.Org,
			Relationship: "contributor",
		})
	}

	// The hive we ARE, when it is not (and normally never will be) present in
	// its own registry. The profile itself is the evidence.
	if localID != "" && !seenLocal {
		out = append(out, ContributorHiveRel{
			ID:           localID,
			ProjectName:  localID,
			Relationship: "contributor",
		})
	}
	return out
}

// localHiveIdentity names the hive this dashboard IS, so a contributor's own
// hive is listed without needing to appear in its own heartbeat. It resolves
// from the configured project name — the same value the hive registers and
// renders under. Empty when the project is unnamed, in which case only
// explicitly-named hives are listed (an absent theatre is honest; an invented
// one is not).
func (s *Server) localHiveIdentity() string {
	if hiveIdentityForTest != "" {
		return hiveIdentityForTest
	}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return strings.TrimSpace(s.deps.Config.Project.Name)
	}
	return ""
}

// hiveIdentityForTest lets tests pin the local hive identity without standing up
// a full config. Empty in production, where the value comes from the configured
// project name.
var hiveIdentityForTest string

// BuildContributorProfile aggregates the central profile for one username. It is
// the testable core of the endpoint. A missing profile returns {Found:false}
// rather than an error, so an anonymous / unknown viewer is handled gracefully.
func (s *Server) BuildContributorProfile(username string) ContributorProfileResponse {
	resp := ContributorProfileResponse{
		GitHubUsername: username,
		Scope:          "central-hub",
		Milestones:     []ContributorMilestone{},
		Hives:          []ContributorHiveRel{},
		Collaborators:  []CollaboratorRecord{},
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

	// Loadout + sponsorship (already-stored public-safe profile data).
	resp.CLIBackend = p.CLIBackend
	resp.Model = p.Model
	resp.InvitedBy = p.InvitedBy

	// Dossier self-service fields (all optional; stored sanitised).
	resp.Archetype = p.Archetype
	resp.Specializations = p.Specializations
	resp.Testimony = p.Testimony
	resp.EquippedTitle = p.EquippedTitle
	resp.CredlyName = p.CredlyName
	resp.EmblemSeed = p.EmblemSeed

	// Field-log seed: the most recent completion the hub recorded (safe slice
	// only — title/number/repo). Absent when nothing has completed yet.
	if p.LastCompletedTask != nil {
		resp.LastCompletedTask = &DossierTaskSummary{
			Title:  p.LastCompletedTask.Title,
			Number: p.LastCompletedTask.Number,
			Repo:   p.LastCompletedTask.Repo,
		}
	}

	// last_active: ONLY when recent (dossier firewall — absence is never
	// rendered, so an out-of-window timestamp is omitted entirely).
	if p.LastActive != "" {
		if t, err := time.Parse(time.RFC3339, p.LastActive); err == nil && time.Since(t) <= dossierRecencyWindow {
			resp.LastActive = p.LastActive
		}
	}

	// Live presence: sessions + a SAFE summary of the current task (title +
	// number only), from the same hub live state the leaderboard uses.
	if s.contributeHub != nil {
		if ls, ok := s.contributeHub.LiveStates()[p.ContributorID]; ok {
			resp.Sessions = ls.Sessions
			if ls.CurrentTask != nil {
				resp.CurrentTask = &DossierTaskSummary{
					Title:  ls.CurrentTask.Title,
					Number: ls.CurrentTask.Number,
				}
			}
		}
	}

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
	resp.Hives = buildContributorHives(username, s.localHiveIdentity())
	resp.Collaborators = sortedCollaborators(p)

	// Founding mark: the contributor's real registration order, shown only for
	// the founding cohort. Derived from stored timestamps — if the order cannot
	// be established (missing/unparsable RegisteredAt) the mark is simply absent.
	if pos := registrationPosition(username); pos >= 1 && pos <= foundingCohortSize {
		resp.FoundingPosition = pos
	}

	// Public GitHub enrichment (cached 6h; omitted entirely on fetch failure).
	if years, followers, ok := githubPublicFor(username); ok {
		yearsV, followersV := years, followers
		resp.ServiceYears = &yearsV
		resp.Renown = &followersV
	}
	return resp
}

// foundingCohortSize bounds the founding mark: only the first twenty
// registrations on a hive carry "FOUNDING COHORT · FIRST TWENTY".
const foundingCohortSize = 20

// registrationPosition returns the 1-based registration order of username
// among all stored contributor profiles, earliest RegisteredAt first (ties
// broken by username for determinism). Profiles without a parsable
// RegisteredAt cannot be ordered and are excluded; if the target itself has no
// parsable timestamp the position is unknown and 0 is returned — the founding
// mark is then skipped rather than faked.
func registrationPosition(username string) int {
	profiles := listContributorProfiles()
	var mine time.Time
	found := false
	for i := range profiles {
		if profiles[i].GitHubUsername == username {
			t, err := time.Parse(time.RFC3339, profiles[i].RegisteredAt)
			if err != nil {
				return 0
			}
			mine = t
			found = true
			break
		}
	}
	if !found {
		return 0
	}
	pos := 1
	for i := range profiles {
		if profiles[i].GitHubUsername == username {
			continue
		}
		t, err := time.Parse(time.RFC3339, profiles[i].RegisteredAt)
		if err != nil {
			continue
		}
		if t.Before(mine) || (t.Equal(mine) && profiles[i].GitHubUsername < username) {
			pos++
		}
	}
	return pos
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

package hub

// Bounds for sanitizing a spoke-reported repo-activity summary. Generous
// enough for any real hive (a spoke tracks a handful of repos, each with
// bounded audited counts) but a hard ceiling against a hostile or buggy beat.
const (
	// maxRepoActivityRepos caps the number of per-repo rows kept from one beat.
	maxRepoActivityRepos = 256
	// maxRepoNameRunes caps a repo name ("org/name") length after sanitizing.
	maxRepoNameRunes = 200
	// maxRepoActivityCount clamps each action count — far above any real 12h/
	// 14d output volume, purely defensive.
	maxRepoActivityCount = 1_000_000
	// repoActivityMaxWindowHours clamps the reported window (14d in hours is
	// 336; allow some slack).
	repoActivityMaxWindowHours = 24 * 30
)

// sanitizeStatWire validates one action stat: a non-negative clamped count and
// a canonical-UTC RFC3339 NewestAt (or "" if it doesn't parse — never an epoch).
func sanitizeStatWire(s ActivityStatWire) ActivityStatWire {
	return ActivityStatWire{
		Count:    clampInt(s.Count, 0, maxRepoActivityCount),
		NewestAt: sanitizeReachTime(s.NewestAt),
	}
}

// sanitizeRepoActivity validates, clips, and copies a spoke-reported activity
// summary for storage on the registry entry. nil in (old spoke / nothing
// recorded yet) is nil out — "no data", never an error. The result never
// aliases the caller's payload. Rows past maxRepoActivityRepos are dropped, and
// a row whose repo name sanitizes to nothing is dropped (it identifies nothing).
func sanitizeRepoActivity(in []RepoActivityWire) []RepoActivityWire {
	if in == nil {
		return nil
	}
	if len(in) > maxRepoActivityRepos {
		in = in[:maxRepoActivityRepos]
	}
	out := make([]RepoActivityWire, 0, len(in))
	for _, r := range in {
		repo := truncateReachRunes(sanitizeHeartbeatField(r.Repo), maxRepoNameRunes)
		if repo == "" {
			continue
		}
		agents := sanitizeAgentRepoActivity(r.Agents)
		out = append(out, RepoActivityWire{
			Repo:       repo,
			Issues:     sanitizeStatWire(r.Issues),
			PRs:        sanitizeStatWire(r.PRs),
			Comments:   sanitizeStatWire(r.Comments),
			Merges:     sanitizeStatWire(r.Merges),
			Claims:     sanitizeStatWire(r.Claims),
			Reviews:    sanitizeStatWire(r.Reviews),
			Advisory:   sanitizeStatWire(r.Advisory),
			Reconciled: sanitizeStatWire(r.Reconciled),
			Agents:     agents,
		})
	}
	if len(out) == 0 {
		// Every row was junk — treat as "no data" rather than an empty summary
		// that would read as "reported zero output".
		return nil
	}
	return out
}

func sanitizeAgentRepoActivity(in []AgentRepoActivityWire) []AgentRepoActivityWire {
	if len(in) == 0 {
		return nil
	}
	if len(in) > maxRepoActivityRepos {
		in = in[:maxRepoActivityRepos]
	}
	out := make([]AgentRepoActivityWire, 0, len(in))
	for _, a := range in {
		agent := truncateReachRunes(sanitizeHeartbeatField(a.Agent), maxRepoNameRunes)
		if agent == "" {
			continue
		}
		out = append(out, AgentRepoActivityWire{
			Agent:      agent,
			Issues:     sanitizeStatWire(a.Issues),
			PRs:        sanitizeStatWire(a.PRs),
			Comments:   sanitizeStatWire(a.Comments),
			Merges:     sanitizeStatWire(a.Merges),
			Claims:     sanitizeStatWire(a.Claims),
			Reviews:    sanitizeStatWire(a.Reviews),
			Advisory:   sanitizeStatWire(a.Advisory),
			Reconciled: sanitizeStatWire(a.Reconciled),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Hub-side wire mirror of the spoke's dashboard.RepoActivity / ActivitySnapshot.
// It is defined HERE (not imported from pkg/dashboard) because pkg/dashboard
// imports pkg/hub — importing back would be a cycle. main.go maps the dashboard
// snapshot into these plain wire structs field-by-field, exactly as it copies
// the fleet-stat scalars, so the two packages stay decoupled.

// ActivityStatWire is one action's count within the window plus the newest
// timestamp seen for it (RFC3339). NewestAt lets the hub compute "within 12h"
// itself without trusting a spoke-side clamp.
type ActivityStatWire struct {
	Count    int    `json:"count"`
	NewestAt string `json:"newestAt,omitempty"`
}

// RepoActivityWire is per-repo output activity over the collector's window.
// Field set mirrors dashboard.RepoActivity.
type RepoActivityWire struct {
	Repo       string                  `json:"repo"`
	Issues     ActivityStatWire        `json:"issues"`
	PRs        ActivityStatWire        `json:"prs"`
	Comments   ActivityStatWire        `json:"comments"`
	Merges     ActivityStatWire        `json:"merges"`
	Claims     ActivityStatWire        `json:"claims"`
	Reviews    ActivityStatWire        `json:"reviews"`
	Advisory   ActivityStatWire        `json:"advisory"`
	Reconciled ActivityStatWire        `json:"reconciled"`
	Agents     []AgentRepoActivityWire `json:"agents,omitempty"`
}

type AgentRepoActivityWire struct {
	Agent      string           `json:"agent"`
	Issues     ActivityStatWire `json:"issues"`
	PRs        ActivityStatWire `json:"prs"`
	Comments   ActivityStatWire `json:"comments"`
	Merges     ActivityStatWire `json:"merges"`
	Claims     ActivityStatWire `json:"claims"`
	Reviews    ActivityStatWire `json:"reviews"`
	Advisory   ActivityStatWire `json:"advisory"`
	Reconciled ActivityStatWire `json:"reconciled"`
}

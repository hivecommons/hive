package spoke

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

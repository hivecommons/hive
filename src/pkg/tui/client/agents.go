package client

import "context"

// Agent is one entry from GET /api/agents.
//
// Fields are transcribed from the dashboard handler, pkg/dashboard/api_agents.go
// (handleAgentsList / agentListEntry), which the published spec now matches
// exactly (kubestellar/hive#5023 widened dashboard/openapi.json from 32
// documented operations to 255 paths, and /api/agents is one of them — the
// design doc's §2.1 gap this task was filed against is now closed). Both
// sources agree field-for-field; nothing here is invented or guessed from the
// live response.
//
// Two things the design doc's contract table implied this struct might need —
// a "last activity" timestamp and a running/paused/idle status string — are
// NOT part of this payload. handleAgentsList only ever sets Name, ID,
// DisplayName, Enabled, Managed, Backend and Model; activity and live state
// live elsewhere (e.g. /api/status), not here. A future task that needs those
// fields renders from a different endpoint rather than this struct growing
// fields the server never sends.
type Agent struct {
	// Name is the agent's config key — what hivectl and the dashboard use to
	// address it in every other endpoint (/api/pause/{name}, /api/kick/{name},
	// /api/config/agent/{name}, ...).
	Name string `json:"name"`

	// ID is the agent's stable identifier, distinct from Name (agentListEntry.ID
	// is populated from cfg.ID, not derived from the map key).
	ID string `json:"id"`

	// DisplayName is the human-facing label. The handler falls back to Name
	// when the config has none, but still marshals it with `omitempty` — so an
	// agent whose config carries no display name at all can (in principle)
	// arrive as an empty string on the wire; callers should fall back to Name
	// exactly as the handler does when this is empty.
	DisplayName string `json:"displayName,omitempty"`

	// Enabled reports whether the agent is turned on in config.
	Enabled bool `json:"enabled"`

	// Managed is true for CRUD-created agents, false for base/pack-defined
	// agents — the latter cannot be deleted (see handleAgentDelete).
	Managed bool `json:"managed"`

	// Backend is the agent's configured backend, e.g. "claude", "copilot",
	// "codex".
	Backend string `json:"backend"`

	// Model is the agent's configured model.
	Model string `json:"model"`
}

// Agents lists every configured agent (managed and pack-defined) with its
// core identity and current backend/model, from GET /api/agents.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var agents []Agent
	if err := c.getJSON(ctx, "/api/agents", &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

package client

import "context"

// PackAgent is one agent in an ACMM level's roster.
//
// Transcribed field-for-field from config.PackAgent (pkg/config/acmm_packs.go:25),
// which handlePacksList marshals whole; dashboard/openapi.json documents the
// same 21 fields and the two agree exactly. The whole type is modelled rather
// than the handful today's web overlay happens to render, because which subset
// a roster preview needs is T19's decision, not this one's.
type PackAgent struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// Role is the agent's functional role (e.g. "scanner", "reviewer").
	Role        string `json:"role"`
	Description string `json:"description"`
	Emoji       string `json:"emoji"`
	Color       string `json:"color"`
	// SortOrder is the roster display order within the pack.
	SortOrder int    `json:"sortOrder"`
	Backend   string `json:"backend"`
	Model     string `json:"model"`
	// BeadRole is the bead-store role this agent writes under.
	BeadRole string `json:"beadRole"`
	// KickTemplate names the prompt template the governor kicks this agent with.
	KickTemplate string `json:"kickTemplate"`
	// IncludeRepos reports whether the kick prompt carries the repo list.
	IncludeRepos bool `json:"includeRepos"`
	// LaneKeywords are the work keywords that route issues to this agent.
	LaneKeywords []string `json:"laneKeywords"`
	// Interactions and KnowledgeUse are prose descriptions of how the agent
	// works with others and with the knowledge base; the web overlay renders
	// both as the agent's "what it does" copy.
	Interactions string `json:"interactions"`
	KnowledgeUse string `json:"knowledgeUse"`
	// Hidden agents are part of the pack but not shown in the roster.
	Hidden bool `json:"hidden,omitempty"`
	// StaleTimeout is the agent's inactivity timeout in seconds, 0 for default.
	StaleTimeout int `json:"staleTimeout,omitempty"`
	// Mode is the pack-seeded ACMM mode for this agent. Note that setting a
	// level CLEARS every per-agent mode override so these re-apply cleanly.
	Mode string `json:"mode,omitempty"`
	// OnDemand agents are not kicked on the governor's cadence.
	OnDemand bool `json:"onDemand,omitempty"`
	// CavemanMode is the reduced-capability variant, when the pack defines one.
	CavemanMode string `json:"cavemanMode,omitempty"`
}

// PackGovernor is the governor configuration a level recommends.
//
// Transcribed from config.PackGovernor (pkg/config/acmm_packs.go:49); the spec
// documents the same six fields.
type PackGovernor struct {
	// Modes is the mode set this level runs (e.g. "advisory").
	Modes string `json:"modes"`
	// MergePolicy is the level's PR merge policy.
	MergePolicy string `json:"mergePolicy"`
	// EvalIntervalS is the governor evaluation cadence in seconds.
	EvalIntervalS int `json:"evalIntervalS,omitempty"`
	// Cadences is mode name → agent name → interval string.
	Cadences map[string]map[string]string `json:"cadences,omitempty"`
	// Thresholds is mode name → queue-depth threshold.
	Thresholds map[string]int `json:"thresholds,omitempty"`
	// PlanAutoApprove releases a decomposed epic's children without a human
	// plan review. False on the low levels, true on the high-trust ones.
	PlanAutoApprove bool `json:"planAutoApprove,omitempty"`
}

// Pack is one ACMM maturity level: its identity, the governor settings it
// recommends, and the agent roster it defines.
type Pack struct {
	// Level is the maturity level, 1-6.
	Level       int    `json:"level"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// AgentCount is len(Agents), sent by the server so a caller rendering only
	// a summary need not walk the roster.
	AgentCount int          `json:"agentCount"`
	Governor   PackGovernor `json:"governor"`
	// Current flags the level the hive is running. Exactly one pack carries it
	// on a configured hive; see ACMMStatus.Level.
	Current bool        `json:"current"`
	Agents  []PackAgent `json:"agents"`
}

// ACMMStatus is every defined ACMM pack plus the level currently in force.
//
// It is NOT a wire type — GET /api/packs returns a bare ARRAY of packs, and the
// active level is carried inside it as the `current` flag rather than as a
// field of its own. ACMMStatus is the small amount of assembly that saves every
// caller from re-deriving the same thing, which is why it has no json tags:
// nothing marshals it, and giving it tags would suggest the server sends this
// shape.
type ACMMStatus struct {
	// Level is the level whose pack is flagged current, or 0 when none is —
	// which is what an unconfigured hive looks like, not an error.
	Level int
	// Packs is every defined pack, in the order the server returned them.
	Packs []Pack
}

// Current returns the pack the hive is running, and whether one was flagged.
func (s ACMMStatus) Current() (Pack, bool) {
	for _, p := range s.Packs {
		if p.Current {
			return p, true
		}
	}
	return Pack{}, false
}

// ACMM reads every ACMM pack and the level currently in force, from
// GET /api/packs.
//
// The task text named this endpoint only as "the operation the dashboard uses
// for reading the ACMM level"; it is `/api/packs`, and there is no `/api/acmm`
// level read. (`/api/acmm/evaluation` and `/api/acmm-recommendation` are
// read-only advisory endpoints — what the hive SHOULD be at, not what it is.)
func (c *Client) ACMM(ctx context.Context) (ACMMStatus, error) {
	var packs []Pack
	if err := c.getJSON(ctx, "/api/packs", &packs); err != nil {
		return ACMMStatus{}, err
	}
	status := ACMMStatus{Packs: packs}
	if current, ok := status.Current(); ok {
		status.Level = current.Level
	}
	return status, nil
}

// GovernorIntervalChange is an eval-interval value the level change moved.
type GovernorIntervalChange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// GovernorCadenceChange is one agent's cadence, in one mode, that the level
// change moved.
type GovernorCadenceChange struct {
	Mode  string `json:"mode"`
	Agent string `json:"agent"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// GovernorChanges reports the governor settings the level change actually
// moved, capturing the OLD values: a running governor keeps its current ticker
// until the hive restarts, so "what changed" is not readable from live config
// afterwards.
//
// Transcribed from dashboard.GovernorChanges (pkg/dashboard/api_packs.go:64).
// dashboard/openapi.json types this field as a bare `{"type": "object"}` with
// no properties, so the shape comes from the Go type — the same drift #5077
// tracks for other endpoints.
type GovernorChanges struct {
	EvalIntervalS *GovernorIntervalChange `json:"eval_interval_s,omitempty"`
	Cadences      []GovernorCadenceChange `json:"cadences,omitempty"`
}

// ACMMLevelResult is what PUT /api/packs/level reports after setting a level.
//
// Setting a level is not just writing a number: the handler clears every
// per-agent mode override, force-applies the pack to reconcile the roster and
// the governor's cadences and thresholds, and pauses or resumes agents to match
// the new roster. These fields are that reconciliation's receipt, and a caller
// that shows only "level set" hides work the operator asked for and may want to
// check.
type ACMMLevelResult struct {
	OK bool `json:"ok"`
	// Level is the level now in force.
	Level int `json:"level"`
	// PackAgents is the new level's non-hidden roster.
	PackAgents []string `json:"packAgents"`
	// PackUpdated names the agents the pack apply actually modified.
	PackUpdated []string `json:"packUpdated"`
	// GovernorChanges is absent when the level moved no governor setting.
	GovernorChanges *GovernorChanges `json:"governor_changes,omitempty"`
	// Paused and Resumed are the agents whose run state was changed to match
	// the new level's roster.
	Paused  []string `json:"paused"`
	Resumed []string `json:"resumed"`
}

// ApplyACMM sets the hive's ACMM level via PUT /api/packs/level.
//
// The task text proposed ApplyACMM(ctx) returning ACMMStatus. Both are
// corrected against the endpoint: the level is a required request-body field,
// and the response is this reconciliation receipt rather than the pack list —
// re-reading the packs afterwards is a separate ACMM call.
//
// OWNER-ONLY, so a non-owner gets an APIError with 403; use IsForbidden to tell
// that apart from an unreachable hive.
//
// The level range is deliberately NOT validated here. The server owns it
// (1-6 today) and answers an out-of-range level with a 400 naming the range, so
// duplicating the bound would only add a way for this client to go stale
// against a level the hive has since gained.
//
// A 500 is the one failure that is NOT a no-op: it means the level was
// persisted but the roster could not be reconciled to it, which the handler
// reports precisely so the operator sees the drift rather than trusting a false
// success. Callers must not present it as "nothing happened".
func (c *Client) ApplyACMM(ctx context.Context, level int) (ACMMLevelResult, error) {
	var result ACMMLevelResult
	body := struct {
		Level int `json:"level"`
	}{Level: level}
	if err := c.putJSON(ctx, "/api/packs/level", body, &result); err != nil {
		return ACMMLevelResult{}, err
	}
	return result, nil
}

package dashboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

func (h *ContributeWSHub) ActiveCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := make(map[string]bool)
	for _, c := range h.connections {
		if c.profile != nil && c.profile.GitHubUsername != "" {
			seen[c.profile.GitHubUsername] = true
		}
	}
	return len(seen)
}

func (h *ContributeWSHub) ActiveSessionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.connections)
}

type ContributorLiveState struct {
	Active      bool           `json:"active"`
	CurrentTask *WSTaskAssign  `json:"current_task,omitempty"`
	Tasks       []WSTaskAssign `json:"tasks,omitempty"`
	Sessions    int            `json:"sessions"`
	Role        string         `json:"role,omitempty"`
}

func (h *ContributeWSHub) LiveStates() map[string]ContributorLiveState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]ContributorLiveState, len(h.connections))
	for _, c := range h.connections {
		c.mu.Lock()
		cid := ""
		if c.profile != nil {
			cid = c.profile.ContributorID
		}
		role := c.role
		stale := time.Since(c.lastPong) > wsHeartbeatTimeout
		var task *WSTaskAssign
		if c.currentTask != nil && !stale {
			t := *c.currentTask
			task = &t
		}
		c.mu.Unlock()
		if cid != "" && !stale {
			existing := out[cid]
			existing.Active = true
			existing.Sessions++
			if role != "" {
				existing.Role = role
			}
			if task != nil {
				existing.CurrentTask = task
				dupe := false
				for _, t := range existing.Tasks {
					if t.TaskID == task.TaskID {
						dupe = true
						break
					}
				}
				if !dupe {
					existing.Tasks = append(existing.Tasks, *task)
				}
			}
			out[cid] = existing
		}
	}
	return out
}

// RoleBreakdown returns a count of active connections grouped by role.
// Connections without a role (task-driven mode) are counted under "task-driven".
func (h *ContributeWSHub) RoleBreakdown() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	breakdown := make(map[string]int)
	for _, c := range h.connections {
		c.mu.Lock()
		role := c.role
		c.mu.Unlock()
		if role == "" {
			role = "task-driven"
		}
		breakdown[role]++
	}
	return breakdown
}

// FleetClanker is a read-only view of one connected contributor ("clanker")
// session as the operator-facing Management & Operations tab renders it. It
// carries only what the contributor handshake already put on the wire plus the
// live connection timing the hub already tracks — no secrets, no new state.
type FleetClanker struct {
	ContributorID   string `json:"contributor_id"`
	GitHubUsername  string `json:"github_username,omitempty"`
	CLIBackend      string `json:"cli_backend,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	KnowledgeLoaded *bool  `json:"knowledge_loaded,omitempty"`
	KnowledgeError  string `json:"knowledge_error,omitempty"`
	// AdvisorModel / AdvisorEffort: the second model reviewing this
	// contributor's work and its effort (hivecommons/hive#7760); omitted for a
	// single-model backend.
	AdvisorModel       string        `json:"advisor_model,omitempty"`
	AdvisorEffort      string        `json:"advisor_effort,omitempty"`
	Role               string        `json:"role,omitempty"`
	ClientRole         string        `json:"client_role,omitempty"`
	AssignedRole       string        `json:"assigned_agent_role,omitempty"`
	RoleMismatch       string        `json:"role_mismatch,omitempty"`
	TrustTier          string        `json:"trust_tier,omitempty"`
	EligibleForTrusted bool          `json:"eligible_for_trusted,omitempty"`
	ConnectedAt        string        `json:"connected_at,omitempty"`
	LastActivity       string        `json:"last_activity,omitempty"`
	Stale              bool          `json:"stale,omitempty"`
	CurrentTask        *WSTaskAssign `json:"current_task,omitempty"`
	// IdleReason is the machine-readable reason this clanker currently has no work
	// (#2546): one of the taskUnavailable* reasons last sent to it. Empty when the
	// clanker is actively working (CurrentTask set) or has never been refused. It
	// lets the operator distinguish "idle: no_matching_work" from "idle:
	// contribution_suspended" instead of an undifferentiated idle. Read-only.
	IdleReason string `json:"idle_reason,omitempty"`
	// PromptPreview is the exact assignment prompt built for CurrentTask (#2539),
	// surfaced read-only so an operator can see the instruction the agent is
	// running. It NEVER contains the minted github_token — the token travels on the
	// task_assign WSMessage separately and is not stored here. Empty when idle.
	PromptPreview string `json:"prompt_preview,omitempty"`
	// Capabilities is the client-declared runtime posture from the handshake
	// (#2547 declare half): container runtime, OS/arch, agent/relay versions,
	// credential type. Nil when the client declared none (unversioned client).
	// Surfaced read-only exactly like CLIBackend/Model/Role so the Operations tab
	// COULD display it; it is NEVER used to route or gate work.
	Capabilities *ContributorCapabilities `json:"capabilities,omitempty"`
	// LastFailure is the most recent task failure this clanker reported (#2547),
	// surfaced read-only so an operator can attribute a run of failures instead
	// of inferring the cause from a tmux tail. Its Kind is SELF-REPORTED by the
	// client and advisory — the hub records and displays it, and never routes,
	// gates, or adjusts a work item's failure cooldown on it. Nil until this
	// connection has failed a task.
	LastFailure *ContributorFailure `json:"last_failure,omitempty"`
	// PaneTail is the last few lines of the agent's terminal pane as the relay
	// most recently reported them in a task_progress (#7317 item 3) — what the
	// agent is showing RIGHT NOW, for the operator asking "why has this clanker
	// been silent for twenty minutes". Set only while CurrentTask is in flight
	// (idle, the last pane is stale) and bounded/redacted by boundPaneTail like
	// the stored copy on a TaskRunRecord. handleContributeFleet strips it for
	// any viewer paneTailViewer does not admit. Diagnostic only: nothing routes
	// on it.
	PaneTail []string `json:"pane_tail,omitempty"`
	// LabelInterests (#2677) mirrors the contributor's own OPT-IN label-affinity
	// list (#2637, ContributorProfile.LabelInterests) so an operator can see
	// fleet-wide who prefers what without cross-referencing each profile
	// separately. Strictly READ-ONLY here: an operator never sets or edits this
	// through the fleet view — it stays contributor-owned via the existing
	// PUT /api/contribute/interests. Omitted when the contributor has declared
	// none.
	LabelInterests []string `json:"label_interests,omitempty"`
	// AgentRoleGrants is the operator-managed grant list for privileged spoke
	// agent roles. It is shown to owner/read-write viewers in the fleet row; the
	// server-side mutation endpoint remains the enforcement boundary.
	AgentRoleGrants  []string                     `json:"agent_role_grants,omitempty"`
	OperatorMessages []ContributorOperatorMessage `json:"operator_messages,omitempty"`
	// Protocol compares the contributor-protocol version this client DECLARED
	// against the one this hub speaks (#2547 peer-compatibility criterion,
	// building on the versions #2567 put on the wire). Always set — an
	// unversioned relay reports verdict "unknown", which is a supported state,
	// not a fault. Derived per snapshot from Capabilities.RelayProtocolVersion;
	// it stores nothing new and, like Capabilities, is never routed or gated on.
	Protocol *ProtocolCompat `json:"protocol,omitempty"`
}

// FleetWorkItem is a read-only view of one in-flight task the fleet is working,
// surfaced the way the operator work-list lists items (repo / number / title /
// who is on it / status). Derived entirely from live connection state — the hub
// tracks currentTask per connection; nothing here is fabricated.
type FleetWorkItem struct {
	TaskID         string `json:"task_id"`
	Kind           string `json:"kind,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Number         int    `json:"number,omitempty"`
	Title          string `json:"title,omitempty"`
	ContributorID  string `json:"contributor_id,omitempty"`
	GitHubUsername string `json:"github_username,omitempty"`
	CLIBackend     string `json:"cli_backend,omitempty"`
	Status         string `json:"status"`
	// Labels are the chosen issue's labels (#2539), shown alongside the prompt
	// preview in the ops Task panel. Metadata only.
	Labels []string `json:"labels,omitempty"`
	// PromptPreview is the exact prompt shipped for this work item (#2539),
	// surfaced read-only in the ops Task panel so the instruction is legible
	// before/as it runs. It NEVER contains the github_token. Empty if unknown.
	PromptPreview string `json:"prompt_preview,omitempty"`
}

// FleetSnapshot is the read-only payload the Management & Operations tab hydrates
// from. Everything is derived from the hub's current live connections — it adds
// no enforcement and mutates nothing.
type FleetSnapshot struct {
	Clankers []FleetClanker  `json:"clankers"`
	Work     []FleetWorkItem `json:"work"`
}

// FleetSnapshot returns the current connected-clanker fleet and its in-flight
// work, read-only, from the hub's live connection registry. A connection whose
// last pong is older than wsHeartbeatTimeout is reported with Stale=true and its
// in-flight task is treated as no longer active (matching LiveStates()).
func (h *ContributeWSHub) FleetSnapshot() FleetSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap := FleetSnapshot{
		Clankers: make([]FleetClanker, 0, len(h.connections)),
		Work:     make([]FleetWorkItem, 0),
	}
	for _, c := range h.connections {
		c.mu.Lock()
		fc := FleetClanker{
			CLIBackend:      c.cliBackend,
			Model:           c.model,
			ReasoningEffort: c.reasoningEffort,
			KnowledgeLoaded: c.knowledgeLoaded,
			KnowledgeError:  c.knowledgeError,
			AdvisorModel:    c.advisorModel,
			AdvisorEffort:   c.advisorEffort,
			Role:            c.role,
			ClientRole:      c.clientRole,
			AssignedRole:    c.assignedRole,
			ConnectedAt:     c.connectedAt.UTC().Format(time.RFC3339),
			LastActivity:    c.lastPong.UTC().Format(time.RFC3339),
			Stale:           time.Since(c.lastPong) > wsHeartbeatTimeout,
		}
		if c.assignedRole != "" && normalizeAgentRole(c.clientRole) != "" && normalizeAgentRole(c.clientRole) != effectiveAssignedAgentRole(c.assignedRole) {
			if c.assignedRole == "none" {
				fc.RoleMismatch = fmt.Sprintf("client requested %s; owner assigned general work", c.clientRole)
			} else {
				fc.RoleMismatch = fmt.Sprintf("client requested %s; owner assigned %s", c.clientRole, c.assignedRole)
			}
		}
		// #2547: surface the client-declared capabilities read-only (a copy so the
		// snapshot never aliases live connection state). Nil for unversioned clients.
		if c.capabilities != nil {
			capsCopy := *c.capabilities
			fc.Capabilities = &capsCopy
		}
		// #2547 peer-compatibility: derive the hub-vs-client protocol comparison
		// from the version already declared above. Always present so the operator
		// row can show the hub's own version even when the client declared none —
		// a bare "proto 1.1" chip with nothing to compare it against was the gap.
		declaredVersion := ""
		if c.capabilities != nil {
			declaredVersion = c.capabilities.RelayProtocolVersion
		}
		compat := peerProtocolCompat(declaredVersion)
		fc.Protocol = &compat
		// #2547: same treatment for the last reported failure — a copy, with the
		// client's free-text reason redacted and bounded. Reason is the only
		// CLIENT-CONTROLLED free text on this snapshot: it is an error string the
		// relay chose, so it can carry a token it happened to print, and it has no
		// length the client is obliged to respect. Both are handled here, at the
		// boundary where it becomes operator-visible.
		if c.lastFailure != nil {
			failCopy := *c.lastFailure
			failCopy.Reason = truncateFailureReason(redactTokens(failCopy.Reason))
			fc.LastFailure = &failCopy
		}
		if c.profile != nil {
			fc.ContributorID = c.profile.ContributorID
			fc.GitHubUsername = c.profile.GitHubUsername
			fc.TrustTier = c.profile.TrustTier
			fc.EligibleForTrusted = contributorEligibleForTrusted(c.profile)
			// #2677: mirror the contributor's own label interests read-only (a copy
			// so the snapshot never aliases the live profile slice).
			if len(c.profile.LabelInterests) > 0 {
				fc.LabelInterests = append([]string(nil), c.profile.LabelInterests...)
			}
			if len(c.profile.AgentRoleGrants) > 0 {
				fc.AgentRoleGrants = append([]string(nil), c.profile.AgentRoleGrants...)
			}
			if len(c.profile.OperatorMessages) > 0 {
				fc.OperatorMessages = append([]ContributorOperatorMessage(nil), c.profile.OperatorMessages...)
			}
		}
		var task *WSTaskAssign
		var promptPreview string
		var taskLabels []string
		if c.currentTask != nil && !fc.Stale {
			t := *c.currentTask
			task = &t
			// #2539: surface the stored prompt (never the token) for the active
			// task so the ops tab can preview the instruction being run.
			promptPreview = c.currentPrompt
			if len(c.currentLabels) > 0 {
				taskLabels = append([]string(nil), c.currentLabels...)
			}
			// #7317 item 3: and the pane the agent is showing for it. A bounded,
			// redacted copy — never the live slice — for the same aliasing reason
			// as every other field on this snapshot. Only a pane the relay
			// reported for THIS task (#7605): until its first progress frame the
			// stored pane is still the previous task's final screen.
			fc.PaneTail = c.paneTailFor(task.TaskID)
		}
		// #2546: when the clanker is NOT actively working, expose why it is idle so
		// the operator sees "idle: no_matching_work" etc. Suppressed while a task is
		// in flight (the reason, if any, is stale then).
		if task == nil {
			fc.IdleReason = c.lastIdleReason
		}
		c.mu.Unlock()
		fc.CurrentTask = task
		fc.PromptPreview = promptPreview
		if task != nil {
			snap.Work = append(snap.Work, FleetWorkItem{
				TaskID:         task.TaskID,
				Kind:           task.Kind,
				Repo:           task.Repo,
				Number:         task.Number,
				Title:          task.Title,
				ContributorID:  fc.ContributorID,
				GitHubUsername: fc.GitHubUsername,
				CLIBackend:     fc.CLIBackend,
				Status:         "in-progress",
				Labels:         taskLabels,
				PromptPreview:  promptPreview,
			})
		}
		snap.Clankers = append(snap.Clankers, fc)
	}
	// Deterministic order so the operator view is stable across polls.
	sort.Slice(snap.Clankers, func(i, j int) bool {
		if snap.Clankers[i].ConnectedAt != snap.Clankers[j].ConnectedAt {
			return snap.Clankers[i].ConnectedAt < snap.Clankers[j].ConnectedAt
		}
		return snap.Clankers[i].ContributorID < snap.Clankers[j].ContributorID
	})
	sort.Slice(snap.Work, func(i, j int) bool {
		if snap.Work[i].Repo != snap.Work[j].Repo {
			return snap.Work[i].Repo < snap.Work[j].Repo
		}
		return snap.Work[i].Number < snap.Work[j].Number
	})
	return snap
}

// CooldownCounts returns two read-only tallies the Operations/Management tabs
// surface next to the ready queue (see handleContributeFleet):
//
//   - cooldown: how many completed issues are STILL within their cooldown window
//     and therefore held out of selection. It counts only NON-expired entries in
//     completedTasks (an entry past its cooldownForLocked() period is expired-but-
//     not-yet-swept and must not inflate the count — it matches what
//     isTaskInCooldown would actually gate). When cooldown is disabled by the
//     operator kill-switch (cooldownEnabled()==false), nothing is gated, so this
//     is 0.
//   - inFlight: how many distinct issues are currently held by a live
//     connection — reuses activeIssueKeys(), the SAME set selectTask uses as its
//     double-assign guard and ReadyQueue uses to exclude in-flight work.
//
// Read-only: it mutates nothing (unlike isTaskInCooldown it does not sweep
// expired entries) and adds no enforcement.
func (h *ContributeWSHub) CooldownCounts() (cooldown, inFlight int) {
	// cooldown: count non-expired completedTasks under the completion lock. When the
	// operator kill-switch disables cooldown, nothing is gated, so the count is 0.
	if h.cooldownEnabled() {
		h.completedMu.Lock()
		for key, t := range h.completedTasks {
			if time.Since(t) <= h.cooldownForLocked(key) {
				cooldown++
			}
		}
		h.completedMu.Unlock()
	}

	// inFlight: distinct issues held by a live connection — the same activeIssues
	// set selectTask's guard and ReadyQueue use, so the header count matches what is
	// actually excluded from "ready".
	inFlight = len(h.activeIssueKeys())
	return cooldown, inFlight
}

// HeldCount returns how many OPERATOR-HELD issues are also present in the current
// actionable universe — i.e. held issues that WOULD be offerable if not parked. It
// mirrors what the ready queue actually surfaces as Held (ReadyQueue appends exactly
// these), so the header "N on hold" tally matches the greyed rows the operator sees,
// rather than counting stale hold keys for issues no longer actionable. Read-only:
// it mutates nothing and adds no enforcement. Uses the SAME canonical "%s#%d" key
// form (repo.Full # number) every admission check builds, so it cannot silently miss
// on a repo-name spelling mismatch (the #2648 class of bug).
func (h *ContributeWSHub) HeldCount() int {
	if h == nil || h.server == nil {
		return 0
	}
	var hold map[string]struct{}
	if h.server.deps != nil && h.server.deps.Config != nil {
		hold = queueHoldSet(h.server.deps.Config.Hub.ContributeQueueHold)
	}
	if len(hold) == 0 {
		return 0
	}
	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return 0
	}
	count := 0
	for _, repo := range status.Repos {
		for _, raw := range repo.ActionableIssues {
			b, err := json.Marshal(raw)
			if err != nil {
				continue
			}
			var issue map[string]any
			if err := json.Unmarshal(b, &issue); err != nil {
				continue
			}
			number := 0
			switch n := issue["number"].(type) {
			case float64:
				number = int(n)
			case int:
				number = n
			}
			if number == 0 {
				continue
			}
			if _, isHeld := hold[fmt.Sprintf("%s#%d", repo.Full, number)]; isHeld {
				count++
			}
		}
	}
	return count
}

func (h *ContributeWSHub) ActiveConnections() []ContributorConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ContributorConnection, 0, len(h.connections))
	for _, c := range h.connections {
		c.mu.Lock()
		out = append(out, ContributorConnection{
			profile:         c.profile,
			cliBackend:      c.cliBackend,
			model:           c.model,
			reasoningEffort: c.reasoningEffort,
			role:            c.role,
			clientRole:      c.clientRole,
			assignedRole:    c.assignedRole,
			connectedAt:     c.connectedAt,
			currentTask:     c.currentTask,
			tmuxOutput:      append([]string{}, c.tmuxOutput...),
			tmuxOutputTask:  c.tmuxOutputTask,
		})
		c.mu.Unlock()
	}
	return out
}

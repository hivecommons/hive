package hub

import (
	"strings"
	"time"
)

// Fleet-divergence derivation.
//
// This file turns the raw per-agent signals a spoke reports (state, needsLogin,
// sessionMissing, expectedActive, canOpen*/canMerge, plus the hive-level
// blockers on the registry row) into the three-way picture the fleet view
// exists to show:
//
//	EXPECTED  — the governor's current mode schedules this agent to run now.
//	ACTUAL    — the agent's pane is truly alive and working.
//	ABLE      — the agent can open issues, open PRs, and merge PRs.
//
// and makes the DELTAS between them loud:
//
//	STUCK     — expected active, but not actually running/working.
//	IMPOTENT  — actually running, but not able to do its mission.
//	quiet     — paused or expected-off; never a fault (provenance shown, not an
//	            alarm).
//
// Everything here is DERIVED from signals already on the row; nothing is a new
// wire field beyond the six raw booleans/strings. The run-state machine is
// NOT re-implemented — it reuses classifyInactiveAgent (agent_inactivity.go) so
// the fleet view and the agents-inactive alert can never disagree about whether
// an agent is stuck.
//
// Backward compatibility is load-bearing: a spoke too old to report the new
// fields sends them all zero-valued. ExpectedActive=false and the capability
// bools=false must therefore read as UNKNOWN — never as "expected off" or
// "cannot work" — so STUCK and IMPOTENT never fire on a legacy spoke.

// agentRunState is the ACTUAL-leg verdict for one agent.
type agentRunState int

const (
	// runUnknown — the spoke did not report enough to classify (legacy spoke,
	// or an agent in a transient state). Never a fault.
	runUnknown agentRunState = iota
	// runWorking — running, authenticated, session present, not idle-with-work.
	runWorking
	// runIdleAtPrompt — running and authenticated but idle while work waits.
	runIdleAtPrompt
	// runStuckAtLogin — running but sitting on a login prompt past grace.
	runStuckAtLogin
	// runSessionGone — the manager believes it runs, but the tmux session is
	// gone (zombie).
	runSessionGone
	// runDead — expected active and enabled, but not running and not paused.
	runDead
	// runQuietByDesign — paused, or not expected active in the current mode.
	runQuietByDesign
)

// String is the stable wire label the frontend renders; it never re-derives the
// state machine.
func (s agentRunState) String() string {
	switch s {
	case runWorking:
		return "working"
	case runIdleAtPrompt:
		return "idle-at-prompt"
	case runStuckAtLogin:
		return "stuck-at-login"
	case runSessionGone:
		return "session-gone"
	case runDead:
		return "dead"
	case runQuietByDesign:
		return "quiet-by-design"
	default:
		return "unknown"
	}
}

// capabilityTier collapses the three ABLE booleans + blockers into a traffic
// light for the badge.
const (
	tierGreen = "green" // able to do everything its mode grants
	tierAmber = "amber" // partially able (can open issues but a write is gated/blocked)
	tierRed   = "red"   // cannot do any write it is expected to
	tierGray  = "gray"  // unknown (legacy spoke)
)

// hiveBlockers are the hive-level conditions that stop an otherwise-capable
// agent from doing its job. Populated from fields already on the registry row —
// no new wire fields. A blocker downgrades capability regardless of the ACMM
// gate: an agent whose mode grants merge still cannot merge if the App lacks
// permission or the repo target is misconfigured.
type hiveBlockers struct {
	GitHubAppRequired       bool
	GitHubAppPermIssue      string
	GitHubAppState          string
	RepoTargetMisconfigured bool
	RepoTargetIssue         string
	InferenceAuthError      string
}

// any reports whether any hive-level blocker is set.
func (b hiveBlockers) any() bool {
	return b.GitHubAppRequired || b.RepoTargetMisconfigured ||
		strings.TrimSpace(b.GitHubAppPermIssue) != "" ||
		strings.TrimSpace(b.RepoTargetIssue) != "" ||
		strings.TrimSpace(b.InferenceAuthError) != "" ||
		blockingGitHubAppState(b.GitHubAppState)
}

// reason returns a one-line human cause for the first blocker found, or "".
func (b hiveBlockers) reason() string {
	switch {
	case strings.TrimSpace(b.GitHubAppPermIssue) != "":
		return b.GitHubAppPermIssue
	case b.GitHubAppRequired:
		return "GitHub App not installed or not authorized"
	case blockingGitHubAppState(b.GitHubAppState):
		return "GitHub App state: " + b.GitHubAppState
	case strings.TrimSpace(b.RepoTargetIssue) != "":
		return b.RepoTargetIssue
	case b.RepoTargetMisconfigured:
		return "repo target misconfigured"
	case strings.TrimSpace(b.InferenceAuthError) != "":
		return b.InferenceAuthError
	default:
		return ""
	}
}

// blockingGitHubAppState reports whether a GitHubAppState string names a state
// that blocks pushes/merges. Empty and the healthy "ok"/"active" states do not
// block; anything else (e.g. "suspended", "perm_mismatch") does.
func blockingGitHubAppState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "ok", "active", "installed", "healthy":
		return false
	default:
		return true
	}
}

// agentVerdict is the full derived picture for one agent.
type agentVerdict struct {
	RunState       agentRunState
	CanOpenIssue   bool
	CanOpenPR      bool
	CanMerge       bool
	Able           bool   // can do everything its mode grants, with no blocker and not login-stuck
	Stuck          bool   // expected active but not actually working
	Impotent       bool   // actually running but not able
	QuietByDesign  bool   // paused or expected-off — never a fault
	Problem        bool   // THE alarm: governor expects it on, it can't deliver (any reason)
	CapabilityTier string // tierGreen | tierAmber | tierRed | tierGray
	BlockedReason  string // one human sentence; empty when Able
}

// interactiveLoginBackend reports whether a backend authenticates via an
// interactive CLI login (so NeedsLogin is a real blocker for it). Inference /
// API-key backends never sit on a login prompt, so a stray NeedsLogin there is
// not treated as a capability blocker. An empty backend (legacy spoke) is
// treated as interactive so we do not under-report a real login block.
func interactiveLoginBackend(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "vllm", "llm-d", "litellm", "watsonx":
		return false
	default:
		return true
	}
}

// legacyAgent reports whether the spoke omitted the new divergence fields — the
// telltale is that it reports none of the capabilities AND is not expected
// active. A capable or expected agent proves the spoke speaks the new protocol.
// Used only to keep the run-state machine from labeling a legacy agent "dead".
func legacyAgent(a AgentSummary) bool {
	return !a.ExpectedActive && !a.Enabled &&
		!a.CanOpenIssue && !a.CanOpenPR && !a.CanMerge && strings.TrimSpace(a.Backend) == ""
}

// deriveAgentVerdict computes the full three-leg verdict for one agent.
//
// queuedWork gates the idle rule (same as classifyInactiveAgent). now is passed
// for deterministic tests. blockers are the hive-level conditions on the row.
func deriveAgentVerdict(a AgentSummary, blockers hiveBlockers, queuedWork int, now time.Time) agentVerdict {
	v := agentVerdict{
		CanOpenIssue: a.CanOpenIssue,
		CanOpenPR:    a.CanOpenPR,
		CanMerge:     a.CanMerge,
	}

	paused := a.Paused || strings.EqualFold(a.State, agentStatePaused)
	running := strings.EqualFold(a.State, agentStateRunning)
	legacy := legacyAgent(a)

	// --- ACTUAL leg: reuse the existing inactivity state machine. ---
	kind := classifyInactiveAgent(a, queuedWork, now)
	switch {
	case paused:
		v.RunState = runQuietByDesign
	case !legacy && !a.ExpectedActive && !running:
		// Not scheduled to run in this mode and not running: off by design, not
		// a fault. (A legacy spoke reports ExpectedActive=false for everything,
		// so this branch is guarded to spokes that actually speak the field.)
		v.RunState = runQuietByDesign
	case kind == agentInactiveSessionMissing:
		v.RunState = runSessionGone
	case kind == agentInactiveNeedsLogin:
		v.RunState = runStuckAtLogin
	case kind == agentInactiveIdleWithWork:
		v.RunState = runIdleAtPrompt
	case running:
		v.RunState = runWorking
	case !legacy && a.ExpectedActive && a.Enabled:
		// Expected to run, configured on, but the spoke does not report it
		// running and it is not paused — it is down.
		v.RunState = runDead
	default:
		v.RunState = runUnknown
	}
	v.QuietByDesign = v.RunState == runQuietByDesign

	// --- ABLE leg. ---
	// Unknown for a legacy spoke: it reports no capabilities or expected/enabled
	// signals, so we cannot claim it is unable — leave Able=false, tier gray,
	// and never mark Impotent. Its bare state="running" is NOT trustworthy as
	// "working" here either: without the new signals we cannot tell working from
	// impotent, and rendering "working" next to a gray capability badge reads as
	// a contradiction (the Bluefin "off + working + ✗✗✗" confusion). Force the
	// whole row to UNKNOWN so a legacy spoke never looks like either health or a
	// fault — it looks like what it is: not yet reporting.
	if legacy {
		v.RunState = runUnknown
		v.QuietByDesign = false
		v.markUnknown()
		return v
	}

	loginBlocked := a.NeedsLogin && interactiveLoginBackend(a.Backend)
	hiveBlocked := blockers.any()
	blocked := loginBlocked || hiveBlocked

	// modeGrantsWrite: the agent's mode grants at least one write action (open
	// PR or merge) beyond opening issues. An advisory issues-only agent does
	// not — so it is fully able the moment it can open issues, and a write
	// blocker cannot make it "partial".
	modeGrantsWrite := a.CanOpenPR || a.CanMerge

	// capable = "if this agent were working, could it exercise its FULL mission?"
	// The CanOpen*/CanMerge booleans already encode the mode's ceiling (an
	// advisory issues-only agent simply reports CanMerge=false — not being able
	// to merge is not a fault for it). The only things that TAKE AWAY a granted
	// capability are an interactive login block or a hive-level blocker. We
	// require CanOpenIssue as the floor: an agent that cannot even open an issue
	// at this ACMM level has no reachable mission.
	capable := a.CanOpenIssue && !blocked

	// Able = actually able to fulfill its mission RIGHT NOW: capable AND truly
	// working. A capable-but-stopped or capable-but-paused agent is not doing
	// its mission, so it must not inflate the "K able" count next to "M
	// running". This makes the header read as a subset chain (expected ⊇
	// running ⊇ able) and makes IMPOTENT = running − able coherent.
	v.Able = capable && v.RunState == runWorking

	// Reason + tier.
	switch {
	case loginBlocked:
		v.BlockedReason = "sitting at login prompt"
	case hiveBlocked:
		v.BlockedReason = blockers.reason()
	case !a.CanOpenIssue && !a.CanOpenPR && !a.CanMerge:
		v.BlockedReason = "no write capability at this ACMM level"
	}

	// Tier reflects CAPABILITY (would it work if running), not liveness — a
	// healthy-but-paused agent still shows green so the badge answers "can this
	// agent do its job" independent of the run-state column beside it.
	//
	//   green — fully capable of its mission.
	//   amber — can still open issues, but a blocker takes away a WRITE its mode
	//           would otherwise grant (partial: half its job works).
	//   red   — cannot even open an issue (blocked at the floor, or no capability
	//           at this ACMM level).
	switch {
	case capable:
		v.CapabilityTier = tierGreen
	case a.CanOpenIssue && blocked && modeGrantsWrite:
		v.CapabilityTier = tierAmber
	default:
		v.CapabilityTier = tierRed
	}

	// --- DELTAS. ---
	// Suppressed for quiet-by-design agents: an off-schedule or paused agent is
	// neither stuck nor impotent.
	if !v.QuietByDesign {
		switch v.RunState {
		case runDead, runStuckAtLogin, runSessionGone, runIdleAtPrompt:
			// Only "stuck" when the governor actually expects it active now.
			v.Stuck = a.ExpectedActive
		}
		// IMPOTENT: running but not capable of its mission (blocked/gated). Uses
		// `capable`, not v.Able, because v.Able already requires runWorking —
		// an idle-at-prompt running agent that is blocked should still read as
		// impotent.
		if running && !capable {
			v.Impotent = true
		}
	}

	// PROBLEM is the ONE signal the operator scans for: the governor is calling
	// this agent (expected active) AND it cannot do its work — for ANY reason
	// (stuck at login, zombie session, dead, blocked, or running-but-impotent).
	// It deliberately unifies Stuck and Impotent so the view has a single "is
	// this a problem?" per agent. It is FALSE for paused/off-schedule agents
	// (quiet by design — the operator's own choice, not called) and for legacy/
	// unknown rows (returned early above — can't-verify is not broken).
	v.Problem = a.ExpectedActive && !v.QuietByDesign && !v.Able

	return v
}

// markUnknown sets the unknown/legacy capability tier and clears the derived
// capability flags so a legacy agent never reads as able or impotent.
func (v *agentVerdict) markUnknown() {
	v.Able = false
	v.Impotent = false
	v.CapabilityTier = tierGray
	v.BlockedReason = ""
}

// agentFleetRollup is the per-spoke divergence summary: the three counts the
// header shows, the delta totals, and the single Problems count that drives the
// hive's health dot and its sort position.
type agentFleetRollup struct {
	Expected int `json:"expected"`
	Running  int `json:"running"`
	Able     int `json:"able"`
	Stuck    int `json:"stuck"`
	Impotent int `json:"impotent"`
	// Problems is how many agents the governor expects on that cannot deliver
	// (any reason) — THE alarm count. >0 → the hive's dot is red and it sorts to
	// the top.
	Problems int `json:"problems"`
	// Known is how many agents reported the new divergence signals (non-legacy).
	// When Known==0 the whole hive is UNKNOWN (a spoke not yet rolled to this
	// build) and its dot is gray, never green — absence of a problem we cannot
	// see is not health.
	Known int `json:"known"`
}

// AgentVerdictJSON is the per-agent row the fleet view renders. It carries the
// raw heartbeat signals the browser displays (name/state/backend/paused
// provenance/last-activity) plus the derived verdict (run-state string,
// capability, deltas). runState is a string so the frontend never re-derives
// the state machine.
type AgentVerdictJSON struct {
	Name           string `json:"name"`
	Backend        string `json:"backend,omitempty"`
	Mode           string `json:"mode,omitempty"`
	Enabled        bool   `json:"enabled"`
	ExpectedActive bool   `json:"expectedActive"`
	State          string `json:"state,omitempty"`
	RunState       string `json:"runState"`
	LastActivityAt string `json:"lastActivityAt,omitempty"`
	Paused         bool   `json:"paused,omitempty"`
	PausedBy       string `json:"pausedBy,omitempty"`
	PausedTrigger  string `json:"pausedTrigger,omitempty"`
	PausedReason   string `json:"pausedReason,omitempty"`
	PausedAt       string `json:"pausedAt,omitempty"`
	CanOpenIssue   bool   `json:"canOpenIssue"`
	CanOpenPR      bool   `json:"canOpenPR"`
	CanMerge       bool   `json:"canMerge"`
	Able           bool   `json:"able"`
	CapabilityTier string `json:"capabilityTier"`
	Stuck          bool   `json:"stuck,omitempty"`
	Impotent       bool   `json:"impotent,omitempty"`
	QuietByDesign  bool   `json:"quietByDesign,omitempty"`
	// Problem is THE alarm: governor expects this agent on and it can't deliver.
	Problem bool `json:"problem,omitempty"`
	// Unknown means the spoke did not report the new divergence signals (legacy
	// build). The frontend renders these rows as "unknown", never as off/✗.
	Unknown       bool   `json:"unknown,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
}

// buildAgentVerdicts derives the per-agent verdict rows for one hive, skipping
// blank-named entries. Returns nil when the hive reports no usable agents.
func buildAgentVerdicts(agents []AgentSummary, blockers hiveBlockers, queuedWork int, now time.Time) []AgentVerdictJSON {
	var out []AgentVerdictJSON
	for _, a := range agents {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		v := deriveAgentVerdict(a, blockers, queuedWork, now)
		out = append(out, AgentVerdictJSON{
			Name:           a.Name,
			Backend:        a.Backend,
			Mode:           a.Mode,
			Enabled:        a.Enabled,
			ExpectedActive: a.ExpectedActive,
			State:          a.State,
			RunState:       v.RunState.String(),
			LastActivityAt: a.LastActivityAt,
			Paused:         a.Paused,
			PausedBy:       a.PausedBy,
			PausedTrigger:  a.PausedTrigger,
			PausedReason:   a.PausedReason,
			PausedAt:       a.PausedAt,
			CanOpenIssue:   v.CanOpenIssue,
			CanOpenPR:      v.CanOpenPR,
			CanMerge:       v.CanMerge,
			Able:           v.Able,
			CapabilityTier: v.CapabilityTier,
			Stuck:          v.Stuck,
			Impotent:       v.Impotent,
			QuietByDesign:  v.QuietByDesign,
			Problem:        v.Problem,
			Unknown:        v.CapabilityTier == tierGray,
			BlockedReason:  v.BlockedReason,
		})
	}
	return out
}

// rollupAgents summarizes a hive's agents into the divergence counts. It counts
// only agents the spoke actually reports on (skips blank names) and derives a
// verdict per agent via deriveAgentVerdict.
//
//   - Expected: agents the governor's current mode schedules active now.
//   - Running:  agents actually working (RunState == runWorking).
//   - Able:     agents that can do their full mission.
//
// A legacy spoke contributes zero to Expected/Able (unknown) but its running
// agents still count toward Running via the working state, so the header
// degrades gracefully rather than reading "0 running" for an old spoke.
func rollupAgents(agents []AgentSummary, blockers hiveBlockers, queuedWork int, now time.Time) agentFleetRollup {
	var r agentFleetRollup
	for _, a := range agents {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		v := deriveAgentVerdict(a, blockers, queuedWork, now)
		if v.CapabilityTier != tierGray {
			r.Known++
		}
		if a.ExpectedActive {
			r.Expected++
		}
		if v.RunState == runWorking {
			r.Running++
		}
		if v.Able {
			r.Able++
		}
		if v.Stuck {
			r.Stuck++
		}
		if v.Impotent {
			r.Impotent++
		}
		if v.Problem {
			r.Problems++
		}
	}
	return r
}

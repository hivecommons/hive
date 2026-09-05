package hub

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inferencehealth"
)

const (
	EnvAgentRestartProblemThreshold     = "HIVE_HUB_AGENT_RESTART_PROBLEM_THRESHOLD"
	DefaultAgentRestartProblemThreshold = 5
)

func agentRestartProblemThreshold() int {
	if raw := strings.TrimSpace(os.Getenv(EnvAgentRestartProblemThreshold)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultAgentRestartProblemThreshold
}

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
	// runQuotaExhausted — running but the provider/monthly quota is exhausted.
	runQuotaExhausted
	// runRestartStorm — the agent has restarted too often in the recent window.
	runRestartStorm
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
	case runQuotaExhausted:
		return "quota-exhausted"
	case runRestartStorm:
		return "restart-storm"
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
	ProviderLimitReason     string
	GatewayHealth           []inferencehealth.GatewayStatus
}

// any reports whether any hive-level blocker is set.
func (b hiveBlockers) any() bool {
	return b.GitHubAppRequired || b.RepoTargetMisconfigured ||
		strings.TrimSpace(b.GitHubAppPermIssue) != "" ||
		strings.TrimSpace(b.RepoTargetIssue) != "" ||
		strings.TrimSpace(b.InferenceAuthError) != "" ||
		strings.TrimSpace(b.ProviderLimitReason) != "" ||
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
	case strings.TrimSpace(b.ProviderLimitReason) != "":
		return b.ProviderLimitReason
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
		!a.CanOpenIssue && !a.CanOpenPR && !a.CanMerge &&
		!a.NeedsLogin && !a.QuotaExhausted && strings.TrimSpace(a.Backend) == ""
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
	case a.QuotaExhausted:
		v.RunState = runQuotaExhausted
	case agentRestartStorm(a):
		v.RunState = runRestartStorm
	case kind == agentInactiveNeedsLogin:
		// A login prompt outranks the off-schedule quiet branch below: a
		// wedged interactive credential is a HIVE-wide fault (every kick to
		// this backend will fail), not a scheduling choice — and spokes that
		// omit expectedActive on the wire (seen live: EPM + alchemy-logging,
		// three copilot agents each sitting at "Please use /login" with 40+
		// queued items) read as green "quiet by design" without this, which
		// is the exact at-a-glance failure the verdict exists to prevent.
		// classifyInactiveAgent already applies the paused check, the
		// running-state gate, and the 20-minute login grace.
		v.RunState = runStuckAtLogin
	case !legacy && !a.ExpectedActive:
		// Not scheduled to run in this mode: off by design, not a fault —
		// REGARDLESS of the reported session state. Spokes keep persistent
		// agent sessions alive between kicks, so an off-schedule agent still
		// reports state=running while it sits idle; labeling that "should be
		// working" contradicted the hive's own dashboard, which shows the
		// agent OFF in the current mode (seen live: a surge-mode hive with
		// every agent off rendered "4 running · should be working" on /fleet).
		// The governor's expectation is the schedule truth here, not the
		// session's existence. (A legacy spoke reports ExpectedActive=false
		// for everything, so this branch is guarded to spokes that actually
		// speak the field.)
		v.RunState = runQuietByDesign
	case kind == agentInactiveSessionMissing:
		v.RunState = runSessionGone
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
	quotaBlocked := a.QuotaExhausted
	gwFault, gatewayBlocked := gatewayFaultForBackend(blockers.GatewayHealth, a.Backend)
	hiveBlocked := blockers.any()
	// #5958: the spoke has given up relaunching this agent after repeated
	// identical start failures. It is not running and cannot be made to run by
	// anything the hive does on its own, so it can never be ABLE — counting it
	// toward "K able" is the specific over-count that let a hive show agents
	// green while none of them had started.
	startBlocked := strings.TrimSpace(a.StartBlockedReason) != ""
	blocked := loginBlocked || quotaBlocked || gatewayBlocked || hiveBlocked || startBlocked

	// modeGrantsWrite: the agent's mode grants at least one write action (open
	// PR or merge) beyond opening issues. An advisory issues-only agent does
	// not — so it is fully able the moment it can open issues, and a write
	// blocker cannot make it "partial".
	modeGrantsWrite := a.CanOpenPR || a.CanMerge

	// advisoryOnly: the ACMM level grants this agent NO GitHub writes at all —
	// not even opening issues. That is the operator's chosen maturity level,
	// not a fault: at these levels the agent's whole mission is advisory
	// (findings flow to the digest), so "cannot write" must never read as a
	// PROBLEM. Before this, every working advisory agent on an L1/L2 hive was
	// flagged "PROBLEM: no write capability at this ACMM level" on the fleet
	// page, drowning out real problems.
	advisoryOnly := !a.CanOpenIssue && !modeGrantsWrite

	// capable = "if this agent were working, could it exercise its FULL mission?"
	// The CanOpen*/CanMerge booleans already encode the mode's ceiling (an
	// advisory issues-only agent simply reports CanMerge=false — not being able
	// to merge is not a fault for it, and an advisory-only agent's mission
	// needs no GitHub write at all). The only things that TAKE AWAY a granted
	// capability are an interactive login block or a hive-level blocker.
	capable := (a.CanOpenIssue || advisoryOnly) && !blocked

	// Able = actually able to fulfill its mission RIGHT NOW: capable AND truly
	// working. A capable-but-stopped or capable-but-paused agent is not doing
	// its mission, so it must not inflate the "K able" count next to "M
	// running". This makes the header read as a subset chain (expected ⊇
	// running ⊇ able) and makes IMPOTENT = running − able coherent.
	v.Able = capable && v.RunState == runWorking

	// Reason + tier. An unblocked advisory-only agent carries NO reason: its
	// lack of write capability is the ACMM level working as designed.
	switch {
	// First: the spoke has already diagnosed this one concretely, and its reason
	// ("copilot: not logged in") beats every generic phrasing below — including
	// "sitting at login prompt", which describes the same fault less usefully
	// and without saying that relaunching has been given up on.
	case startBlocked:
		v.BlockedReason = a.StartBlockedReason
	case loginBlocked:
		v.BlockedReason = "sitting at login prompt"
	case quotaBlocked:
		v.BlockedReason = "provider quota exhausted"
	case agentRestartStorm(a):
		v.BlockedReason = agentRestartProblemReason(a)
	case gatewayBlocked:
		v.BlockedReason = inferencehealth.Reason(gwFault)
	case hiveBlocked:
		v.BlockedReason = blockers.reason()
	}

	// Tier reflects CAPABILITY (would it work if running), not liveness — a
	// healthy-but-paused agent still shows green so the badge answers "can this
	// agent do its job" independent of the run-state column beside it.
	//
	//   green — fully capable of its mission (for an advisory-only agent, the
	//           mission is the digest — no GitHub write required).
	//   amber — can still open issues, but a blocker takes away a WRITE its mode
	//           would otherwise grant (partial: half its job works).
	//   red   — cannot do its mission at all: the floor is blocked, or its
	//           inference gateway cannot serve any turn.
	switch {
	case capable:
		v.CapabilityTier = tierGreen
	case a.CanOpenIssue && blocked && modeGrantsWrite && !gatewayBlocked && !startBlocked:
		// Amber means "half its job works". An agent that never started does no
		// half of its job, so it is excluded here for the same reason a dead
		// gateway is — the badge must not read as partial capability when the
		// CLI is not running at all.
		v.CapabilityTier = tierAmber
	default:
		v.CapabilityTier = tierRed
	}

	// --- DELTAS. ---
	// Suppressed for quiet-by-design agents: an off-schedule or paused agent is
	// neither stuck nor impotent.
	if !v.QuietByDesign {
		switch v.RunState {
		case runDead, runSessionGone, runIdleAtPrompt:
			// Only "stuck" when the governor actually expects it active now.
			v.Stuck = a.ExpectedActive
		case runStuckAtLogin:
			// A wedged login is stuck regardless of the schedule: the broken
			// credential is hive-wide and the wire may omit expectedActive
			// entirely (the EPM/alchemy case) — gating on it hid the fault.
			v.Stuck = true
		case runQuotaExhausted, runRestartStorm:
			// Provider quota exhaustion is a hive/provider fault even if the
			// schedule bit is absent on the wire.
			v.Stuck = true
		}
		// An agent the spoke has stopped relaunching is stuck by definition, in
		// whatever run-state the wire reports it (#5958). Set after the switch so
		// it cannot be lost to a run-state the cases above do not name.
		if startBlocked {
			v.Stuck = true
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
	//
	// runStuckAtLogin bypasses the expectedActive gate: the credential fault is
	// real whether or not the wire carries the schedule bit (see the ACTUAL-leg
	// ordering above).
	// A start-blocked agent is deliberately NOT given a bypass here, unlike
	// runStuckAtLogin. A wedged interactive credential is hive-wide — every kick
	// to that backend fails — so it is a fault whatever the schedule says. A
	// failed start is one agent, and if the governor is not scheduling that
	// agent in this mode, the operator has not asked it to run and must not be
	// alarmed about it (#5958). It is still never ABLE: the quiet-by-design run
	// state already denies that above, so the count stays honest without the
	// alarm. When the mode next schedules the agent, ExpectedActive carries it
	// into this gate on its own.
	v.Problem = (a.ExpectedActive || v.RunState == runStuckAtLogin || v.RunState == runQuotaExhausted || v.RunState == runRestartStorm) && !v.QuietByDesign && !v.Able

	return v
}

func agentRestartStorm(a AgentSummary) bool {
	return a.Restarts.Last24h >= agentRestartProblemThreshold()
}

func agentRestartProblemReason(a AgentSummary) string {
	if !agentRestartStorm(a) {
		return ""
	}
	reason := strings.TrimSpace(a.Restarts.LastReason)
	if reason == "" {
		return fmt.Sprintf("agent restarts: %s ×%d/24h", a.Name, a.Restarts.Last24h)
	}
	return fmt.Sprintf("agent restarts: %s ×%d/24h (%s)", a.Name, a.Restarts.Last24h, reason)
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
	// Paused is how many expected-active agents are deliberately quiet because
	// the operator paused them. Live all-paused hives (aslom/hive-agent,
	// castrojo/endusers, hashicorp/dev-portal, inference-sim/sim2real, and
	// peers) reported Expected>0, Running=0, Problems=0 and therefore fell
	// into the false "no agents running" outage leg. Pause is an operator
	// choice, not a dead session; count it separately so hive health can say
	// "all agents paused" instead of inventing a crash.
	Paused   int `json:"paused,omitempty"`
	Stuck    int `json:"stuck"`
	Impotent int `json:"impotent"`
	// Problems is how many agents the governor expects on that cannot deliver
	// (any reason) — THE alarm count. >0 → the hive's dot is red and it sorts to
	// the top.
	Problems int `json:"problems"`
	// LoginStuck is how many of the Problems are interactive agents wedged at a
	// login prompt. When every problem is a login block, the hive verdict can
	// name the single actionable cause ("re-login needed") instead of the
	// generic "N agent(s) blocked".
	LoginStuck int `json:"loginStuck,omitempty"`
	// IdleWithWork is how many of the Problems are running agents sitting idle
	// past the threshold while the hive has queued work. Named separately so
	// the verdict can say "idle with work queued" — the remedy (kick/schedule)
	// differs from a dead session or a login wedge.
	IdleWithWork int `json:"idleWithWork,omitempty"`
	// DeadOrGone is how many of the Problems have no live session at all
	// (dead, or running-with-session-missing zombies). When every problem is
	// in this class the verdict keeps the familiar "no agents running".
	DeadOrGone int `json:"deadOrGone,omitempty"`
	// QuotaExhausted is how many Problems are provider/monthly quota limited.
	QuotaExhausted int `json:"quotaExhausted,omitempty"`
	RestartStorms int `json:"restartStorms,omitempty"`
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
	Name            string `json:"name"`
	Backend         string `json:"backend,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Enabled         bool   `json:"enabled"`
	ExpectedActive  bool   `json:"expectedActive"`
	KickIntervalSec int64  `json:"kickIntervalSec,omitempty"`
	State           string `json:"state,omitempty"`
	RunState        string `json:"runState"`
	LastActivityAt  string `json:"lastActivityAt,omitempty"`
	Paused          bool   `json:"paused,omitempty"`
	PausedBy        string `json:"pausedBy,omitempty"`
	PausedTrigger   string `json:"pausedTrigger,omitempty"`
	PausedReason    string `json:"pausedReason,omitempty"`
	PausedAt        string `json:"pausedAt,omitempty"`
	QuotaExhausted  bool   `json:"quotaExhausted,omitempty"`
	CanOpenIssue    bool   `json:"canOpenIssue"`
	CanOpenPR       bool   `json:"canOpenPR"`
	CanMerge        bool   `json:"canMerge"`
	Able            bool   `json:"able"`
	CapabilityTier  string `json:"capabilityTier"`
	Stuck           bool   `json:"stuck,omitempty"`
	Impotent        bool   `json:"impotent,omitempty"`
	QuietByDesign   bool   `json:"quietByDesign,omitempty"`
	// Problem is THE alarm: governor expects this agent on and it can't deliver.
	Problem bool `json:"problem,omitempty"`
	// Unknown means the spoke did not report the new divergence signals (legacy
	// build). The frontend renders these rows as "unknown", never as off/✗.
	Unknown       bool   `json:"unknown,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
	RestartProblem string `json:"restartProblem,omitempty"`
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
			Name:            a.Name,
			Backend:         a.Backend,
			Mode:            a.Mode,
			Enabled:         a.Enabled,
			ExpectedActive:  a.ExpectedActive,
			KickIntervalSec: a.KickIntervalSec,
			State:           a.State,
			RunState:        v.RunState.String(),
			LastActivityAt:  a.LastActivityAt,
			Paused:          a.Paused,
			PausedBy:        a.PausedBy,
			PausedTrigger:   a.PausedTrigger,
			PausedReason:    a.PausedReason,
			PausedAt:        a.PausedAt,
			QuotaExhausted:  a.QuotaExhausted,
			CanOpenIssue:    v.CanOpenIssue,
			CanOpenPR:       v.CanOpenPR,
			CanMerge:        v.CanMerge,
			Able:            v.Able,
			CapabilityTier:  v.CapabilityTier,
			Stuck:           v.Stuck,
			Impotent:        v.Impotent,
			QuietByDesign:   v.QuietByDesign,
			Problem:         v.Problem,
			Unknown:         v.CapabilityTier == tierGray,
			BlockedReason:   v.BlockedReason,
			RestartProblem:  agentRestartProblemReason(a),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li == lj {
			return out[i].Name < out[j].Name
		}
		return li < lj
	})
	return out
}

// agentRosterMismatch is the additive warning shown when a hive's reported
// agent names drift from the ACMM pack that defines its expected roster.
type agentRosterMismatch struct {
	Level      int      `json:"level"`
	Missing    []string `json:"missing,omitempty"`
	Unexpected []string `json:"unexpected,omitempty"`
	Reason     string   `json:"reason"`
}

func computeAgentRosterMismatch(level int, agents []AgentSummary) *agentRosterMismatch {
	if level <= 0 || len(agents) == 0 {
		return nil
	}
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return nil
	}
	expected := make(map[string]struct{}, len(pack.Agents))
	for _, a := range pack.Agents {
		name := strings.TrimSpace(a.Name)
		if name == "" || a.Hidden {
			continue
		}
		expected[name] = struct{}{}
	}
	actual := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		if !agent.AgentAvailableAtACMMLevel(name, level) {
			continue
		}
		actual[name] = struct{}{}
	}
	var missing, unexpected []string
	for name := range expected {
		if _, ok := actual[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range actual {
		if _, ok := expected[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)
	if len(missing) == 0 && len(unexpected) == 0 {
		return nil
	}
	parts := make([]string, 0, 2)
	if len(missing) > 0 {
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(unexpected) > 0 {
		parts = append(parts, "unexpected: "+strings.Join(unexpected, ", "))
	}
	return &agentRosterMismatch{
		Level:      level,
		Missing:    missing,
		Unexpected: unexpected,
		Reason:     fmt.Sprintf("agent roster mismatch for L%d: %s", level, strings.Join(parts, "; ")),
	}
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
		if a.ExpectedActive && (a.Paused || strings.EqualFold(a.State, agentStatePaused)) {
			r.Paused++
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
			switch v.RunState {
			case runStuckAtLogin:
				r.LoginStuck++
			case runIdleAtPrompt:
				r.IdleWithWork++
			case runQuotaExhausted:
				r.QuotaExhausted++
			case runRestartStorm:
				r.RestartStorms++
			case runDead, runSessionGone:
				r.DeadOrGone++
			case runWorking:
				if v.BlockedReason == "sitting at login prompt" {
					// Login prompts are actionable the moment the ABLE leg sees
					// NeedsLogin on an interactive backend, even while the ACTUAL
					// leg is still inside its 20-minute grace and therefore says
					// runWorking. Live "placeholder/" and available-akswec2 pool
					// hives hit this shape: Problems>0 but LoginStuck stayed zero,
					// so /fleet hid the cause behind "N agent(s) blocked". Bucket
					// by the blocked reason as a fallback so the hive chip tells
					// the operator to re-login.
					r.LoginStuck++
				}
			}
		}
	}
	return r
}

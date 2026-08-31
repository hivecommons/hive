package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/governor"
	"github.com/kubestellar/hive/pkg/planning"
	"github.com/kubestellar/hive/pkg/resolve"
	"github.com/kubestellar/hive/pkg/skillreg"
	"github.com/kubestellar/hive/pkg/tokens"
	"github.com/kubestellar/hive/pkg/watchdog"
)

// skillsConventionalDir is the conventional on-disk location the dashboard
// probes for a skills registry. It is the same directory the scheduler loads at
// kick time (scheduler.skillsRegistryDir), so the count reported here and the
// skills actually injected into an agent's context come from one place. The
// load stays best-effort and optional: an absent directory reports "not
// configured" rather than an error.
//
// It is a var, not a const, purely so tests can point it at a temp dir.
var skillsConventionalDir = dataVolumePath + "/skills"

const defaultLookbackHours = 24

// Cadence sentinel values: a per-mode cadence set to one of these means the
// governor will not kick the agent on a timer in that mode. Named constants so
// the two next-kick computations below agree on the exact spellings the config
// layer emits, rather than each carrying its own literal list — the drift these
// were introduced to prevent, and which had already started before they were
// wired in.
//
// config.Cadence.IsPaused() remains the authority for the interval-mode
// judgment (it also accepts "paused" and "0"); these are the string-cadence
// spellings the dashboard's own formatting paths compare against.
const (
	cadencePause    = "pause"
	cadenceOff      = "off"
	cadenceOnDemand = "on demand"
)

// SetProxyViolationsProvider registers a function that returns per-agent
// proxy violation counts, called during status builds.
func SetProxyViolationsProvider(fn func() map[string]int) {
	proxyViolationsMu.Lock()
	defer proxyViolationsMu.Unlock()
	proxyViolationsFn = fn
}

func getProxyViolationsFn() func() map[string]int {
	proxyViolationsMu.RLock()
	defer proxyViolationsMu.RUnlock()
	return proxyViolationsFn
}

// SetBackendAuthProvider registers a function reporting whether shared
// credentials exist for a CLI backend (available, known). Lets the status
// build show honest auth state for agents with no running pane.
func SetBackendAuthProvider(fn func(backend string) (bool, bool)) {
	backendAuthMu.Lock()
	defer backendAuthMu.Unlock()
	backendAuthFn = fn
}

func getBackendAuthFn() func(backend string) (bool, bool) {
	backendAuthMu.RLock()
	defer backendAuthMu.RUnlock()
	return backendAuthFn
}

// SetAgentAuthProvider registers a PER-AGENT auth probe (agent.Manager's
// AgentAuthAvailable). It supersedes the backend-level SetBackendAuthProvider
// wherever it is registered, because the backend-level probe stats ONE shared
// credential path (/data/home/.claude/.credentials.json) that is empty under
// the per-agent-UID layout — every agent then reported (known, !available) and
// the dashboard painted a 🔑 "needs login" badge on agents that were running,
// being kicked, and passing deep health. The per-agent probe resolves the
// agent's OWN home and gates on whether the backend has an interactive login
// at all. The backend-level provider is retained as a fallback for callers
// that only know a backend name.
func SetAgentAuthProvider(fn func(agentName string) (bool, bool)) {
	backendAuthMu.Lock()
	defer backendAuthMu.Unlock()
	agentAuthFn = fn
}

func getAgentAuthFn() func(agentName string) (bool, bool) {
	backendAuthMu.RLock()
	defer backendAuthMu.RUnlock()
	return agentAuthFn
}

var (
	backendAuthMu sync.RWMutex
	backendAuthFn func(backend string) (bool, bool)
	agentAuthFn   func(agentName string) (bool, bool)

	cachedHealth   map[string]any
	cachedHealthMu sync.RWMutex

	proxyViolationsMu sync.RWMutex
	proxyViolationsFn func() map[string]int

	entitledModelsMu sync.RWMutex
	entitledModelsFn func(endpoint string) (models []string, source string, known bool)

	inferenceAuthMu sync.RWMutex
	inferenceAuthFn func() (errMsg string, since time.Time)
	// inferenceBudget* carry the PROVIDER spending-limit signal (#4294) —
	// distinct from the auth signal above and from the hive's own token budget.
	inferenceBudgetMu sync.RWMutex
	inferenceBudgetFn func() (errMsg string, since, lastRebuff time.Time, rebuffs int)
)

// SetEntitledModelsProvider registers a function that reports the per-key
// entitled model set the proxy has learned for a LiteLLM endpoint (from a
// key-info probe or a "team not allowed" 403). The dashboard uses it to narrow
// the LiteLLM model dropdown to models the configured key can actually use.
func SetEntitledModelsProvider(fn func(endpoint string) (models []string, source string, known bool)) {
	entitledModelsMu.Lock()
	defer entitledModelsMu.Unlock()
	entitledModelsFn = fn
}

func getEntitledModelsFn() func(endpoint string) (models []string, source string, known bool) {
	entitledModelsMu.RLock()
	defer entitledModelsMu.RUnlock()
	return entitledModelsFn
}

// SetInferenceAuthProvider registers a function reporting the proxy's current
// inference-backend auth-failure signal: a non-empty, log-safe cause string
// (and the time it first latched) ONLY while the backend has been auth-failing
// for several consecutive calls, empty otherwise. The heartbeat builder reports
// it to the hub so a hive silently 401'ing on every inference call surfaces as
// a health signal instead of merely looking quiet. Wired to the proxy's
// InferenceAuthError; nil in tests and on spokes with no proxy, which the
// accessors below tolerate.
func SetInferenceAuthProvider(fn func() (errMsg string, since time.Time)) {
	inferenceAuthMu.Lock()
	defer inferenceAuthMu.Unlock()
	inferenceAuthFn = fn
}

func getInferenceAuthFn() func() (errMsg string, since time.Time) {
	inferenceAuthMu.RLock()
	defer inferenceAuthMu.RUnlock()
	return inferenceAuthFn
}

// SetInferenceBudgetProvider registers a function reporting the proxy's current
// PROVIDER spending-limit signal (kubestellar/hive#4294): a non-empty log-safe
// cause, when it latched, and how many rebuffs have been seen — only while the
// provider is refusing on a money limit.
//
// This is deliberately a separate provider from the auth one: an operator
// facing "the gateway rejects our key" and one facing "the gateway will not
// spend more money today" take different actions, and folding them into one
// signal would tell neither of them which they have. Wired to the proxy's
// InferenceBudgetExceeded; nil in tests and on spokes with no proxy, which the
// accessor tolerates.
func SetInferenceBudgetProvider(fn func() (errMsg string, since, lastRebuff time.Time, rebuffs int)) {
	inferenceBudgetMu.Lock()
	defer inferenceBudgetMu.Unlock()
	inferenceBudgetFn = fn
}

// InferenceBudgetExceeded returns the current provider spending-limit signal,
// or zero values when no provider is registered or the provider is serving.
//
// lastRebuff (the most recent rebuff, which moves forward; since does not) is
// carried through because the eval cycle suppresses kicks only while it is
// fresh — suppressing on the latch alone would withhold the very traffic that
// clears the latch. See pkg/proxy/inference_budget.go.
func InferenceBudgetExceeded() (errMsg string, since, lastRebuff time.Time, rebuffs int) {
	inferenceBudgetMu.RLock()
	fn := inferenceBudgetFn
	inferenceBudgetMu.RUnlock()
	if fn == nil {
		return "", time.Time{}, time.Time{}, 0
	}
	return fn()
}

// InferenceAuthError returns the current inference-backend auth-failure signal
// from the registered provider, or ("", zero) when none is registered or the
// backend is healthy. It is the single source both heartbeat fields read (the
// AdvisoryError fold and the dedicated inference-auth field) so they can never
// disagree about whether a hive is auth-failing.
func InferenceAuthError() (errMsg string, since time.Time) {
	fn := getInferenceAuthFn()
	if fn == nil {
		return "", time.Time{}
	}
	return fn()
}

// AgentStatusPayload is a lightweight payload containing only agent metadata,
// broadcast on a fast cadence (every ~10s) independent of the full eval cycle.
type AgentStatusPayload struct {
	Timestamp        string                    `json:"timestamp"`
	Agents           []FrontendAgent           `json:"agents"`
	ConfiguredAgents []FrontendConfiguredAgent `json:"configuredAgents"`
	GovMode          string                    `json:"govMode"`
}

// BuildAgentOnlyStatus builds a lightweight agent-only status from in-memory
// data. No GitHub API calls, no metrics collection — just agent state.
func BuildAgentOnlyStatus(
	govState governor.State,
	agentStatuses map[string]*agent.AgentProcess,
	cfg *config.Config,
) *AgentStatusPayload {
	return &AgentStatusPayload{
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		Agents:           buildAgents(agentStatuses, cfg, govState),
		ConfiguredAgents: buildConfiguredAgents(cfg),
		GovMode:          strings.ToLower(string(govState.Mode)),
	}
}

func BuildFrontendStatus(
	govState governor.State,
	actionable *github.ActionableResult,
	agentStatuses map[string]*agent.AgentProcess,
	cfg *config.Config,
	tokenCollector *tokens.Collector,
	gov *governor.Governor,
	beadStores map[string]*beads.Store,
	ghClient *github.Client,
	ctx context.Context,
	metricsCollector *MetricsCollector,
) *StatusPayload {
	agentMetrics := map[string]any{}
	if metricsCollector != nil {
		agentMetrics = metricsCollector.Get()
	}

	issueToMerge := buildIssueToMerge(metricsCollector)

	payload := &StatusPayload{
		Timestamp:           time.Now().UTC().Format(time.RFC3339),
		HiveID:              cfg.HiveID,
		Agents:              buildAgents(agentStatuses, cfg, govState),
		ConfiguredAgents:    buildConfiguredAgents(cfg),
		Governor:            buildGovernor(govState, cfg),
		Tokens:              buildTokens(tokenCollector),
		Repos:               buildRepos(cfg, actionable),
		Beads:               BuildBeadsFromConfig(beadStores, cfg),
		Planning:            BuildPlanning(beadStores, architectPausedFromStatuses(agentStatuses), detectACMMLevel(cfg)),
		Health:              buildHealth(ghClient, ctx),
		Budget:              buildBudget(gov, tokenCollector),
		CadenceMatrix:       buildCadenceMatrix(cfg, agentStatuses),
		GHRateLimits:        buildGHRateLimits(ghClient, ctx, cfg),
		AgentMetrics:        agentMetrics,
		Hold:                buildHold(actionable),
		IssueToMerge:        issueToMerge,
		ACMMLevel:           detectACMMLevel(cfg),
		ACMMLevelConfigured: cfg.ACMMLevel != nil,
		ACMMPackAgents:      buildACMMPackAgents(cfg),
		SystemResources:     collectSystemResources(),
		Platform:            buildPlatform(cfg),
		Security:            buildSecurity(cfg),
	}
	return payload
}

// agentDisabledInConfig reports whether an agent is switched OFF in config —
// `enabled: false` — as opposed to merely not running right now.
//
// The distinction matters because a disabled agent is never started, so it has
// no tmux session and no captured pane, yet it still appears in the runtime
// roster: ApplyPack adds every agent in the ACMM level's pack to the manager
// and gates only the Start on Enabled (api_packs.go). That is deliberate — the
// runtime card is how an operator turns the agent back on — but it means every
// consumer reading "not running" has to ask WHY before calling it a fault.
//
// The live config wins; proc.Config is the manager's copy and only refreshes
// on UpdateConfig/reconcile. Callers must check "running" FIRST: an agent
// disabled while it is still up keeps running until the next reconcile, and it
// genuinely does have a session until then.
func agentDisabledInConfig(cfg *config.Config, name string, proc *agent.AgentProcess) bool {
	if cfg != nil {
		if liveCfg, ok := cfg.Agents[name]; ok {
			return !liveCfg.Enabled
		}
	}
	if proc != nil {
		return !proc.Config.Enabled
	}
	return false
}

func buildConfiguredAgents(cfg *config.Config) []FrontendConfiguredAgent {
	if cfg == nil {
		return []FrontendConfiguredAgent{}
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := cfg.Agents[names[i]], cfg.Agents[names[j]]
		if left.GetSortOrder() != right.GetSortOrder() {
			return left.GetSortOrder() < right.GetSortOrder()
		}
		return names[i] < names[j]
	})

	acmmLevel := 0
	if cfg.ACMMLevel != nil {
		acmmLevel = *cfg.ACMMLevel
	}

	agents := make([]FrontendConfiguredAgent, 0, len(names))
	for _, name := range names {
		if !agent.AgentAvailableAtACMMLevel(name, acmmLevel) {
			continue
		}
		agentCfg := cfg.Agents[name]
		// Same resolution buildAgents uses: explicit per-agent Mode wins,
		// otherwise the ACMM level default for this agent.
		mode := agent.DefaultAgentMode(name, acmmLevel)
		if modeStr := agentCfg.Mode; modeStr != "" {
			if parsed, ok := agent.ParseAgentMode(modeStr); ok {
				mode = parsed
			}
		}
		agents = append(agents, FrontendConfiguredAgent{
			Name: name, DisplayName: agentCfg.DisplayName,
			Description: agentCfg.Description, SortOrder: agentCfg.GetSortOrder(),
			Emoji: agentCfg.Emoji, Color: agentCfg.Color, Enabled: agentCfg.Enabled,
			ModelOwner: agentCfg.ModelOwner, BackendOwner: agentCfg.BackendOwner,
			Mode: mode.String(), ModeEmoji: mode.Emoji(),
		})
	}
	return agents
}

// buildSecurity summarizes operator-facing security posture from effective
// config. It is additive and secret-free, so older dashboards can ignore it.
func buildSecurity(cfg *config.Config) *FrontendSecurity {
	sec := &FrontendSecurity{}
	if cfg == nil {
		return sec
	}
	sec.IntentEnforced = cfg.Intent.Enforce
	sec.IoscanEnabled = cfg.Ioscan.IsEnabled()
	sec.IoscanFailMode = "open"
	if cfg.Ioscan.FailClosed() {
		sec.IoscanFailMode = "closed"
	}
	sec.IoscanCanaries = cfg.Ioscan.Canaries
	sec.ReviewRequireApproval = cfg.Review.RequireApproval
	sec.ReviewFanOut = cfg.Review.FanOut
	sec.RetroEnabled = cfg.Retro.Enabled
	sec.OTelEnabled = cfg.EffectiveOTel().Enabled
	sec.SandboxEnabled = cfg.AgentSandbox.Enabled
	sec.TotalAgents = len(cfg.Agents)
	for name, a := range cfg.Agents {
		if a.SandboxEnabled(cfg.AgentSandbox) {
			sec.SandboxedAgents++
		}
		if dashboardAgentReviewCapable(name, a, cfg.Review.ReviewerAgents) {
			sec.ReviewCapableAgents++
		}
	}
	return sec
}

func dashboardAgentReviewCapable(name string, a config.AgentConfig, allowed []string) bool {
	if !a.Enabled || a.Paused || a.OnDemand {
		return false
	}
	if len(allowed) > 0 {
		for _, n := range allowed {
			if strings.TrimSpace(n) == name {
				return true
			}
		}
		return false
	}
	fields := append([]string{name, a.Role}, a.Aliases...)
	fields = append(fields, a.LaneKeywords...)
	fields = append(fields, a.DetectKeywords...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), "review") {
			return true
		}
	}
	return false
}

// buildPlatform surfaces the v4 spoke capabilities (forge, mint, skills) from
// config. It is nil-safe and never panics: a nil config yields a zero-value
// block (github forge, mint off, skills unavailable), matching the additive
// contract that a github-only, mint-off hive sees no behavior change. No secret
// or key material is ever exposed — the mint block reports only enabled/issuer/
// keyPresent.
func buildPlatform(cfg *config.Config) *FrontendPlatform {
	p := &FrontendPlatform{
		Forge:  FrontendForge{Kind: config.ForgeGitHub, Repos: []string{}},
		Skills: buildSkills(),
	}
	if cfg == nil {
		return p
	}

	// --- Forge ---
	p.Forge.Kind = cfg.Project.ForgeKind()
	p.Forge.PrimaryRepo = cfg.Project.PrimaryRepo
	repos := cfg.Project.Repos
	if repos == nil {
		repos = []string{}
	}
	p.Forge.Repos = repos
	p.Forge.RepoCount = len(repos)
	// InstanceURL is reported only for a non-default self-managed instance;
	// the public SaaS defaults (github.com / gitlab.com) stay empty so the UI
	// shows a URL only when it is meaningful.
	switch p.Forge.Kind {
	case config.ForgeGitLab:
		if url := cfg.GitLab.InstanceURL(); url != "" && url != config.DefaultGitLabURL {
			p.Forge.InstanceURL = url
		}
	case config.ForgeGitea:
		// Gitea has no public default, so any configured URL is meaningful.
		p.Forge.InstanceURL = cfg.Gitea.InstanceURL()
	}

	// --- Mint --- (never expose key material — enabled/issuer/keyPresent only)
	p.Mint = FrontendMint{
		Enabled: cfg.Mint.Enabled,
		Issuer:  cfg.Mint.Issuer,
	}
	if cfg.Mint.KeyPath != "" {
		if _, err := os.Stat(cfg.Mint.KeyPath); err == nil {
			p.Mint.KeyPresent = true
		}
	}

	return p
}

// buildSkills probes the conventional skills directory and reports the registry
// state. The registry is not wired into the runtime yet, so this is best-effort
// and honest-empty: when the directory does not exist it reports available=false
// / loaded=0 ("not configured") rather than fabricating a count. When the dir is
// present, skillreg.Load tolerates unreadable/malformed files and returns the
// number of skills successfully loaded.
func buildSkills() FrontendSkills {
	dir := skillsConventionalDir
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return FrontendSkills{Available: false, Loaded: 0}
	}
	// Load tolerates a missing/unreadable dir and bad files; a returned error is
	// only for a truly unexpected condition, so treat any error as "unavailable".
	loaded, lErr := skillreg.NewRegistry().Load(dir, nil)
	if lErr != nil {
		return FrontendSkills{Available: false, Loaded: 0, Dir: dir}
	}
	return FrontendSkills{Available: true, Loaded: loaded, Dir: dir}
}

func buildAgents(statuses map[string]*agent.AgentProcess, cfg *config.Config, govState governor.State) []FrontendAgent {
	currentMode := strings.ToLower(string(govState.Mode))

	// The watchdog's authority is hive-wide, but it rides each agent's payload
	// because it is only meaningful next to that agent's conditions: it is what
	// tells a reader whether a condition implied an action actually taken.
	watchdogMode := ""
	if cfg != nil {
		if s, _ := watchdog.SettingsFrom(cfg.Governor.Watchdog); s.Enabled() {
			watchdogMode = string(s.Mode)
		}
	}

	packAllowed := acmmPackAllowedSet(cfg)
	onDemandSet := config.OnDemandAgentsFromPacks()

	names := make([]string, 0, len(statuses))
	for name := range statuses {
		if packAllowed != nil && !packAllowed[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		orderI := 100
		orderJ := 100
		if agentI, ok := cfg.Agents[names[i]]; ok {
			orderI = agentI.GetSortOrder()
		}
		if agentJ, ok := cfg.Agents[names[j]]; ok {
			orderJ = agentJ.GetSortOrder()
		}
		if orderI != orderJ {
			return orderI < orderJ
		}
		return names[i] < names[j]
	})

	agents := make([]FrontendAgent, 0, len(statuses))
	for _, name := range names {
		proc := statuses[name]
		cli := proc.Config.Backend
		if proc.BackendOverride != "" {
			cli = proc.BackendOverride
		}
		model := proc.Config.Model
		if proc.ModelOverride != "" {
			model = proc.ModelOverride
		}

		busy := "idle"
		if proc.State == agent.StateRunning {
			busy = "working"
		}

		lastKick := ""
		if proc.LastKick != nil {
			lastKick = formatHumanTime(*proc.LastKick)
		}

		cadenceValue := lookupCadenceValueForMode(name, currentMode, cfg)
		if cadenceValue == "" {
			cadenceValue = lookupCadenceValue(name, cfg)
		}
		cadence := cadenceDisplay(cadenceValue)
		nextKick := computeNextKickFromCadence(proc.LastKick, cadenceValue)

		// offByCadence: the agent's cadence for the CURRENT governor mode is a
		// non-kicking value ("pause"/"off"), so the governor will never kick it
		// even though the process itself may be perfectly healthy. This is how
		// a hive stuck in SURGE (where surge:{scanner:pause,...}) leaves every
		// agent alive-but-off. The dashboard surfaces this as a hollow-green
		// dot so operators do not mistake a governor-paused agent for a running
		// one. On-demand agents are excluded — they are intentionally not on a
		// cadence and carry their own on-demand styling.
		//
		// Derived from the shared cadence resolver so it agrees exactly with the
		// heartbeat's ExpectedActive (config.ExpectedActive): both read the same
		// per-mode cadence and the same IsPaused semantics. offByCadence is the
		// stricter "has a pause/off ENTRY" form (an ABSENT cadence is not off),
		// which is why it checks the resolved cadence directly rather than
		// negating ExpectedActive.
		modeCadence := cfg.CadenceValueForMode(name, currentMode)
		offByCadence := modeCadence != "" && modeCadence.IsPaused() &&
			!proc.Config.OnDemand && !onDemandSet[name]

		pinnedCli := proc.PinnedCLI != "" || proc.Config.CLIPinned
		pinnedModel := proc.PinnedModel != ""

		const summaryLines = 20
		var liveSummary string
		if pane := proc.PaneLines(summaryLines); len(pane) > 0 {
			liveSummary = redactTokens(strings.Join(pane, "\n"))
		}
		var detailSummary string
		const maxDetailLines = 50
		if buf := proc.OutputBuffer; buf != nil && buf.Count() > 0 {
			lines := agent.DeduplicateBlocks(buf.Last(buf.Count()))
			if len(lines) > maxDetailLines {
				lines = lines[len(lines)-maxDetailLines:]
			}
			detailSummary = redactTokens(strings.Join(lines, "\n"))
		} else if pane := proc.FilteredPaneLines(0); len(pane) > 0 {
			if len(pane) > maxDetailLines {
				pane = pane[len(pane)-maxDetailLines:]
			}
			detailSummary = redactTokens(strings.Join(pane, "\n"))
		}

		agentID := proc.ID
		if agentID == "" {
			agentID = name
		}

		agentCfg := proc.Config
		a := FrontendAgent{
			Name:          name,
			ID:            agentID,
			DisplayName:   agentCfg.DisplayName,
			Description:   agentCfg.Description,
			Role:          agentCfg.Role,
			SortOrder:     agentCfg.GetSortOrder(),
			Emoji:         agentCfg.Emoji,
			Color:         agentCfg.Color,
			BeadRole:      agentCfg.GetBeadRole(),
			Managed:       agentCfg.Managed,
			Enabled:       !agentDisabledInConfig(cfg, name, proc),
			ReplicaBase:   agentCfg.ReplicaOf,
			ReplicaIndex:  agentCfg.ReplicaIndex,
			ReplicaCount:  agentCfg.ReplicaCount,
			OnDemand:      agentCfg.OnDemand,
			Sandboxed:     agentCfg.SandboxEnabled(cfg.AgentSandbox),
			Session:       name,
			State:         string(proc.State),
			Busy:          busy,
			Paused:        proc.Paused,
			PausedAt:      formatOptionalTime(proc.PausedAt),
			PausedReason:  proc.PausedReason,
			PausedTrigger: proc.PausedTrigger,
			PausedBy:      proc.PausedBy,
			OffByCadence:  offByCadence,
			CLI:           cli,
			Model:         model,
			Cadence:       cadence,
			PinnedCli:     pinnedCli,
			PinnedModel:   pinnedModel,
			PinnedBoth:    pinnedCli && pinnedModel,
			Pinned:        pinnedCli || pinnedModel,
			LastKick:      lastKick,
			NextKick:      nextKick,
			Restarts:      proc.RestartCount,
			GovBackend:    cli,
			GovModel:      model,
			GovCostWeight: 0,
			LiveSummary:   liveSummary,
			DetailSummary: detailSummary,
			StatsConfig:   resolveStatsSources(loadStatsConfig(name), cfg),
			LastError:     proc.LastError,
			StallNudges:   proc.StallNudges,
			ActionNudges:  proc.ActionNudges,
			// #4697: transient-API-error retry nudges, surfaced beside the
			// other two nudge counters.
			TransientNudges: proc.TransientNudges,
			Conditions:      proc.WatchdogConditions,
			WatchdogMode:    watchdogMode,
		}

		acmmLevel := 0
		if cfg.ACMMLevel != nil {
			acmmLevel = *cfg.ACMMLevel
		}
		mode := agent.DefaultAgentMode(name, acmmLevel)
		if modeStr := agentCfg.Mode; modeStr != "" {
			if parsed, ok := agent.ParseAgentMode(modeStr); ok {
				mode = parsed
			}
		}
		defaultMode := agent.DefaultAgentMode(name, acmmLevel)
		a.Mode = mode.String()
		a.ModeEmoji = mode.Emoji()
		a.DefaultMode = defaultMode.String()
		a.IsCustomMode = mode != defaultMode
		a.NeedsLogin = proc.NeedsLogin
		// Per-agent probe FIRST: it resolves this agent's own per-UID HOME and
		// answers "does this backend even have an interactive login?" before
		// looking at any file. The backend-level probe (shared legacy path
		// only) is the fallback for deployments where the per-agent provider
		// was not registered.
		if agentFn := getAgentAuthFn(); agentFn != nil {
			avail, known := agentFn(name)
			a.AuthAvailable = avail
			a.AuthKnown = known
		} else if authFn := getBackendAuthFn(); authFn != nil {
			avail, known := authFn(cli)
			a.AuthAvailable = avail
			a.AuthKnown = known
		}
		a.NeedsRestart = proc.HasLaunched && proc.LaunchedMode != mode
		a.OnDemand = agentCfg.OnDemand || onDemandSet[name]
		if pvFn := getProxyViolationsFn(); pvFn != nil {
			a.ProxyViolations = pvFn()[name]
		}

		agents = append(agents, a)
	}
	return agents
}

// loadStatsConfig reads the per-agent stats configuration from /data/agents/{name}/stats.json.
// Falls back to config StatsDisplay, then built-in defaults when the file is missing or empty.
func loadStatsConfig(name string) []any {
	statsFile := fmt.Sprintf("/data/agents/%s/stats.json", name)
	data, err := os.ReadFile(statsFile)
	if err == nil {
		var wrapper struct {
			Stats []any `json:"stats"`
		}
		if json.Unmarshal(data, &wrapper) == nil && len(wrapper.Stats) > 0 {
			return wrapper.Stats
		}
		var stats []any
		if json.Unmarshal(data, &stats) == nil && len(stats) > 0 {
			return stats
		}
	}
	return defaultStatsConfig(name)
}

// statsResolverSources are the stat `source` values that are fulfilled by the
// variable-resolution engine (pkg/resolve) rather than by a client-side data
// blob. "resolver" reads the stat's `field` as a ${VAR} reference / variable
// name; env/static/script/http name a resolver type directly and read `field`
// as the variable name. Everything else (agentMetrics, health, tokens, status)
// stays a dashboard-native, client-resolved source and is passed through
// untouched — this keeps existing stats byte-for-byte the same.
var statsResolverSources = map[string]bool{
	"resolver": true, "env": true, "static": true, "script": true, "http": true,
}

// resolveStatsSources fills in a server-computed `value` for any stat whose
// source is resolver-backed, using the config's variable-resolution registry.
// A resolver-backed stat carries its variable name in `field`; the resolved
// string is attached as `value`, which the dashboard renders directly (its
// resolveStatValue prefers a pre-resolved value). Stats with a dashboard-native
// source are returned unchanged. Unresolvable resolver stats get no value and
// render blank, exactly like any other missing metric — never an error.
func resolveStatsSources(stats []any, cfg *config.Config) []any {
	if cfg == nil || len(stats) == 0 {
		return stats
	}
	var reg *resolve.Registry
	for _, raw := range stats {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		src, _ := m["source"].(string)
		if !statsResolverSources[src] {
			continue
		}
		field, _ := m["field"].(string)
		if field == "" {
			continue
		}
		if reg == nil {
			reg = cfg.ResolveRegistry(nil)
		}
		// Resolve ${field} in template scope, exposing no runtime built-ins —
		// a stat value comes from operator-defined variables, not kick built-ins.
		val := reg.Expand(context.Background(), "${"+field+"}", resolve.ScopeTemplate, nil)
		// Leave value unset if it did not resolve (still the literal ${field}).
		if val != "${"+field+"}" {
			m["value"] = val
		}
	}
	return stats
}

// LoadStatsConfigWithCfg reads stats from disk, then falls back to config StatsDisplay field.
func LoadStatsConfigWithCfg(name string, cfg *config.Config) []any {
	statsFile := fmt.Sprintf("/data/agents/%s/stats.json", name)
	data, err := os.ReadFile(statsFile)
	if err == nil {
		var wrapper struct {
			Stats []any `json:"stats"`
		}
		if json.Unmarshal(data, &wrapper) == nil && len(wrapper.Stats) > 0 {
			return wrapper.Stats
		}
		var stats []any
		if json.Unmarshal(data, &stats) == nil && len(stats) > 0 {
			return stats
		}
	}
	if agentCfg, ok := cfg.Agents[name]; ok && len(agentCfg.StatsDisplay) > 0 {
		result := make([]any, 0, len(agentCfg.StatsDisplay))
		for _, s := range agentCfg.StatsDisplay {
			entry := map[string]any{
				"key": s.Key, "label": s.Label,
				"source": s.Source, "field": s.Field, "style": s.Style,
			}
			if s.TrendField != "" {
				entry["trendField"] = s.TrendField
			}
			if s.Target > 0 {
				entry["target"] = s.Target
			}
			if s.Desc != "" {
				entry["desc"] = s.Desc
			}
			result = append(result, entry)
		}
		return result
	}
	return defaultStatsConfig(name)
}

func defaultStatsConfig(name string) []any {
	defaults := map[string][]any{
		"scanner": {
			map[string]any{"key": "actionable", "label": "Actionable", "source": "status", "field": "actionableCount", "style": "spark", "trendField": "actionable"},
			map[string]any{"key": "openPrs", "label": "Open PRs", "source": "status", "field": "openPrCount", "style": "spark", "trendField": "openPrs"},
			map[string]any{"key": "mergeable", "label": "Mergeable", "source": "status", "field": "mergeableCount", "style": "spark", "trendField": "mergeable"},
		},
		"ci-maintainer": {
			// desc strings explain what each health check verifies (see
			// pkg/github/health.go); the dashboard renders them as hover
			// tooltips on the HEALTH CHECKS rows and agent stat chips.
			map[string]any{"key": "coverage", "label": "Coverage", "source": "agentMetrics", "field": "coverage", "style": "pct-bar", "target": 91},
			map[string]any{"key": "brew", "label": "Brew", "source": "health", "field": "brew", "style": "dot", "desc": "Homebrew tap formula version matches a recent console release tag (stable or nightly, last 20 releases)"},
			map[string]any{"key": "helm", "label": "Helm", "source": "health", "field": "helm", "style": "dot", "desc": "Helm chart exists at deploy/helm/kubestellar-console/Chart.yaml in the primary repo"},
			map[string]any{"key": "ci", "label": "CI", "source": "health", "field": "ci", "style": "pct", "desc": "Pass rate over the last 10 completed workflow runs on the primary repo (success or skipped count as passing)"},
			map[string]any{"key": "weekly", "label": "Weekly", "source": "health", "field": "weekly", "style": "dot", "desc": "Most recent 'Weekly Coverage Review' workflow run did not fail"},
			map[string]any{"key": "nightly", "label": "Nightly Tests", "source": "health", "field": "nightly", "style": "dot", "desc": "Most recent 'Nightly Test Suite' workflow run did not fail"},
			map[string]any{"key": "nightlyCompliance", "label": "Compliance", "source": "health", "field": "nightlyCompliance", "style": "dot", "desc": "Most recent 'Nightly Compliance & Perf' workflow run did not fail"},
			map[string]any{"key": "nightlyDashboard", "label": "Dashboard", "source": "health", "field": "nightlyDashboard", "style": "dot", "desc": "Most recent 'Nightly Dashboard Health' workflow run did not fail"},
			map[string]any{"key": "nightlyGhaw", "label": "gh-aw", "source": "health", "field": "nightlyGhaw", "style": "dot", "desc": "Most recent 'Nightly gh-aw Version Check' workflow run did not fail"},
			map[string]any{"key": "nightlyPlaywright", "label": "Playwright", "source": "health", "field": "nightlyPlaywright", "style": "dot", "desc": "Most recent 'Playwright Cross-Browser (Nightly)' workflow run did not fail"},
			map[string]any{"key": "nightlyRel", "label": "Nightly Rel", "source": "health", "field": "nightlyRel", "style": "dot", "desc": "Most recent scheduled weekday 'Release' workflow run succeeded (nightly release; Sundays belong to the weekly release)"},
			map[string]any{"key": "weeklyRel", "label": "Weekly Rel", "source": "health", "field": "weeklyRel", "style": "dot", "desc": "Most recent scheduled Sunday 'Release' workflow run succeeded (weekly release)"},
			map[string]any{"key": "deploy_vllm_d", "label": "vLLM-d", "source": "health", "field": "deploy_vllm_d", "style": "dot", "desc": "deploy-vllm-d job of the latest completed 'Build and Deploy KC' run on main did not fail"},
			map[string]any{"key": "deploy_pok_prod", "label": "PokProd", "source": "health", "field": "deploy_pok_prod", "style": "dot", "desc": "deploy-pok-prod job of the latest completed 'Build and Deploy KC' run on main did not fail"},
		},
		"outreach": {
			map[string]any{"key": "stars", "label": "Stars", "source": "agentMetrics", "field": "stars", "style": "spark", "trendField": "stars"},
			map[string]any{"key": "forks", "label": "Forks", "source": "agentMetrics", "field": "forks", "style": "number"},
			map[string]any{"key": "contributors", "label": "Contributors", "source": "agentMetrics", "field": "contributors", "style": "number"},
			map[string]any{"key": "adopters", "label": "Adopters", "source": "agentMetrics", "field": "adopters", "style": "number"},
			map[string]any{"key": "acmm", "label": "ACMM", "source": "agentMetrics", "field": "acmm", "style": "number"},
			map[string]any{"key": "outreachOpen", "label": "Open PRs", "source": "agentMetrics", "field": "outreachOpen", "style": "spark", "trendField": "outreachOpen"},
			map[string]any{"key": "outreachMerged", "label": "Merged PRs", "source": "agentMetrics", "field": "outreachMerged", "style": "spark", "trendField": "outreachMerged"},
		},
		"architect": {
			map[string]any{"key": "prs", "label": "PRs", "source": "agentMetrics", "field": "prs", "style": "number"},
			map[string]any{"key": "closed", "label": "Closed", "source": "agentMetrics", "field": "closed", "style": "number"},
		},
	}
	if d, ok := defaults[name]; ok {
		return d
	}
	return []any{}
}

// CollectAgentStats resolves current stat values for all agents, keyed by agent name then stat key.
func CollectRepoSnapshots(payload *StatusPayload) map[string]governor.RepoSnapshot {
	if payload == nil || len(payload.Repos) == 0 {
		return nil
	}
	result := make(map[string]governor.RepoSnapshot, len(payload.Repos))
	for _, r := range payload.Repos {
		result[r.Name] = governor.RepoSnapshot{
			Issues: r.Issues,
			PRs:    r.PRs,
		}
	}
	return result
}

func CollectAgentStats(payload *StatusPayload) map[string]map[string]any {
	result := make(map[string]map[string]any)
	for _, a := range payload.Agents {
		statsConfig := a.StatsConfig
		if len(statsConfig) == 0 {
			continue
		}
		vals := make(map[string]any)
		for _, raw := range statsConfig {
			stat, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			key, _ := stat["key"].(string)
			if key == "" {
				continue
			}
			source, _ := stat["source"].(string)
			field, _ := stat["field"].(string)
			var v any
			switch source {
			case "status":
				switch field {
				case "actionableCount":
					total := 0
					for _, r := range payload.Repos {
						total += len(r.ActionableIssues)
					}
					v = total
				case "openPrCount":
					total := 0
					for _, r := range payload.Repos {
						total += len(r.OpenPrs)
					}
					v = total
				case "mergeableCount":
					total := 0
					for _, r := range payload.Repos {
						for _, pr := range r.OpenPrs {
							if m, ok := pr.(map[string]any); ok {
								if mb, ok := m["mergeable"].(bool); ok && mb {
									total++
								}
							}
						}
					}
					v = total
				}
			case "health":
				if payload.Health != nil {
					v = payload.Health[field]
				}
			case "agentMetrics":
				if am, ok := payload.AgentMetrics[a.Name]; ok {
					if m, ok := am.(map[string]any); ok {
						v = m[field]
					}
				}
			case "tokens":
				if bucket, ok := payload.Tokens.ByAgent[a.Name]; ok {
					switch field {
					case "input":
						v = bucket.Input
					case "output":
						v = bucket.Output
					case "cacheRead":
						v = bucket.CacheRead
					case "cacheCreate":
						v = bucket.CacheCreate
					case "sessions":
						v = bucket.Sessions
					case "messages":
						v = bucket.Messages
					}
				}
			}
			if v != nil {
				vals[key] = v
			}
		}
		if len(vals) > 0 {
			result[a.Name] = vals
		}
	}
	return result
}

func formatHumanTime(t time.Time) string {
	local := t.Local()
	return local.Format("1/2 3:04 PM MST")
}

func computeNextKick(lastKick *time.Time, cadence string) string {
	if cadence == "" || cadence == cadenceOff || cadence == cadencePause || cadence == cadenceOnDemand {
		return ""
	}
	base := time.Now()
	if lastKick != nil {
		base = *lastKick
	}
	d := parseCadenceDuration(cadence)
	if d == 0 {
		return ""
	}
	next := base.Add(d)
	return formatHumanTime(next)
}

func computeNextKickFromCadence(lastKick *time.Time, cadence config.Cadence) string {
	if cadence == "" || cadence.IsPaused() {
		return ""
	}
	base := time.Now()
	if lastKick != nil && cadence.Mode() == config.CadenceModeInterval {
		base = *lastKick
	}
	next, ok := cadence.NextAfter(base)
	if !ok {
		return ""
	}
	return formatHumanTime(next)
}

func parseCadenceDuration(cadence string) time.Duration {
	cadence = strings.TrimSpace(cadence)
	if cadence == "" || cadence == cadenceOff || cadence == cadencePause || cadence == cadenceOnDemand {
		return 0
	}
	d, err := time.ParseDuration(cadence)
	if err == nil {
		return d
	}
	// Handle shorthand like "15m", "1h", "2m" — already valid for ParseDuration
	// Handle "5min" style
	cadence = strings.Replace(cadence, "min", "m", 1)
	d, err = time.ParseDuration(cadence)
	if err == nil {
		return d
	}
	return 0
}

func formatCadenceDuration(seconds int64) string {
	const secondsPerHour = 3600
	const secondsPerMinute = 60
	if seconds%secondsPerHour == 0 {
		return fmt.Sprintf("%dh", seconds/secondsPerHour)
	}
	if seconds%secondsPerMinute == 0 {
		return fmt.Sprintf("%dm", seconds/secondsPerMinute)
	}
	return fmt.Sprintf("%ds", seconds)
}

func lookupCadence(agentName string, cfg *config.Config) string {
	return lookupCadenceForMode(agentName, "idle", cfg)
}

func lookupCadenceForMode(agentName, modeName string, cfg *config.Config) string {
	return cadenceDisplay(lookupCadenceValueForMode(agentName, modeName, cfg))
}

func lookupCadenceValue(agentName string, cfg *config.Config) config.Cadence {
	return lookupCadenceValueForMode(agentName, "idle", cfg)
}

func lookupCadenceValueForMode(agentName, modeName string, cfg *config.Config) config.Cadence {
	if mode, ok := cfg.Governor.Modes[modeName]; ok {
		if c, ok := mode.Cadences[agentName]; ok {
			return c
		}
		baseName := cfg.BaseAgentName(agentName)
		if baseName != agentName {
			if c, ok := mode.Cadences[baseName]; ok {
				return c
			}
		}
	}
	return ""
}

func cadenceDisplay(c config.Cadence) string {
	if c == "" {
		return ""
	}
	if c.Mode() == config.CadenceModeInterval {
		return c.Interval()
	}
	return c.ShortLabel(time.Now())
}

func buildGovernor(state governor.State, cfg *config.Config) FrontendGovernor {
	// The gauge must show the thresholds the governor ACTUALLY laddered on, or
	// it explains the current mode with numbers that did not produce it. This
	// used to be a private copy of the defaults plus its own explicit-wins
	// check; both now come from config.EffectiveThreshold, which is the same
	// call pkg/governor makes. Scaling by repo count (#3498) is what made the
	// duplication actively wrong rather than merely redundant.
	repos := cfg.Project.RepoCount()
	thresholds := FrontendThresholds{
		Quiet: cfg.Governor.EffectiveThreshold("quiet", repos),
		Busy:  cfg.Governor.EffectiveThreshold("busy", repos),
		Surge: cfg.Governor.EffectiveThreshold("surge", repos),
	}

	nextKick := ""
	if cfg.Governor.EvalIntervalS > 0 {
		next := time.Now().Add(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
		nextKick = formatHumanTime(next)
	}

	return FrontendGovernor{
		Active:     true,
		Mode:       strings.ToLower(string(state.Mode)),
		Issues:     state.QueueIssues,
		PRs:        state.QueuePRs,
		Thresholds: thresholds,
		NextKick:   nextKick,
	}
}

func buildTokens(collector *tokens.Collector) FrontendTokens {
	ft := FrontendTokens{
		LookbackHours: defaultLookbackHours,
		Totals:        FrontendTokenTotals{},
		Sessions:      []FrontendSession{},
		ByAgent:       make(map[string]FrontendTokenBucket),
		ByModel:       make(map[string]FrontendTokenBucket),
		ByAgentModel:  make(map[string]string),
	}

	if collector == nil {
		return ft
	}

	summary := collector.Summary()
	if summary == nil {
		return ft
	}

	ft.Totals.Sessions = summary.SessionCount
	ft.Totals.Input = summary.TotalInput
	ft.Totals.Output = summary.TotalOutput
	ft.Totals.CacheRead = summary.TotalCacheRead
	ft.Totals.CacheCreate = summary.TotalCacheCreate
	ft.Totals.Messages = summary.TotalMessages

	// Per-agent breakdown with full detail
	for agentName, detail := range summary.ByAgentDetail {
		bucket := FrontendTokenBucket{
			Input:       detail.Input,
			Output:      detail.Output,
			CacheRead:   detail.CacheRead,
			CacheCreate: detail.CacheCreate,
			Messages:    detail.Messages,
			Sessions:    detail.Sessions,
		}
		if detail.Sessions > 0 {
			totalForAgent := detail.Input + detail.Output + detail.CacheRead + detail.CacheCreate
			bucket.AvgPerSession = totalForAgent / int64(detail.Sessions)
		}
		ft.ByAgent[agentName] = bucket
	}

	// Per-model breakdown with full detail
	for modelName, detail := range summary.ByModelDetail {
		bucket := FrontendTokenBucket{
			Input:       detail.Input,
			Output:      detail.Output,
			CacheRead:   detail.CacheRead,
			CacheCreate: detail.CacheCreate,
			Messages:    detail.Messages,
			Sessions:    detail.Sessions,
		}
		if detail.Sessions > 0 {
			totalForModel := detail.Input + detail.Output + detail.CacheRead + detail.CacheCreate
			bucket.AvgPerSession = totalForModel / int64(detail.Sessions)
		}
		ft.ByModel[modelName] = bucket
	}

	// Per-agent RESOLVED model: the model of each agent's most recently active
	// session. Config models that are routing aliases ("auto"/"default") only
	// name a concrete model at runtime, and that resolved id is what the session
	// logs record. Exposing it lets the frontend attribute an auto-mode agent's
	// live badge to the row where its tokens actually land.
	ft.ByAgentModel = resolveAgentModels(summary.Sessions)

	// Individual sessions are omitted from the status payload to reduce size
	// (~70% savings). Use GET /api/tokens for the full session list.

	return ft
}

// aliasModels are model ids that are routing aliases, not concrete models. A
// session logged under one of these carries no resolved-model information, so
// it must not be used as an agent's resolved model.
var aliasModels = map[string]bool{
	"":        true,
	"unknown": true,
	"auto":    true,
	"default": true,
}

// resolveAgentModels maps each agent to the concrete model of its most recently
// active session. Sessions whose Model is empty/"unknown"/an alias ("auto",
// "default") are skipped, since they carry no resolved-model signal. When two
// qualifying sessions exist for an agent, the one with the greater LastActive
// wins (ties keep the first seen). Agents with no qualifying session are absent
// from the result.
func resolveAgentModels(sessions []tokens.SessionSummary) map[string]string {
	type pick struct {
		model      string
		lastActive int64
	}
	best := make(map[string]pick)
	for _, s := range sessions {
		if s.Agent == "" {
			continue
		}
		if aliasModels[strings.ToLower(strings.TrimSpace(s.Model))] {
			continue
		}
		if cur, ok := best[s.Agent]; !ok || s.LastActive > cur.lastActive {
			best[s.Agent] = pick{model: s.Model, lastActive: s.LastActive}
		}
	}
	out := make(map[string]string, len(best))
	for agent, p := range best {
		out[agent] = p.model
	}
	return out
}

func buildRepos(cfg *config.Config, actionable *github.ActionableResult) []FrontendRepo {
	repos := make([]FrontendRepo, 0, len(cfg.Project.Repos))

	issuesByRepo := make(map[string][]any)
	prsByRepo := make(map[string][]any)

	if actionable != nil {
		for _, issue := range actionable.Issues.Items {
			issuesByRepo[issue.Repo] = append(issuesByRepo[issue.Repo], issue)
		}
		for _, pr := range actionable.PRs.Items {
			prsByRepo[pr.Repo] = append(prsByRepo[pr.Repo], pr)
		}
	}

	for _, repoName := range cfg.Project.Repos {
		// A repo entry may be a bare name ("cuga-agent") or an explicit
		// cross-org reference ("laredo/cuga-agent") — both are supported config.
		// Prefixing the org unconditionally turned the latter into
		// "laredo/laredo/cuga-agent" in the REPOSITORIES card and in every
		// GitHub link built from it.
		full := repoName
		if !strings.Contains(repoName, "/") {
			full = cfg.Project.Org + "/" + repoName
		}

		issueCount := 0
		prCount := 0
		if actionable != nil && actionable.TotalByRepo != nil {
			if counts, ok := actionable.TotalByRepo[repoName]; ok {
				issueCount = counts.Issues
				prCount = counts.PRs
			}
		}

		r := FrontendRepo{
			Name:             repoName,
			Full:             full,
			Issues:           issueCount,
			PRs:              prCount,
			ActionableIssues: issuesByRepo[repoName],
			OpenPrs:          prsByRepo[repoName],
		}
		if r.ActionableIssues == nil {
			r.ActionableIssues = []any{}
		}
		if r.OpenPrs == nil {
			r.OpenPrs = []any{}
		}
		repos = append(repos, r)
	}

	return repos
}

func buildBeads(stores map[string]*beads.Store) FrontendBeads {
	fb := FrontendBeads{}
	for name, store := range stores {
		count := store.Count()
		if name == "supervisor" {
			fb.Supervisor = count
		} else {
			fb.Workers += count
		}
	}
	return fb
}

// BuildPlanning computes the governor PLANNING metric block from bead metadata
// across all stores (Phase 2 planning intelligence). An epic is "active" once it
// carries a plan_status; "awaiting review" while that status is draft; and
// "decomposing" (executing) when approved with at least one open child. Replans24h
// (Phase 3) counts epics whose last_replan_at metadata falls within the last 24h,
// so the tile reflects real governor stall-replans purely from bead state.
// PendingDecompose (Phase 4) counts issue-sourced epics not yet built by the
// architect; architectPaused says whether that queue is blocked on a paused
// architect (so the tile can show the "resume the architect" message).
func BuildPlanning(stores map[string]*beads.Store, architectPaused bool, acmmLevel int) FrontendPlanning {
	return buildPlanningAt(stores, architectPaused, acmmLevel, time.Now())
}

// buildPlanningAt is BuildPlanning with an injected `now`, so the replans_24h
// window is unit-testable with backdated last_replan_at timestamps.
func buildPlanningAt(stores map[string]*beads.Store, architectPaused bool, acmmLevel int, now time.Time) FrontendPlanning {
	fp := FrontendPlanning{Available: planning.PlanningAllowedAtLevel(acmmLevel)}
	cutoff := now.Add(-planningReplanWindow)
	for _, store := range stores {
		all := store.List(beads.ListFilter{})

		// Count open children per epic so we can tell executing plans apart from
		// fully-completed ones.
		openChildrenByEpic := make(map[string]int)
		for _, b := range all {
			if epicID := b.Meta(planning.MetaParentEpic); epicID != "" {
				if b.Status == beads.StatusOpen || b.Status == beads.StatusInProgress {
					openChildrenByEpic[epicID]++
				}
			}
		}

		for _, b := range all {
			if b.Type != beads.TypeEpic {
				continue
			}
			status := b.Meta(planning.MetaPlanStatus)
			if status == "" {
				continue // never decomposed
			}
			fp.ActivePlans++
			switch status {
			case planning.PlanStatusDraft:
				fp.AwaitingReview++
			case planning.PlanStatusApproved:
				if openChildrenByEpic[b.ID] > 0 {
					fp.Decomposing++
				}
			}
			// Phase 4: an issue-sourced epic still marked decompose_pending is
			// queued for the architect (children not yet materialized).
			if planning.DecomposePending(b) {
				fp.PendingDecompose++
			}
			if raw := b.Meta(planning.MetaLastReplanAt); raw != "" {
				if t, err := time.Parse(time.RFC3339, raw); err == nil && t.After(cutoff) {
					fp.Replans24h++
				}
			}
		}
	}
	// Only flag the paused-architect condition when it actually matters: there is
	// queued work AND the architect is paused. Otherwise the tile stays quiet.
	fp.ArchitectPaused = fp.PendingDecompose > 0 && architectPaused
	return fp
}

// planningReplanWindow is the trailing window over which replans_24h counts
// re-decomposed plans (matches the tile label "24h").
const planningReplanWindow = 24 * time.Hour

// architectPausedFromStatuses reads the architect's paused state from the
// already-collected agent statuses, so the PLANNING tile can flag a queue
// blocked on a paused architect WITHOUT taking the agent-manager lock.
func architectPausedFromStatuses(statuses map[string]*agent.AgentProcess) bool {
	if ap, ok := statuses[planning.ArchitectAgentName]; ok && ap != nil {
		return ap.Paused
	}
	return false
}

// BuildBeadsFromConfig uses agent config bead_role to partition bead counts.
func BuildBeadsFromConfig(stores map[string]*beads.Store, cfg *config.Config) FrontendBeads {
	fb := FrontendBeads{}
	for name, store := range stores {
		count := store.Count()
		role := "worker"
		if agentCfg, ok := cfg.Agents[name]; ok {
			role = agentCfg.GetBeadRole()
		}
		if role == "supervisor" {
			fb.Supervisor = count
		} else {
			fb.Workers += count
		}
	}
	return fb
}

func copyHealthMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func buildHealth(ghClient *github.Client, ctx context.Context) map[string]any {
	if ghClient == nil || ctx == nil {
		cachedHealthMu.RLock()
		defer cachedHealthMu.RUnlock()
		if cachedHealth != nil {
			return copyHealthMap(cachedHealth)
		}
		return map[string]any{"ci": 100}
	}

	health := ghClient.FetchWorkflowHealth(ctx)

	cachedHealthMu.Lock()
	cachedHealth = health
	cachedHealthMu.Unlock()

	return copyHealthMap(health)
}

func buildBudget(gov *governor.Governor, tokenCollector *tokens.Collector) FrontendBudget {
	budget := gov.GetBudget()
	windowStart, windowEnd, hasWindow := gov.BudgetWindow()
	now := time.Now()

	var totalTokens int64
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			totalTokens = summary.TotalTokens
		}
	}

	// Compute elapsed/remaining wall-clock time from the governor's effective
	// budget period. BudgetWindow is the single source of truth for configured
	// period_days; using the default duration here makes a two-day budget look
	// like a seven-day one in the dashboard.
	var hoursElapsed float64
	var windowHoursRemaining float64
	if hasWindow {
		hoursElapsed = now.Sub(windowStart).Hours()
		if hoursElapsed < 0 {
			hoursElapsed = 0
		}
		windowHoursRemaining = windowEnd.Sub(now).Hours()
		if windowHoursRemaining < 0 {
			windowHoursRemaining = 0
		}
	}

	used := totalTokens
	if !budget.ResetAt.IsZero() {
		// Governor window is open: CurrentSpend is the live window-relative
		// spend. The collector lifetime total remains the fallback for
		// limit-less tracking before any window exists.
		used = budget.CurrentSpend
	}

	fb := FrontendBudget{
		WeeklyBudget:         budget.WeeklyLimit,
		Used:                 used,
		LastUpdated:          now.UTC().Format(time.RFC3339),
		WindowHoursRemaining: windowHoursRemaining,
	}

	if budget.WeeklyLimit > 0 {
		const pctMultiplier = 100.0
		remaining := budget.WeeklyLimit - used
		if remaining < 0 {
			remaining = 0
		}
		fb.Remaining = remaining
		fb.PctUsed = float64(used) / float64(budget.WeeklyLimit) * pctMultiplier

		if hoursElapsed > 0 {
			burnRate := float64(used) / hoursElapsed
			fb.BurnRateHourly = burnRate
			fb.BurnRateInstant = burnRate
			periodHours := governor.BudgetWindowDuration.Hours()
			if hasWindow {
				periodHours = windowEnd.Sub(windowStart).Hours()
			}
			fb.ProjectedWeekly = int64(burnRate * periodHours)
			fb.ProjectedPct = float64(fb.ProjectedWeekly) / float64(budget.WeeklyLimit) * pctMultiplier
			if burnRate > 0 {
				fb.HoursRemaining = float64(remaining) / burnRate
			}
		}
		fb.HoursElapsed = hoursElapsed

		fb.Exhausted = used >= budget.WeeklyLimit
		if hasWindow {
			fb.WindowEndsAt = windowEnd.UTC().Format(time.RFC3339)
			fb.WindowStartsAt = windowStart.UTC().Format(time.RFC3339)
		}
	}

	return fb
}

func buildCadenceMatrix(cfg *config.Config, agentStatuses map[string]*agent.AgentProcess) []FrontendCadence {
	if cfg == nil {
		return nil
	}
	acmmLevel := 0
	if cfg.ACMMLevel != nil {
		acmmLevel = *cfg.ACMMLevel
	}
	sortedNames := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		if !agent.AgentAvailableAtACMMLevel(name, acmmLevel) {
			continue
		}
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	matrix := make([]FrontendCadence, 0, len(sortedNames))
	for _, name := range sortedNames {
		entry := FrontendCadence{Agent: name}

		paused := false
		if proc, ok := agentStatuses[name]; ok && proc.Paused {
			paused = true
		}

		onDemand := false
		if agentCfg, ok := cfg.Agents[name]; ok && agentCfg.OnDemand {
			onDemand = true
		}

		for modeName, mode := range cfg.Governor.Modes {
			rawCadence := mode.Cadences[name]
			cadence := cadenceDisplay(rawCadence)
			title := cadenceTooltip(rawCadence)
			if cadence == "" || cadence == "pause" {
				cadence = "off"
			}
			if onDemand {
				cadence = "on demand"
			} else if paused && cadence != "off" {
				cadence = "paused"
			}
			switch modeName {
			case "idle":
				entry.Idle = cadence
				entry.IdleTitle = title
			case "quiet":
				entry.Quiet = cadence
				entry.QuietTitle = title
			case "busy":
				entry.Busy = cadence
				entry.BusyTitle = title
			case "surge":
				entry.Surge = cadence
				entry.SurgeTitle = title
			}
		}
		matrix = append(matrix, entry)
	}

	return matrix
}

func cadenceTooltip(c config.Cadence) string {
	if c == "" || c.IsPaused() {
		return ""
	}
	next, ok := c.NextAfter(time.Now())
	if !ok {
		return c.HumanSummary()
	}
	return c.HumanSummary() + " — next: " + formatHumanTime(next)
}

func buildHold(actionable *github.ActionableResult) FrontendHold {
	if actionable == nil {
		return FrontendHold{Items: []any{}}
	}
	items := make([]any, 0, len(actionable.Hold.Items))
	var holdIssues, holdPRs int
	for _, item := range actionable.Hold.Items {
		items = append(items, item)
		if item.Type == "pr" {
			holdPRs++
		} else {
			holdIssues++
		}
	}
	return FrontendHold{
		Issues: holdIssues,
		PRs:    holdPRs,
		Total:  actionable.Hold.Total,
		Items:  items,
	}
}

func buildGHRateLimits(ghClient *github.Client, ctx context.Context, cfg *config.Config) map[string]any {
	result := map[string]any{
		"core":      map[string]any{},
		"alerts":    []any{},
		"pullbacks": []any{},
	}

	authType := "token"
	authLabel := "unknown"
	// HasApp() so a placeholder app_id is not reported as App-authenticated
	// identity in the rate-limit card — it has no installation and mints no
	// tokens.
	if cfg.GitHub.HasApp() {
		authType = "app"
		authLabel = cfg.Project.Org
	} else if cfg.GitHub.Token != "" {
		authType = "token"
		authLabel = "personal"
	}
	result["identity"] = map[string]any{
		"type":  authType,
		"label": authLabel,
	}

	if ghClient != nil && ctx != nil {
		limits, err := ghClient.RateLimits(ctx)
		if err == nil && limits != nil {
			result["core"] = map[string]any{
				"limit":     limits.Core.Limit,
				"remaining": limits.Core.Remaining,
				"reset":     limits.Core.Reset.Format(time.RFC3339),
			}
		}
	}

	return result
}

// buildIssueToMerge converts the MTTR result from the metrics collector into
// the map[string]any format that the dashboard frontend expects.
func buildIssueToMerge(mc *MetricsCollector) map[string]any {
	if mc == nil {
		return map[string]any{}
	}
	mttr := mc.GetMTTR()
	if mttr == nil || mttr.Count == 0 {
		return map[string]any{}
	}

	history := make([]map[string]any, 0, len(mttr.History))
	for _, h := range mttr.History {
		history = append(history, map[string]any{
			"t":      h.T,
			"avg":    h.Avg,
			"median": h.Median,
		})
	}

	return map[string]any{
		"avg_minutes":     mttr.AvgMinutes,
		"median_minutes":  mttr.MedianMinutes,
		"p90_minutes":     mttr.P90Minutes,
		"count":           mttr.Count,
		"fastest_minutes": mttr.FastestMinutes,
		"slowest_minutes": mttr.SlowestMinutes,
		"updated_at":      mttr.UpdatedAt,
		"history":         history,
	}
}

func acmmPackAllowedSet(cfg *config.Config) map[string]bool {
	if cfg.ACMMLevel == nil {
		return nil
	}
	level := *cfg.ACMMLevel
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return nil
	}
	allowed := make(map[string]bool, len(pack.Agents))
	for _, a := range pack.Agents {
		if !a.Hidden {
			allowed[a.Name] = true
		}
	}
	return allowed
}

func buildACMMPackAgents(cfg *config.Config) []string {
	level := detectACMMLevel(cfg)
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(pack.Agents))
	for _, a := range pack.Agents {
		if !a.Hidden {
			names = append(names, a.Name)
		}
	}
	return names
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// SystemResources contains disk and memory usage metrics for the host system.
type SystemResources struct {
	DiskUsedGB  float64 `json:"diskUsedGB"`
	DiskTotalGB float64 `json:"diskTotalGB"`
	DiskPct     float64 `json:"diskPct"`
	MemUsedMB   float64 `json:"memUsedMB"`
	MemTotalMB  float64 `json:"memTotalMB"`
	MemPct      float64 `json:"memPct"`
	CpuPct      float64 `json:"cpuPct"`
	CpuCores    int     `json:"cpuCores"`
}

const (
	bytesPerGB          = 1024.0 * 1024.0 * 1024.0
	bytesPerMB          = 1024.0 * 1024.0
	microsecondsPerSec  = 1_000_000.0
	pctMultiplierSysRes = 100.0
	cpuSampleDelayMs    = 200

	// dataVolumePath is the mount point for the hive data volume.
	dataVolumePath = "/data"

	// cgroupMemCurrent is the cgroup v2 file for current memory usage.
	cgroupMemCurrent = "/sys/fs/cgroup/memory.current"

	// cgroupMemMax is the cgroup v2 file for memory limit.
	cgroupMemMax = "/sys/fs/cgroup/memory.max"

	// cgroupCPUStat is the cgroup v2 file for CPU accounting.
	cgroupCPUStat = "/sys/fs/cgroup/cpu.stat"

	// cgroupV1MemUsage is the cgroup v1 file for current memory usage.
	cgroupV1MemUsage = "/sys/fs/cgroup/memory/memory.usage_in_bytes"

	// cgroupV1MemLimit is the cgroup v1 file for memory limit.
	cgroupV1MemLimit = "/sys/fs/cgroup/memory/memory.limit_in_bytes"

	// cgroupV1CPUUsage is the cgroup v1 file for accumulated CPU time (nanoseconds).
	cgroupV1CPUUsage = "/sys/fs/cgroup/cpuacct/cpuacct.usage"

	// nanosPerMicro converts nanoseconds to microseconds.
	nanosPerMicro = 1000

	// maxCgroupMemBytes is the sentinel value cgroups uses to indicate "no limit".
	maxCgroupMemBytes = 1 << 62

	// cgroupV1UnlimitedMemBytes is the cgroup v1 sentinel for "no limit".
	cgroupV1UnlimitedMemBytes int64 = 9223372036854771712

	// maxReasonableDiskBytes caps disk reporting to filter out virtual filesystems (e.g., OCI NFS reporting 8 EiB).
	maxReasonableDiskBytes = 100 * 1024 * 1024 * 1024 * 1024

	// rootFSPath is the fallback mount point when /data reports unreasonable values.
	rootFSPath = "/"
)

// collectSystemResources gathers disk, memory, and CPU usage.
// Disk comes from syscall.Statfs on /data.
// Memory comes from cgroup v2 files.
// CPU comes from cgroup v2 cpu.stat (usage_usec sampled twice).
func collectSystemResources() *SystemResources {
	res := &SystemResources{}

	// --- Disk ---
	var stat syscall.Statfs_t
	diskPath := dataVolumePath
	if err := syscall.Statfs(diskPath, &stat); err == nil {
		totalBytes := stat.Blocks * uint64(stat.Bsize)
		// OCI NFS can report ~8 EiB; fall back to root filesystem.
		if totalBytes > maxReasonableDiskBytes {
			diskPath = rootFSPath
			_ = syscall.Statfs(diskPath, &stat)
			totalBytes = stat.Blocks * uint64(stat.Bsize)
		}
		freeBytes := stat.Bavail * uint64(stat.Bsize)
		usedBytes := totalBytes - freeBytes
		res.DiskTotalGB = roundTo(float64(totalBytes)/bytesPerGB, 1)
		res.DiskUsedGB = roundTo(float64(usedBytes)/bytesPerGB, 1)
		if totalBytes > 0 {
			res.DiskPct = roundTo(float64(usedBytes)/float64(totalBytes)*pctMultiplierSysRes, 1)
		}
	}

	// --- Memory (cgroup v2, with v1 fallback) ---
	memCurrent := readCgroupInt64(cgroupMemCurrent)
	if memCurrent < 0 {
		memCurrent = readCgroupInt64(cgroupV1MemUsage)
	}
	memMax := readCgroupInt64(cgroupMemMax)
	if memMax < 0 {
		memMax = readCgroupInt64(cgroupV1MemLimit)
	}
	if memMax <= 0 || memMax >= maxCgroupMemBytes || memMax == cgroupV1UnlimitedMemBytes {
		memMax = readProcMemTotalBytes()
	}
	if memCurrent > 0 && memMax > 0 {
		res.MemUsedMB = roundTo(float64(memCurrent)/bytesPerMB, 0)
		res.MemTotalMB = roundTo(float64(memMax)/bytesPerMB, 0)
		res.MemPct = roundTo(float64(memCurrent)/float64(memMax)*pctMultiplierSysRes, 1)
	}

	// --- CPU (cgroup v2, with v1 fallback, sampled) ---
	usec1 := readCgroupCPUUsageUsec(cgroupCPUStat)
	if usec1 < 0 {
		usec1 = readCgroupV1CPUUsageUsec(cgroupV1CPUUsage)
	}
	if usec1 >= 0 {
		time.Sleep(cpuSampleDelayMs * time.Millisecond)
		usec2 := readCgroupCPUUsageUsec(cgroupCPUStat)
		if usec2 < 0 {
			usec2 = readCgroupV1CPUUsageUsec(cgroupV1CPUUsage)
		}
		if usec2 > usec1 {
			deltaUsec := float64(usec2 - usec1)
			deltaSec := float64(cpuSampleDelayMs) / 1000.0
			numCPUs := float64(runtime.NumCPU())
			if numCPUs < 1 {
				numCPUs = 1
			}
			cpuFraction := deltaUsec / (deltaSec * microsecondsPerSec * numCPUs)
			res.CpuPct = roundTo(cpuFraction*pctMultiplierSysRes, 1)
			// Clamp: on a noisy/oversubscribed host (e.g. a shared CI
			// runner), scheduler bursts within the short cpuSampleDelayMs
			// window can push the measured delta a hair past the
			// theoretical 100% ceiling (observed: 100.1). CpuPct is a
			// display/reporting value, so clamp rather than let a
			// momentary sampling artifact read as over-100% or negative.
			if res.CpuPct > pctMultiplierSysRes {
				res.CpuPct = pctMultiplierSysRes
			} else if res.CpuPct < 0 {
				res.CpuPct = 0
			}
		}
	}

	res.CpuCores = runtime.NumCPU()

	return res
}

// readCgroupInt64 reads a single int64 from a cgroup file. Returns -1 on error
// or if the value is "max" (unlimited).
func readCgroupInt64(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return -1
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// procMeminfo is the path to the host memory info file.
const procMeminfo = "/proc/meminfo"

// kbToBytes converts kB (as reported by /proc/meminfo) to bytes.
const kbToBytes = 1024

// readProcMemTotalBytes reads MemTotal from /proc/meminfo and returns
// the value in bytes. Returns -1 on error.
func readProcMemTotalBytes() int64 {
	data, err := os.ReadFile(procMeminfo)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return kb * kbToBytes
				}
			}
		}
	}
	return -1
}

// readCgroupCPUUsageUsec parses usage_usec from a cgroup v2 cpu.stat file.
// Returns -1 if unavailable.
func readCgroupCPUUsageUsec(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "usage_usec ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				v, err := strconv.ParseInt(parts[1], 10, 64)
				if err == nil {
					return v
				}
			}
		}
	}
	return -1
}

// readCgroupV1CPUUsageUsec reads cgroup v1 cpuacct.usage (nanoseconds) and
// converts to microseconds for compatibility with the v2 sampling logic.
func readCgroupV1CPUUsageUsec(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	nanos, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return -1
	}
	return nanos / nanosPerMicro
}

// roundTo rounds f to the specified number of decimal places.
func roundTo(f float64, decimals int) float64 {
	shift := math.Pow10(decimals)
	return math.Round(f*shift) / shift
}

var statusTokenRedactor = regexp.MustCompile(`(ghp_|gho_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}`)
var deviceCodeRedactor = regexp.MustCompile(`(?i)(one-time code:\s*)[A-Z0-9]{4}-[A-Z0-9]{4}`)
var deviceCodeLineRedactor = regexp.MustCompile(`(?m)^.*(?:login/device|Waiting for authorization|one-time code:|Press any key to copy).*$`)

// apiKeyRedactor matches sk-prefixed API keys: real Anthropic keys (sk-ant-…),
// the per-agent LiteLLM virtual keys hive itself mints (sk-hive-<agent>, see
// Manager.agentEnv), and the generic OpenAI-style sk-… form.
//
// This surface previously redacted only GitHub tokens, so an agent command line
// carrying ANTHROPIC_API_KEY='sk-hive-…' reached the pane/full-log endpoints in
// clear text (observed in the #3852 report). That was survivable only by
// accident: an 80-column pane frequently clipped the key off the end of the
// line before it was ever captured. Widening the pane for #3878 removes that
// accidental cover and makes the exposure reliable, so the redaction has to
// become real. Truncation is not a security control.
//
// The {6,} floor keeps the pattern off ordinary prose ("sk-" alone) while still
// catching short virtual keys such as sk-hive-qa.
//
// The leading (^|[^A-Za-z0-9._-]) guard anchors the match to a real token
// start. Without it the pattern fires INSIDE ordinary identifiers — a module
// named "task-sk-flow" was rewritten to "task-[redacted]", which would corrupt
// the very tool-call text this change exists to make readable. A redactor that
// eats legitimate output is its own bug, so the boundary is asserted by
// TestRedactTokensPreservesNonSecretText.
var apiKeyRedactor = regexp.MustCompile(`(^|[^A-Za-z0-9._-])(sk-[A-Za-z0-9._\-]{6,})`)

func redactTokens(s string) string {
	s = statusTokenRedactor.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) > 7 {
			return m[:7] + "***"
		}
		return "***"
	})
	// ${1} preserves the boundary character the pattern had to consume.
	s = apiKeyRedactor.ReplaceAllString(s, "${1}sk-***REDACTED***")
	s = deviceCodeRedactor.ReplaceAllString(s, "${1}****-****")
	s = deviceCodeLineRedactor.ReplaceAllString(s, "[auth flow redacted]")
	return s
}

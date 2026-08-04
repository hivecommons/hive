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

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/resolve"
	"github.com/kubestellar/hive/v2/pkg/tokens"
)

const defaultLookbackHours = 24

// Cadence sentinel values: a per-mode cadence set to one of these means the
// governor will not kick the agent on a timer in that mode. Kept as named
// constants so the offByCadence detection and computeNextKick agree on the
// exact spellings the config layer emits.
const (
	cadencePause = "pause"
	cadenceOff   = "off"
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

var (
	backendAuthMu sync.RWMutex
	backendAuthFn func(backend string) (bool, bool)

	cachedHealth   map[string]any
	cachedHealthMu sync.RWMutex

	proxyViolationsMu sync.RWMutex
	proxyViolationsFn func() map[string]int

	entitledModelsMu sync.RWMutex
	entitledModelsFn func(endpoint string) (models []string, source string, known bool)

	inferenceAuthMu sync.RWMutex
	inferenceAuthFn func() (errMsg string, since time.Time)
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
	Timestamp string          `json:"timestamp"`
	Agents    []FrontendAgent `json:"agents"`
	GovMode   string          `json:"govMode"`
}

// BuildAgentOnlyStatus builds a lightweight agent-only status from in-memory
// data. No GitHub API calls, no metrics collection — just agent state.
func BuildAgentOnlyStatus(
	govState governor.State,
	agentStatuses map[string]*agent.AgentProcess,
	cfg *config.Config,
) *AgentStatusPayload {
	return &AgentStatusPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Agents:    buildAgents(agentStatuses, cfg, govState),
		GovMode:   strings.ToLower(string(govState.Mode)),
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
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		HiveID:          cfg.HiveID,
		Agents:          buildAgents(agentStatuses, cfg, govState),
		Governor:        buildGovernor(govState, cfg),
		Tokens:          buildTokens(tokenCollector),
		Repos:           buildRepos(cfg, actionable),
		Beads:           BuildBeadsFromConfig(beadStores, cfg),
		Health:          buildHealth(ghClient, ctx),
		Budget:          buildBudget(gov, tokenCollector),
		CadenceMatrix:   buildCadenceMatrix(cfg, agentStatuses),
		GHRateLimits:    buildGHRateLimits(ghClient, ctx, cfg),
		AgentMetrics:    agentMetrics,
		Hold:            buildHold(actionable),
		IssueToMerge:    issueToMerge,
		ACMMLevel:       detectACMMLevel(cfg),
		ACMMPackAgents:  buildACMMPackAgents(cfg),
		SystemResources: collectSystemResources(),
	}
	return payload
}

func buildAgents(statuses map[string]*agent.AgentProcess, cfg *config.Config, govState governor.State) []FrontendAgent {
	currentMode := strings.ToLower(string(govState.Mode))

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

		cadence := lookupCadenceForMode(name, currentMode, cfg)
		if cadence == "" {
			cadence = lookupCadence(name, cfg)
		}
		nextKick := computeNextKick(proc.LastKick, cadence)

		// offByCadence: the agent's cadence for the CURRENT governor mode is a
		// non-kicking value ("pause"/"off"), so the governor will never kick it
		// even though the process itself may be perfectly healthy. This is how
		// a hive stuck in SURGE (where surge:{scanner:pause,...}) leaves every
		// agent alive-but-off. The dashboard surfaces this as a hollow-green
		// dot so operators do not mistake a governor-paused agent for a running
		// one. On-demand agents are excluded — they are intentionally not on a
		// cadence and carry their own on-demand styling.
		offByCadence := (cadence == cadencePause || cadence == cadenceOff) &&
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
			OnDemand:      agentCfg.OnDemand,
			Session:       name,
			State:         string(proc.State),
			Busy:          busy,
			Paused:        proc.Paused,
			PausedAt:      formatOptionalTime(proc.PausedAt),
			PausedReason:  proc.PausedReason,
			PausedTrigger: proc.PausedTrigger,
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
		if authFn := getBackendAuthFn(); authFn != nil {
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
	if cadence == "" || cadence == "off" || cadence == "pause" || cadence == "on demand" {
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

func parseCadenceDuration(cadence string) time.Duration {
	cadence = strings.TrimSpace(cadence)
	if cadence == "" || cadence == "off" || cadence == "pause" || cadence == "on demand" {
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
	if mode, ok := cfg.Governor.Modes[modeName]; ok {
		if c, ok := mode.Cadences[agentName]; ok {
			return c
		}
	}
	return ""
}

func buildGovernor(state governor.State, cfg *config.Config) FrontendGovernor {
	const (
		defaultQuiet = 2
		defaultBusy  = 10
		defaultSurge = 20
	)

	thresholds := FrontendThresholds{
		Quiet: defaultQuiet,
		Busy:  defaultBusy,
		Surge: defaultSurge,
	}

	// Threshold zero means unset (mode entry present only for cadences);
	// keep the defaults so the gauge matches the governor's evaluation.
	if m, ok := cfg.Governor.Modes["quiet"]; ok && m.Threshold > 0 {
		thresholds.Quiet = m.Threshold
	}
	if m, ok := cfg.Governor.Modes["busy"]; ok && m.Threshold > 0 {
		thresholds.Busy = m.Threshold
	}
	if m, ok := cfg.Governor.Modes["surge"]; ok && m.Threshold > 0 {
		thresholds.Surge = m.Threshold
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

	var totalTokens int64
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			totalTokens = summary.TotalTokens
		}
	}

	// Compute hours elapsed since last budget reset
	var hoursElapsed float64
	if !budget.ResetAt.IsZero() {
		hoursElapsed = time.Since(budget.ResetAt).Hours()
		if hoursElapsed < 0 {
			hoursElapsed = 0
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
		WeeklyBudget: budget.WeeklyLimit,
		Used:         used,
		LastUpdated:  time.Now().UTC().Format(time.RFC3339),
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
			const hoursPerWeek = 168.0
			fb.ProjectedWeekly = int64(burnRate * hoursPerWeek)
			fb.ProjectedPct = float64(fb.ProjectedWeekly) / float64(budget.WeeklyLimit) * pctMultiplier
			if burnRate > 0 {
				fb.HoursRemaining = float64(remaining) / burnRate
			}
		}
		fb.HoursElapsed = hoursElapsed

		fb.Exhausted = used >= budget.WeeklyLimit
		if !budget.ResetAt.IsZero() {
			fb.WindowEndsAt = budget.ResetAt.Add(governor.BudgetWindowDuration).UTC().Format(time.RFC3339)
		}
	}

	return fb
}

func buildCadenceMatrix(cfg *config.Config, agentStatuses map[string]*agent.AgentProcess) []FrontendCadence {
	sortedNames := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
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
			cadence := mode.Cadences[name]
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
			case "quiet":
				entry.Quiet = cadence
			case "busy":
				entry.Busy = cadence
			case "surge":
				entry.Surge = cadence
			}
		}
		matrix = append(matrix, entry)
	}
	return matrix
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

var statusTokenRedactor = regexp.MustCompile(`(ghp_|gho_|ghs_|github_pat_)[A-Za-z0-9_]{10,}`)
var deviceCodeRedactor = regexp.MustCompile(`(?i)(one-time code:\s*)[A-Z0-9]{4}-[A-Z0-9]{4}`)
var deviceCodeLineRedactor = regexp.MustCompile(`(?m)^.*(?:login/device|Waiting for authorization|one-time code:|Press any key to copy).*$`)

func redactTokens(s string) string {
	s = statusTokenRedactor.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) > 7 {
			return m[:7] + "***"
		}
		return "***"
	})
	s = deviceCodeRedactor.ReplaceAllString(s, "${1}****-****")
	s = deviceCodeLineRedactor.ReplaceAllString(s, "[auth flow redacted]")
	return s
}

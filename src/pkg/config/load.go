package config

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/skillreg"

	"bufio"
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/resolve"
	"gopkg.in/yaml.v3"
)

// ApplyAgentSpecRepoScopes refreshes config-visible repo scopes from BYO agent
// specs. Runtime enforcement reads Config.Agents, not AgentProcess.Config, so a
// spec-owned repos: list has to be copied here during config load/reload.
//
// Operator-owned scopes win: if the dashboard or hand-edited config marks
// repos_owner: operator, that explicit operator choice is preserved instead of
// being replaced by the spec. Otherwise the spec is authoritative, including an
// omitted repos: key, which means hive-wide.
func (c *Config) ApplyAgentSpecRepoScopes() error {
	if c == nil {
		return nil
	}
	for name, agent := range c.Agents {
		if agent.AgentSpec == "" || agent.ReposOwner == FieldOwnerOperator {
			continue
		}
		spec, err := skillreg.LoadAgentSpec(agent.AgentSpec)
		if err != nil {
			return fmt.Errorf("agent %s: %w", name, err)
		}
		repos := skillreg.SpecRepos(spec)
		agentReposMu.Lock()
		agent.Repos = append([]string(nil), repos...)
		agent.ReposOwner = FieldOwnerSpec
		c.Agents[name] = agent
		agentReposMu.Unlock()
	}
	return nil
}

// Load reads hive.yaml, then applies config.env overrides if present.
// Precedence: hive.yaml < config.env < explicit env vars (via ${} interpolation).
func Load(path string) (*Config, error) {
	return LoadWithOverrides(path, "")
}

func LoadForHub(path string) (*Config, error) {
	return loadWithOverrides(path, "", ValidateOptions{RequireAgents: false})
}

// LoadWithOverrides reads hive.yaml and applies a config.env override file.
// If envPath is empty, it looks for config.env next to hive.yaml, then at
// /etc/hive/config.env. Pass "-" to skip config.env entirely.
func LoadWithOverrides(path, envPath string) (*Config, error) {
	return loadWithOverrides(path, envPath, ValidateOptions{RequireAgents: true})
}

func loadWithOverrides(path, envPath string, validateOpts ValidateOptions) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if envPath != "-" {
		if envPath == "" {
			envPath = findConfigEnv(path)
		}
		if envPath != "" {
			if err := cfg.applyConfigEnv(envPath); err != nil {
				return nil, fmt.Errorf("applying config.env %s: %w", envPath, err)
			}
		}
	}

	cfg.SourcePath = path
	cfg.applyBootstrapEnv()
	cfg.applyDefaults()

	// Merge per-agent overlay files from the agents directory.
	if cfg.Data.AgentsDir != "" {
		overlays, err := LoadAgentOverrides(cfg.Data.AgentsDir)
		if err != nil {
			return nil, fmt.Errorf("loading agent overlays: %w", err)
		}
		overlays = cfg.RejectInvalidAgentOverlays(overlays)
		cfg.MergeAgentOverrides(overlays)
		// Re-apply defaults for overlay agents.
		for name := range overlays {
			cfg.ApplyAgentDefaults(name)
		}
	}

	if err := cfg.ApplyAgentSpecRepoScopes(); err != nil {
		return nil, fmt.Errorf("applying agent spec repo scopes: %w", err)
	}

	if err := cfg.ExpandAgentReplicas(); err != nil {
		return nil, fmt.Errorf("expanding agent replicas: %w", err)
	}

	if err := cfg.validateWithOptions(validateOpts); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// LoadWithDashboardOverlay loads the config from path, then — in Kubernetes
// mode — re-applies the dashboard overlay's agent configs on top, mirroring
// the entrypoint's boot-time seed+overlay merge.
//
// Why this exists: the ConfigMap seed at path carries only provision-time
// agent fields. Runtime reconciliation (ApplyPack raising a hive's ACMM level
// updates kick_template/mode/model) is persisted to the dashboard overlay, NOT
// the seed. The entrypoint merges the overlay over the seed once at boot, but a
// live ConfigMap remount rewrites the seed back to its stale values and fires
// the config watcher. If the watcher reloaded the raw seed it would silently
// revert every reconciled agent field (observed: a hive raised to L5 dropped
// its scanner back to the L2/L3 advisory template at runtime). Applying the
// overlay here keeps the reload consistent with boot.
func LoadWithDashboardOverlay(path string) (*Config, error) {
	return loadWithDashboardOverlay(path, ValidateOptions{RequireAgents: true})
}

func LoadWithDashboardOverlayForHub(path string) (*Config, error) {
	return loadWithDashboardOverlay(path, ValidateOptions{RequireAgents: false})
}

func loadWithDashboardOverlay(path string, validateOpts ValidateOptions) (*Config, error) {
	cfg, err := loadWithOverrides(path, "", validateOpts)
	if err != nil {
		return nil, err
	}
	if !IsKubernetesPod() {
		return cfg, nil
	}
	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil {
		// No overlay (or unreadable) — the seed is authoritative, as at boot.
		return cfg, nil
	}
	var overlay Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &overlay); err != nil {
		return cfg, nil // malformed overlay: fall back to seed, don't fail the reload
	}
	// Tombstones live in the dashboard overlay because that is the only agent
	// source the dashboard can write. Adopt them BEFORE the fullness guard below
	// so a short/empty overlay (one that has no agents yet, or only carries the
	// removed_agents list) still yields the tombstone. Previously this ran AFTER
	// the guard, so on a reload the guard's early return dropped RemovedAgents to
	// empty; the ~2-min saver then rewrote every layer tombstone-free and the
	// deleted agents reappeared on an interval (#2439). Merge already skips and
	// prunes tombstoned agents, so adopting them early is safe even when we bail.
	if !overlay.OTel.IsZero() {
		cfg.OTel = overlay.OTel
		cfg.Tracing = overlay.OTel
	} else if !overlay.Tracing.IsZero() {
		cfg.Tracing = overlay.Tracing
		cfg.OTel = mergeOTelOverride(cfg.OTel, overlay.Tracing)
	}
	// Governor work source: the dashboard's PUT /api/config/governor/work-source
	// writes the whole config to the overlay, but the reload only adopted
	// OTel/Tracing, RemovedAgents and Agents from it — so a work source set
	// from the dashboard was lost on every pod restart and GET returned
	// type "" again. Adopt it here, BEFORE the fullness guard, so a short
	// overlay (no agents yet) still carries it. The whole block is copied so
	// per-adapter settings (teams, hold labels, assigned_only, …) survive with
	// the type. Nothing here touches overlay.Variables — see the security
	// invariant at the end of this function.
	if !overlay.Governor.WorkSource.IsZero() {
		cfg.Governor.WorkSource = overlay.Governor.WorkSource
	}
	// Operator-owned governor cadences (#5632): PUT /api/config/agent/{name}/
	// cadences persists to the overlay, but this reload used to rebuild
	// Governor.Modes from the seed alone — so a ConfigMap remount dropped both
	// the operator's cadence AND its ownership marker from memory, and the next
	// pack apply saw nothing to respect. Adopt them BEFORE the fullness guard,
	// like WorkSource, so a short overlay still carries them.
	adoptOperatorCadenceOverrides(cfg, &overlay)
	if len(overlay.RemovedAgents) > 0 {
		cfg.RemovedAgents = overlay.RemovedAgents
		cfg.PruneRemovedAgents()
		// Observability (#2439): this runs on boot AND on every ~2-min config reload,
		// so keep it at DEBUG. It confirms the reload adopted the overlay's tombstones
		// BEFORE the fullness guard below — the exact ordering whose absence let the
		// deleted agents reappear on an interval.
		slog.Default().Debug("reload: adopted removed-agents from overlay",
			"hive_id", cfg.HiveID,
			"count", len(cfg.RemovedAgents),
			"agents", cfg.RemovedAgents,
		)
	}
	// Guard: the overlay must look like a full hive config (same check the
	// entrypoint and validateSaveGuard apply) before we trust its agents.
	if overlay.Project.Org == "" || len(overlay.Agents) == 0 {
		return cfg, nil
	}
	// Overlay agents win — they carry the reconciled pack-behavior fields.
	//
	// `converse` is not one of those fields, and a dashboard entry that is
	// SILENT on it must not revoke it (#7503). Converse is a pointer precisely
	// so "unset" and "explicitly false" stay distinguishable across this
	// overlay; no pack seeds it, and the dashboard API writes it to BOTH this
	// overlay and the per-agent file, so an explicit value here is always a
	// real decision. A nil here means the dashboard never had an opinion —
	// yet MergeAgentOverrides replaces the whole entry, so the per-agent
	// file's `converse: true` (the documented layer for agent fields, and the
	// one that outranks this overlay) was dropped on every boot and reload.
	// Carry the lower layer's value forward when, and only when, the overlay
	// says nothing; an explicit false still wins.
	for name, oa := range overlay.Agents {
		if oa.Converse != nil {
			continue
		}
		if base, ok := cfg.Agents[name]; ok && base.Converse != nil {
			oa.Converse = base.Converse
			overlay.Agents[name] = oa
		}
	}
	agents := cfg.RejectInvalidAgentOverlays(overlay.Agents)
	cfg.MergeAgentOverrides(agents)
	for name := range agents {
		cfg.ApplyAgentDefaults(name)
	}
	if err := cfg.ApplyAgentSpecRepoScopes(); err != nil {
		return cfg, err
	}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		return cfg, err
	}
	// Security invariant: cfg.Variables (resolver defs + the exec/http trust
	// policy) comes ONLY from the seed loaded above — the overlay's Variables
	// block is intentionally NOT merged. The dashboard overlay is user-writable,
	// so honoring its resolver policy would let a compromised overlay enable
	// script/http execution. Keep this true if overlay merging is ever expanded.
	return cfg, nil
}

// adoptOperatorCadenceOverrides re-applies the dashboard overlay's
// OPERATOR-OWNED governor cadences (and their ownership markers) on top of the
// seed's governor config. Only entries the overlay marks FieldOwnerOperator
// are copied — pack-seeded cadences keep following the seed, so this cannot
// carry a stale pack value forward. Nothing here touches overlay.Variables or
// any other security-sensitive block (see the invariant at the end of
// LoadWithDashboardOverlay).
func adoptOperatorCadenceOverrides(cfg *Config, overlay *Config) {
	for modeName, owners := range overlay.Governor.CadenceOwners {
		overlayMode, hasMode := overlay.Governor.Modes[modeName]
		if !hasMode {
			continue
		}
		for agentName, owner := range owners {
			if owner != FieldOwnerOperator {
				continue
			}
			cadence, hasCadence := overlayMode.Cadences[agentName]
			if !hasCadence {
				continue
			}
			if cfg.Governor.Modes == nil {
				cfg.Governor.Modes = make(map[string]ModeConfig)
			}
			mode := cfg.Governor.Modes[modeName]
			if mode.Cadences == nil {
				mode.Cadences = make(map[string]Cadence)
			}
			mode.Cadences[agentName] = cadence
			cfg.Governor.Modes[modeName] = mode
			cfg.Governor.ClaimCadenceOwnership(modeName, agentName)
		}
	}
}

// findConfigEnv returns the path to a config.env file, or "" if none found.
func findConfigEnv(yamlPath string) string {
	candidates := []string{
		strings.TrimSuffix(yamlPath, "hive.yaml") + "config.env",
		"/etc/hive/config.env",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ParseEnvFile reads a flat KEY=VALUE file (# comments, blank lines skipped).
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		result[key] = val
	}
	return result, scanner.Err()
}

// applyConfigEnv merges flat KEY=VALUE overrides into the loaded config.
func (c *Config) applyConfigEnv(path string) error {
	env, err := ParseEnvFile(path)
	if err != nil {
		return err
	}

	if v, ok := env["PROJECT_ORG"]; ok {
		c.Project.Org = v
	}
	if v, ok := env["PROJECT_REPOS"]; ok {
		c.Project.Repos = strings.Fields(v)
	}
	if v, ok := env["PROJECT_AI_AUTHOR"]; ok {
		c.Project.AIAuthor = v
	}
	if v, ok := env["PROJECT_PRIMARY_REPO"]; ok {
		c.Project.PrimaryRepo = v
	}
	if v, ok := env["PROJECT_OPEN_PRS"]; ok {
		b := v == "true" || v == "1" || v == "yes"
		c.Project.OpenPRs = &b
	}
	if v, ok := env["AGENTS_ENABLED"]; ok {
		for _, name := range strings.Fields(v) {
			if agent, exists := c.Agents[name]; exists {
				agent.Enabled = true
				c.Agents[name] = agent
			}
		}
	}
	if v, ok := env["DASHBOARD_PORT"]; ok {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			c.Dashboard.Port = port
		}
	}
	if v, ok := env["DASHBOARD_AUTH_TOKEN"]; ok {
		c.Dashboard.AuthToken = v
	}
	if c.Dashboard.AuthToken == "" {
		if v, ok := env["HIVE_DASHBOARD_TOKEN"]; ok {
			c.Dashboard.AuthToken = v
		}
	}
	if c.Dashboard.AuthToken == "" {
		c.Dashboard.AuthToken = readDashboardAuthTokenFile()
	}

	return nil
}

func (c *Config) applyBootstrapEnv() {
	if repo := os.Getenv("HIVE_REPO"); repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			if c.Project.Org == "" {
				c.Project.Org = parts[0]
			}
			if len(c.Project.Repos) == 0 {
				c.Project.Repos = []string{parts[1]}
			}
			if c.Project.PrimaryRepo == "" {
				c.Project.PrimaryRepo = parts[1]
			}
		}
	}
	// K8s deployments pass the auth token as an OS env var from a Secret.
	// applyConfigEnv only reads file-based config.env, so without this
	// the token is silently ignored and the dashboard is unauthenticated.
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("DASHBOARD_AUTH_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("HIVE_DASHBOARD_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	if c.Dashboard.AuthToken == "" {
		c.Dashboard.AuthToken = readDashboardAuthTokenFile()
	}
	// K8s-provisioned spokes receive their per-hive authorized GitHub users as a
	// comma-separated env var (owner first). This is what lets a direct-route
	// spoke reject unauthorized device-flow logins without the hub proxy.
	if len(c.Dashboard.AuthorizedUsers) == 0 {
		if v := os.Getenv("HIVE_AUTHORIZED_USERS"); v != "" {
			c.Dashboard.AuthorizedUsers = parseAuthorizedUsers(v)
		}
	}
	if v := os.Getenv("HIVE_SELF_AUTHORIZATION_HOLD"); strings.TrimSpace(v) != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			c.GitHub.SelfAuthorizationHold = &b
			c.GitHub.selfAuthorizationHoldEnvOverride = &b
		}
	}
	if v := strings.TrimSpace(os.Getenv(ContributeSkipLabelsEnvVar)); v != "" {
		c.Hub.ContributeSkipLabels = parseContributeSkipLabels(v)
	}
}

func readDashboardAuthTokenFile() string {
	path := strings.TrimSpace(os.Getenv("DASHBOARD_AUTH_TOKEN_FILE"))
	if path == "" {
		path = dashboardAuthTokenFile
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parseAuthorizedUsers splits a comma-separated authorized-users list, trimming
// whitespace and dropping empty entries. Order is preserved so the first entry
// remains the owner.
func parseAuthorizedUsers(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if u := strings.TrimSpace(p); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// expandEnvVars substitutes ${VAR} references in the raw config text. It runs
// BEFORE the YAML is parsed, so to honor an operator `variables:` block it
// first bootstrap-parses just that block from the same text, builds a
// config-scoped resolve.Registry, and delegates. With no `variables:` block the
// registry is env-only and the result is byte-identical to the legacy behavior
// (${NAME} -> os.LookupEnv(NAME), unset left literal).
func expandEnvVars(s string) string {
	reg := configRegistryFromText(s)
	return reg.Expand(context.Background(), s, resolve.ScopeConfig, nil)
}

// bootstrapVariables is a minimal view of hive.yaml used to read the
// `variables:` block before the whole document is expanded/parsed. Unknown keys
// are ignored by yaml.Unmarshal, so this is cheap and tolerant.
type bootstrapVariables struct {
	Variables VariablesConfig `yaml:"variables"`
}

// configRegistryFromText bootstrap-parses the variables block from raw config
// text and returns a config-scoped resolve.Registry. On any parse failure it
// falls back to an env-only registry (legacy behavior), never an error — config
// expansion must not fail the load.
func configRegistryFromText(raw string) *resolve.Registry {
	var bv bootstrapVariables
	if err := yaml.Unmarshal([]byte(raw), &bv); err != nil {
		return resolve.EnvOnly()
	}
	if len(bv.Variables.Defs) == 0 {
		// No custom variables — env-only, byte-identical to legacy.
		return resolve.EnvOnly()
	}
	specs, pol := bv.Variables.toResolveSpecs()
	return resolve.Build(specs, pol, nil)
}

// toResolveSpecs translates the config-level variables block into the
// resolve package's VarSpec list and Policy.
func (v VariablesConfig) toResolveSpecs() ([]resolve.VarSpec, resolve.Policy) {
	specs := make([]resolve.VarSpec, 0, len(v.Defs))
	for name, def := range v.Defs {
		spec := resolve.VarSpec{
			Name:    name,
			Type:    def.Type,
			Scope:   resolve.Scope(def.Scope),
			Value:   def.Value,
			Env:     def.Env,
			Command: def.Command,
			URL:     def.URL,
			Headers: def.Headers,
		}
		if def.Default != nil {
			spec.HasDefault = true
			spec.Default = *def.Default
		}
		specs = append(specs, spec)
	}
	pol := resolve.Policy{
		AllowExec:     v.Security.AllowExec,
		AllowHTTP:     v.Security.AllowHTTP,
		HTTPAllowlist: v.Security.HTTPAllowlist,
		ExecTimeoutS:  v.Security.ExecTimeoutS,
		HTTPTimeoutS:  v.Security.HTTPTimeoutS,
	}
	return specs, pol
}

// ResolveRegistry builds a resolve.Registry from this config's `variables:`
// block, for use at per-kick template substitution sites (scheduler, dashboard
// preview). With no variables configured it returns an env-only registry whose
// Expand — in template scope, where the runtime built-ins win and there is no
// env fallback — reproduces the previous strings.NewReplacer output exactly.
// Pass a logger to surface disabled/invalid resolver diagnostics; nil is fine.
func (c *Config) ResolveRegistry(logger *slog.Logger) *resolve.Registry {
	if len(c.Variables.Defs) == 0 {
		return resolve.EnvOnly()
	}
	specs, pol := c.Variables.toResolveSpecs()
	return resolve.Build(specs, pol, logger)
}

// GitHubPromptAllowed reports whether an agent's prompt_source pointing at the
// given "owner/repo" slug is permitted to be fetched. This mirrors the seed-only
// gating used for exec/http resolvers: it consults c.Variables.Security, which
// LoadWithDashboardOverlay guarantees comes ONLY from the trusted config seed
// (the dashboard overlay's Variables block is never merged). It returns false
// unless the feature is explicitly enabled AND the slug is on the seed-declared
// allowlist, so a user-writable overlay can never widen the set of readable repos.
func (c *Config) GitHubPromptAllowed(slug string) bool {
	if slug == "" || !c.Variables.Security.AllowGitHubPrompt {
		return false
	}
	for _, allowed := range c.Variables.Security.GitHubPromptAllowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), slug) {
			return true
		}
	}
	return false
}

// GitHubDefinitionAllowed reports whether an agent's definition_source pointing
// at the given "owner/repo" slug is permitted to be fetched. A live whole-agent
// definition is at least as trusted as a live prompt (it re-applies more fields),
// so it reuses the SAME seed-only gate as GitHubPromptAllowed: the feature flag
// and repo allowlist come only from the trusted config seed, never the
// user-writable dashboard overlay (LoadWithDashboardOverlay never merges the
// overlay's Variables block). A compromised overlay therefore can neither widen
// the set of readable repos nor point a live definition at an arbitrary repo.
func (c *Config) GitHubDefinitionAllowed(slug string) bool {
	return c.GitHubPromptAllowed(slug)
}

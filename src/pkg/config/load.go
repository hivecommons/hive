package config

import (
	"fmt"
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*Config, error) {
	return LoadWithOverrides(path, "")
}

// LoadWithOverrides reads hive.yaml and applies a config.env override file.
// If envPath is empty, it looks for config.env next to hive.yaml, then at
// /etc/hive/config.env. Pass "-" to skip config.env entirely.
func LoadWithOverrides(path, envPath string) (*Config, error) {
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

	if err := cfg.ExpandAgentReplicas(); err != nil {
		return nil, fmt.Errorf("expanding agent replicas: %w", err)
	}

	if err := cfg.validate(); err != nil {
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
	cfg, err := Load(path)
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
	agents := cfg.RejectInvalidAgentOverlays(overlay.Agents)
	cfg.MergeAgentOverrides(agents)
	for name := range agents {
		cfg.ApplyAgentDefaults(name)
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

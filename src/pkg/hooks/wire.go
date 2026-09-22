package hooks

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// This file is the ONLY place pkg/hooks touches pkg/config, mirroring
// pkg/celtrigger's wire.go. Keeping the conversion isolated means the rest of
// the package — catalog, registry, dispatcher, actions — is testable without
// constructing a Config, and it keeps the config-shape dependency to one
// reviewable function.

// FromConfig converts the operator's `hooks:` list into the in-package form.
// It does no validation of its own: Compile owns the closed-vocabulary checks
// so there is exactly one fail-closed gate rather than two that can drift.
func FromConfig(cfg *config.Config) []Hook {
	if cfg == nil || len(cfg.Hooks) == 0 {
		return nil
	}
	out := make([]Hook, 0, len(cfg.Hooks))
	for _, h := range cfg.Hooks {
		out = append(out, Hook{
			Name:               h.Name,
			On:                 Transition(h.On),
			Action:             Action(h.Action),
			Params:             h.Params,
			When:               h.When,
			RateLimitPerMinute: h.RateLimitPerMinute,
		})
	}
	return out
}

// CompileFromConfig compiles the operator's `hooks:` list into a Registry.
// A config with no hooks yields an empty Registry (not an error), which makes
// every Fire a cheap no-op.
func CompileFromConfig(cfg *config.Config) (*Registry, error) {
	hooks := FromConfig(cfg)
	if err := validateConfigAgents(cfg, hooks); err != nil {
		return nil, err
	}
	return Compile(hooks)
}

// SignatureFromConfig returns a stable identity for the config's hook list, so
// the wiring layer can recompile only when the operator actually changed it.
func SignatureFromConfig(cfg *config.Config) string {
	return Signature(FromConfig(cfg))
}

func validateConfigAgents(cfg *config.Config, hooks []Hook) error {
	if cfg == nil {
		return nil
	}
	for _, h := range hooks {
		if h.Action != ActionKick {
			continue
		}
		agent := strings.TrimSpace(h.Params["agent"])
		if agent == "" {
			continue
		}
		if _, ok := cfg.Agents[agent]; !ok {
			return fmt.Errorf("hooks: %q: kick: unknown agent %q", h.Name, agent)
		}
	}
	return nil
}

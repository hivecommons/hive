package hooks

import (
	"fmt"
	"strings"
	"sync"

	"github.com/kubestellar/hive/pkg/config"
)

// Registry manages declarative hook rules and matches them against state transition events.
type Registry struct {
	mu    sync.RWMutex
	rules []config.HookRule
}

// NewRegistry creates a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		rules: make([]config.HookRule, 0),
	}
}

// NewRegistryFromConfig builds a Registry populated from a HooksConfig.
func NewRegistryFromConfig(cfg config.HooksConfig) (*Registry, error) {
	reg := NewRegistry()
	for _, rule := range cfg.Rules {
		if err := reg.Register(rule); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// Register adds and validates a single hook rule.
func (r *Registry) Register(rule config.HookRule) error {
	on := strings.TrimSpace(rule.On)
	if on == "" {
		return fmt.Errorf("hook rule missing 'on' transition")
	}

	action := strings.ToLower(strings.TrimSpace(rule.Action))
	switch action {
	case "notify", "script", "pacing", "audit":
		// valid
	default:
		return fmt.Errorf("unknown hook action %q (supported: notify, script, pacing, audit)", rule.Action)
	}

	rule.On = on
	rule.Action = action

	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
	return nil
}

// Rules returns a copy of all currently registered rules.
func (r *Registry) Rules() []config.HookRule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.HookRule, len(r.rules))
	copy(out, r.rules)
	return out
}

// Match returns all rules matching the transition specified in the Event.
func (r *Registry) Match(event Event) []config.HookRule {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var matches []config.HookRule
	for _, rule := range r.rules {
		if matchTransition(rule.On, event.Transition) {
			matches = append(matches, rule)
		}
	}
	return matches
}

func matchTransition(pattern, transition string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	transition = strings.ToLower(strings.TrimSpace(transition))

	if pattern == "*" || pattern == transition {
		return true
	}

	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(transition, prefix)
	}

	return false
}

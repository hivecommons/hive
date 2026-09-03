package celtrigger

import "github.com/hivecommons/hive/pkg/config"

// CompileFromConfig builds an Engine from the additive `triggers:` list in the
// loaded config. It is the intended entry point for the governor/pipeline: call
// it once after config load, then use MatchAgents (or Engine.Match) per event.
//
// Fail-closed: if any rule's expression is malformed, an error is returned and
// no Engine is produced, so the operator can reject the config instead of the
// fleet crashing later. An empty triggers list returns a valid Engine that
// never matches — preserving existing label/governor behavior.
func CompileFromConfig(cfg *config.Config) (*Engine, error) {
	if cfg == nil || len(cfg.Triggers) == 0 {
		// Empty rule set: a valid, never-matching engine.
		return Compile(nil)
	}
	rules := make([]Rule, 0, len(cfg.Triggers))
	for _, t := range cfg.Triggers {
		rules = append(rules, Rule{
			Name:     t.Name,
			Expr:     t.Expr,
			Agent:    t.Agent,
			Priority: t.Priority,
		})
	}
	return Compile(rules)
}

// MatchAgents returns the de-duplicated agent names to trigger for the given
// event, in priority order (highest first). It is the light integration point
// the governor/pipeline calls alongside — not instead of — Hive's built-in
// label/governor triggering.
//
// Wired in: the governor eval cycle (cmd/hive, celMatchedAgents/runEvalCycle)
// calls this per enumerated actionable item and UNIONS the result into the
// due-agents set, after applying the governor's own pause/budget/on-demand
// gates. It is enabled purely by the additive `triggers:` config — an empty
// list yields a nil engine and this is never reached.
func (e *Engine) MatchAgents(event NormalizedEvent) []string {
	matched := e.Match(event)
	if len(matched) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matched))
	agents := make([]string, 0, len(matched))
	for _, r := range matched {
		if r.Agent == "" {
			continue
		}
		if _, dup := seen[r.Agent]; dup {
			continue
		}
		seen[r.Agent] = struct{}{}
		agents = append(agents, r.Agent)
	}
	return agents
}

package dashboard

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux"
)

// malformedModelIDError rejects an operator model id that the backend's CLI
// is certain to refuse, so the mistake is a 400 at save time instead of an
// agent that launches, has every request refused, and restart-loops.
//
// The rule is deliberately narrow, because a model catalogue is never
// complete (new ids ship before hive's lists learn them) and several backends
// take ids hive cannot enumerate at all:
//
//   - claude (Claude Code against the Anthropic API): a bare "claude-…" id
//     with a dot is malformed. Every Anthropic model id is dash-separated —
//     claude-sonnet-5-5, claude-opus-4-1-20250805 — and Claude Code refuses
//     the dotted spelling ("There's an issue with the selected model
//     (claude-sonnet-5.5)"). The dotted form is the Copilot / gateway
//     nomenclature, which is why it is easy to carry over by mistake.
//     Bedrock ("us.anthropic.claude-…") and Vertex ("claude-…@date") ids do
//     not start with a dotted "claude-" family and are untouched.
//   - every other backend: no shape rule. Copilot takes dotted 4.x/5.5 ids
//     and canonicalizes separator drift itself; inference backends and
//     gateways pass the id to the gateway verbatim, where dots are normal
//     (anthropic/claude-opus-4.8 on OpenRouter).
//
// A well-formed id that is simply not in hive's list is NOT rejected here —
// see unknownModelIDWarning.
func malformedModelIDError(backend, model string) error {
	if backend != "claude" || !strings.HasPrefix(model, "claude-") || !strings.Contains(model, ".") {
		return nil
	}
	return fmt.Errorf("model %q is not a valid Claude Code model id: Anthropic model ids use dashes, not dots (did you mean %q?)",
		model, strings.ReplaceAll(model, ".", "-"))
}

// unknownModelIDWarning returns a non-blocking warning when a well-formed
// model id is not in the list hive knows for the backend: the live discovery
// result when one is cached, else the maintained static list. It never
// probes, so it adds no latency to the save. An unknown id may be a model
// released after hive's list was updated, so it is saved and only flagged;
// if the CLI does refuse it, the agent shows "model <id> rejected by the CLI"
// and backs off instead of restart-looping.
func (s *Server) unknownModelIDWarning(backend, model string) string {
	if model == "" || backend == "" {
		return ""
	}
	var known []string
	if s.cliModels != nil {
		if r, ok := s.cliModels.getStale(backend); ok {
			known = r.models
		}
	}
	if len(known) == 0 {
		known = cliStaticFallback(backend)
	}
	if len(known) == 0 {
		return "" // backend with no enumerable list (gateways, inference): nothing to compare
	}
	candidate := model
	if backend == "copilot" {
		candidate = crux.CanonicalizeCopilotModel(model)
	}
	for _, id := range known {
		if id == candidate || id == model {
			return ""
		}
	}
	return fmt.Sprintf("model %q is not in the %s model list this hive knows; saved anyway in case it is newer than the list. If the CLI rejects it, the agent shows \"model %s rejected by the CLI\" and stops being kicked until the model is fixed.",
		model, backend, model)
}

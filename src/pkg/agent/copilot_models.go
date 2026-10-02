package agent

import (
	"strings"
)

// copilotCLIAcceptedModels are the model ids the copilot CLI's --model flag
// accepts, in the CLI's own nomenclature. This is the alias target set for
// CanonicalizeCopilotModel — NOT an allowlist: ids absent from this list are
// passed through unchanged, so it only needs to cover ids whose separator
// spelling is known to drift in the catalog. Note the deliberate mix: bare
// generation numbers (claude-opus-5) have no separator to drift; every X.Y
// minor-version id — Claude's 4.x AND 5.5, GPT's 5.x, Gemini's — is DOTTED.
//
// claude-opus-5-5 and claude-sonnet-5-5 (DASHED) are deliberately absent:
// #9927 found the pinned Copilot CLI 1.0.88 rejects that dashed spelling
// from --model and silently falls back to claude-sonnet-5. The dotted
// claude-opus-5.5 / claude-sonnet-5.5 below are the CLI's actual accepted
// ids for that model (consistent with every other X.Y id in this list) and
// restore the alias so a dashed input still reaches the CLI as a dotted,
// accepted id instead of being dropped entirely. Keep in sync with
// copilotPinnedCLIModels in pkg/dashboard/cli_models.go.
var copilotCLIAcceptedModels = []string{
	// Anthropic — bare generation numbers are separator-free.
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-fable-5",
	// Anthropic — every X.Y minor version, including 5.5, is DOTTED.
	"claude-opus-5.5",
	"claude-sonnet-5.5",
	"claude-opus-4.8",
	"claude-opus-4.7",
	"claude-opus-4.6",
	"claude-opus-4.5",
	"claude-sonnet-4.6",
	"claude-sonnet-4.5",
	"claude-haiku-4.5",
	// OpenAI.
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex",
	"gpt-5.2",
	"gpt-5-mini",
	"gpt-4.1",
	"gpt-4o",
	"o3",
	"o4-mini",
	// Google.
	"gemini-3.6-flash",
	"gemini-3.5-flash",
	"gemini-3.1-pro-preview",
	"gemini-2.5-pro",
	"gemini-flash-3.5",
	// Others seen in live Copilot catalogs.
	"grok-4.5",
	"kimi-k3",
	"kimi-k2.7-code",
	"mai-code-1-flash-picker",
}

// copilotModelKey collapses the separator drift ("." vs "-") and case so two
// spellings of the same model compare equal. Used ONLY as a lookup key into
// the known-good list — never as an output form.
func copilotModelKey(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, ".", "-"))
}

// copilotModelByKey maps each drift-collapsed key to its canonical CLI id.
var copilotModelByKey = func() map[string]string {
	m := make(map[string]string, len(copilotCLIAcceptedModels))
	for _, id := range copilotCLIAcceptedModels {
		k := copilotModelKey(id)
		if _, exists := m[k]; !exists {
			m[k] = id
		}
	}
	return m
}()

// CanonicalizeCopilotModel normalizes a Copilot model id to the form the
// copilot CLI's --model flag accepts. Handles separator drift in BOTH
// directions (catalog dotted / CLI dashed, and vice versa):
//
//	claude-fable.5   -> claude-fable-5   (CLI wants dashed for the -5 family)
//	claude-opus-4-6  -> claude-opus-4.6  (CLI wants dotted for the 4.x family)
//	claude-fable-5   -> claude-fable-5   (already canonical — unchanged)
//	gpt-5.5          -> gpt-5.5          (legitimate dot — unchanged)
//	some-future-id   -> some-future-id   (unknown — passed through verbatim)
//
// Idempotent and safe to apply at every layer: discovery (so dropdowns show
// canonical ids), model set (so stored selections are canonical), and launch
// (so an already-stored bad id self-corrects without operator action).
func CanonicalizeCopilotModel(id string) string {
	if id == "" {
		return id
	}
	if canonical, ok := copilotModelByKey[copilotModelKey(id)]; ok {
		return canonical
	}
	return id
}

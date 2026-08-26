package agent

import "strings"

// Copilot model-id nomenclature drift (#4262).
//
// The Copilot models catalog (the raw /models endpoint and the SDK helper
// probe) periodically returns ids in a different nomenclature than the
// copilot CLI's --model flag accepts, with version separators drifting
// between "." and "-" in BOTH directions. Verified live on a spoke: copilot
// CLI v1.0.78 rejected `--model claude-fable.5` ("is not available") and
// switched itself to `claude-fable-5` — the CLI wants the DASHED form for
// the -5 family (claude-fable-5, claude-sonnet-5, claude-opus-5) while
// older families remain DOTTED (claude-opus-4.6, gpt-5.5, gemini-2.5-pro).
//
// Canonicalization is alias-based, never a blind rewrite: an id is matched
// against the known CLI-accepted list with the separator drift collapsed,
// and only an id that differs from a known-good id purely by "." vs "-"
// drift is normalized to that known-good form. Everything else — unknown
// ids, future models, the "auto" sentinel — passes through untouched, so a
// catalog id we have never seen can still be selected and launched verbatim.

// copilotCLIAcceptedModels are the model ids the copilot CLI's --model flag
// accepts, in the CLI's own nomenclature. This is the alias target set for
// CanonicalizeCopilotModel — NOT an allowlist: ids absent from this list are
// passed through unchanged, so it only needs to cover ids whose separator
// spelling is known to drift in the catalog. Note the deliberate mix: the
// -5 family is DASHED, older families are DOTTED — exactly the drift the
// canonicalization exists to absorb.
var copilotCLIAcceptedModels = []string{
	// Anthropic — the -5 family is DASHED in CLI nomenclature.
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-fable-5",
	// Anthropic — the 4.x family is DOTTED.
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

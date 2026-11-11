package crux

import "strings"

// claudeCLIDatedModels are the Claude CLI ids whose canonical spelling ends in
// a release date. They are the only targets the dotted→dashed rewrite below
// cannot derive by swapping separators: the Copilot catalog says
// claude-haiku-4.5, the Claude CLI list says claude-haiku-4-5-20251001. They
// mirror the dated entries of claudePinnedCLIModels in
// pkg/dashboard/cli_models.go; a dashboard test pins every ClaudeLaunchModel
// target to that list so the two cannot drift apart silently.
var claudeCLIDatedModels = []string{
	"claude-opus-4-5-20251101",
	"claude-haiku-4-5-20251001",
	"claude-sonnet-4-5-20250929",
	"claude-opus-4-1-20250805",
}

// claudeUndatedID drops a trailing -YYYYMMDD release date from a Claude id.
func claudeUndatedID(id string) string {
	i := strings.LastIndex(id, "-")
	if i < 0 || len(id)-i-1 != 8 {
		return id
	}
	for _, c := range id[i+1:] {
		if c < '0' || c > '9' {
			return id
		}
	}
	return id[:i]
}

// claudeModelByCopilotKey maps the separator-collapsed key of every DOTTED
// Claude id in copilotCLIAcceptedModels (the Copilot CLI's spelling) to the
// id the Claude CLI accepts for the same model. Derived, not hand-kept: the
// Claude spelling is the Copilot one with dashes for dots, plus the release
// date for the few ids whose Claude CLI spelling carries one.
var claudeModelByCopilotKey = func() map[string]string {
	dated := make(map[string]string, len(claudeCLIDatedModels))
	for _, id := range claudeCLIDatedModels {
		dated[claudeUndatedID(id)] = id
	}
	m := make(map[string]string)
	for _, id := range copilotCLIAcceptedModels {
		if !strings.HasPrefix(id, "claude-") || !strings.Contains(id, ".") {
			continue
		}
		claudeID := strings.ReplaceAll(id, ".", "-")
		if d, ok := dated[claudeID]; ok {
			claudeID = d
		}
		m[copilotModelKey(id)] = claudeID
	}
	return m
}()

// copilotModelByClaudeID is the reverse of claudeModelByCopilotKey: a Claude
// CLI id to the Copilot CLI's spelling of the same model.
var copilotModelByClaudeID = func() map[string]string {
	m := make(map[string]string, len(claudeModelByCopilotKey))
	for _, id := range copilotCLIAcceptedModels {
		if claudeID, ok := claudeModelByCopilotKey[copilotModelKey(id)]; ok {
			m[claudeID] = id
		}
	}
	return m
}()

// ClaudeLaunchModel returns the id to pass to the Claude CLI's --model flag.
// It rewrites only the Copilot CLI's dotted spelling of a Claude model
// (claude-sonnet-5.5, claude-haiku-4.5), which the Claude CLI rejects with
// "There's an issue with the selected model", to the Claude CLI's own id
// (claude-sonnet-5-5, claude-haiku-4-5-20251001). Ids without a dot (the
// Claude CLI's own ids and the opus/sonnet/haiku aliases) and dotted ids it
// does not know pass through verbatim.
//
// Idempotent, so it is safe at every layer: model set (the stored override
// is spelled for the claude backend) and launch (an id stored before this
// existed self-corrects without operator action), mirroring
// CopilotLaunchModel for the copilot backend.
func ClaudeLaunchModel(id string) string {
	if !strings.Contains(id, ".") {
		return id
	}
	if claudeID, ok := claudeModelByCopilotKey[copilotModelKey(id)]; ok {
		return claudeID
	}
	return id
}

// ModelForBackend re-spells a stored model id for backend after an agent's
// backend changes. ok is false when the id cannot be launched on backend and
// has no equivalent there, so the caller should drop it and let the agent
// fall back to its configured or default model.
//
//   - claude: the Copilot dotted spelling becomes the Claude CLI id
//     (ClaudeLaunchModel). A dotted claude- id with no known equivalent, or a
//     non-Claude model from the Copilot catalog (gpt-…, gemini-…), is not
//     launchable on the Claude CLI, so ok is false.
//   - copilot: a Claude CLI id becomes the Copilot spelling, then the
//     Copilot separator canonicalization applies. Unknown ids pass through,
//     as they do at Copilot launch.
//   - any other backend: unchanged.
func ModelForBackend(model, backend string) (string, bool) {
	if model == "" {
		return model, true
	}
	switch backend {
	case "claude":
		out := ClaudeLaunchModel(model)
		if strings.HasPrefix(out, "claude-") && strings.Contains(out, ".") {
			return "", false
		}
		if !strings.HasPrefix(out, "claude-") {
			if _, known := copilotModelByKey[copilotModelKey(out)]; known {
				return "", false
			}
		}
		return out, true
	case "copilot":
		if copilotID, ok := copilotModelByClaudeID[model]; ok {
			return copilotID, true
		}
		return CanonicalizeCopilotModel(model), true
	default:
		return model, true
	}
}

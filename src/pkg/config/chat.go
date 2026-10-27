package config

import "strings"

// ChatConfig (top-level `chat:`) tunes the dashboard's Hive Chat panel.
type ChatConfig struct {
	// GroundedAnswers gates the model-backed answer path for questions the
	// deterministic slash-command handlers cannot answer.
	GroundedAnswers ChatGroundedAnswersConfig `yaml:"grounded_answers,omitempty" json:"grounded_answers,omitempty"`
}

// ChatGroundedAnswersConfig (`chat.grounded_answers:`) gates grounded chat
// answers: a question that is not a bare slash command (`/agents stuck`,
// "which agent completed last?") is answered by one read-only model turn
// over a compact snapshot of the hive state the dashboard already holds,
// instead of the bare command's full dump. Default ON (the same default-on
// `*bool` pattern as decisions.delegate). The call uses the governor
// reviewer endpoint (governor.trajectory / governor.litellm); Model
// overrides the model. When the turn is unavailable the chat answers with
// the command's usage, never with an unrelated dump.
type ChatGroundedAnswersConfig struct {
	// Enabled turns grounded answers on. nil = true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Model overrides the reviewer model for the grounding turn.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
}

// GroundedAnswersEnabled reports whether grounded chat answers are on
// (default on).
func (c ChatConfig) GroundedAnswersEnabled() bool {
	return c.GroundedAnswers.Enabled == nil || *c.GroundedAnswers.Enabled
}

// GroundedAnswersModel is the configured model override, "" when none.
func (c ChatConfig) GroundedAnswersModel() string {
	return strings.TrimSpace(c.GroundedAnswers.Model)
}

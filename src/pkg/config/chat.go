package config

import "strings"

// ChatConfig (top-level `chat:`) tunes the dashboard's Hive Chat panel.
type ChatConfig struct {
	// GroundedAnswers gates the model-backed answer path for questions the
	// deterministic slash-command handlers cannot answer.
	GroundedAnswers ChatGroundedAnswersConfig `yaml:"grounded_answers,omitempty" json:"grounded_answers,omitempty"`
	// ConfigAssist gates model-proposed, owner-confirmed config changes
	// (RFC #19788). Default OFF.
	ConfigAssist ChatConfigAssistConfig `yaml:"config_assist,omitempty" json:"config_assist,omitempty"`
}

// ChatConfigAssistConfig (`chat.config_assist:`) is the master switch for
// Hive Chat config assist, plus the model its proposal turn runs on.
type ChatConfigAssistConfig struct {
	// Enabled turns config assist on. nil = false: the opposite default of
	// grounded_answers, so the feature is opt-in.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Model overrides the model for the proposal turn. Empty falls back to
	// grounded_answers.model, then the reviewer model (RFC #19788 section 1).
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
}

// ConfigAssistEnabled reports whether chat config assist is on. It is true
// only when config_assist.enabled is explicitly true.
func (c ChatConfig) ConfigAssistEnabled() bool {
	return c.ConfigAssist.Enabled != nil && *c.ConfigAssist.Enabled
}

// ConfigAssistModel is the model override for the config assist proposal
// turn: config_assist.model, else grounded_answers.model, "" when neither is
// set (the reviewer model then applies).
func (c ChatConfig) ConfigAssistModel() string {
	if m := strings.TrimSpace(c.ConfigAssist.Model); m != "" {
		return m
	}
	return c.GroundedAnswersModel()
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

package dashboard

import (
	"os"

	"github.com/hivecommons/hive/pkg/config"
)

// reviewBotsSection exposes effective settings, not just the hive.yaml mirror:
// deployed hives commonly configure this trust boundary in hive-project.yaml.
// This is deliberately read-only; adding a login grants thread-resolution rights.
type reviewBotsSection struct {
	Logins               []string `json:"logins"`
	MaxAttemptsPerThread int      `json:"max_attempts_per_thread"`
	ResolveAfterFix      bool     `json:"resolve_after_fix"`
	Enabled              bool     `json:"enabled"`
	LoadError            string   `json:"load_error,omitempty"`
}

func reviewBotsSectionResponse(cfg *config.Config) reviewBotsSection {
	rb, err := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	section := reviewBotsSection{
		Logins:               append([]string{}, rb.Logins...),
		MaxAttemptsPerThread: rb.MaxAttempts(),
		ResolveAfterFix:      rb.ResolveAfterFixEnabled(),
		Enabled:              rb.Enabled(),
	}
	if err != nil {
		section.LoadError = err.Error()
	}
	return section
}

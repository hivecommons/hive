package dashboard

import (
	"fmt"
	"os"

	"github.com/hivecommons/hive/pkg/config"
)

// reviewBotsSection exposes effective settings, not just the hive.yaml mirror:
// deployed hives commonly configure this trust boundary in hive-project.yaml.
// Logins are deliberately read-only; adding one grants thread-resolution
// rights. MinPriority is the one editable field (PUT /api/config/review,
// review_bots.min_priority): it only narrows what is routed to agents.
// MinPriority is the effective threshold, "P0"-"P3", or "" when every finding
// is routed (unset, "all", or an unrecognised value).
type reviewBotsSection struct {
	Logins               []string `json:"logins"`
	MinPriority          string   `json:"min_priority"`
	MaxAttemptsPerThread int      `json:"max_attempts_per_thread"`
	ResolveAfterFix      bool     `json:"resolve_after_fix"`
	Enabled              bool     `json:"enabled"`
	LoadError            string   `json:"load_error,omitempty"`
}

func reviewBotsSectionResponse(cfg *config.Config) reviewBotsSection {
	rb, err := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	section := reviewBotsSection{
		Logins:               append([]string{}, rb.Logins...),
		MinPriority:          reviewBotsMinPriorityLabel(rb),
		MaxAttemptsPerThread: rb.MaxAttempts(),
		ResolveAfterFix:      rb.ResolveAfterFixEnabled(),
		Enabled:              rb.Enabled(),
	}
	if err != nil {
		section.LoadError = err.Error()
	}
	return section
}

func reviewBotsMinPriorityLabel(rb config.ReviewBotsConfig) string {
	if t := rb.PriorityThreshold(); t >= 0 {
		return fmt.Sprintf("P%d", t)
	}
	return ""
}

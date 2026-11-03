package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/firsttimer"
)

// FirstTimerWelcomeConfig (`governor.first_timer_welcome:`) is the one-time
// welcome the hive posts on a first-time contributor's pull request: a
// thank-you plus an invitation to run their own hive (a clanker) and let it
// do the contribution loop, with the concrete advantages — contributor
// profile, leaderboard place, trust-tier progression that unlocks
// auto-merge, review fixes and rebases handled while they are offline.
//
// Default ON, mirroring good_first_issue: the comment is advertising, posted
// at most once per author per repository, only for HUMAN authors GitHub
// reports as FIRST_TIME_CONTRIBUTOR / FIRST_TIMER (never bots, the hive's own
// PRs, hive-attributed PRs, `author/clanker` PRs or registered relay
// contributors), and it is silent until the hive has a public dashboard URL
// to link. `enabled: false` opts out.
type FirstTimerWelcomeConfig struct {
	// Enabled turns the welcome on. Pointer so an omitted `enabled:` key
	// (nil) is distinguishable from an explicit false: nil defaults ON.
	// Read via IsEnabled(), never dereferenced raw.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TemplatePath is an optional file holding a text/template over
	// firsttimer.Vars that replaces firsttimer.DefaultTemplate. It must
	// contain {{ .Marker }}.
	TemplatePath string `yaml:"template_path,omitempty" json:"template_path,omitempty"`
}

// IsEnabled reports whether the welcome runs: true unless the operator set
// `enabled: false`.
func (f FirstTimerWelcomeConfig) IsEnabled() bool {
	return f.Enabled == nil || *f.Enabled
}

// TemplateText returns the operator's override template, or "" (meaning
// firsttimer.DefaultTemplate) when template_path is unset.
func (f FirstTimerWelcomeConfig) TemplateText() (string, error) {
	path := strings.TrimSpace(f.TemplatePath)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("governor.first_timer_welcome.template_path: %w", err)
	}
	return string(data), nil
}

// ValidateFirstTimerWelcome rejects a template_path that cannot be read or
// whose template would not parse or lacks the marker (which would make the
// sweep post a new comment every pass).
func ValidateFirstTimerWelcome(f FirstTimerWelcomeConfig) error {
	text, err := f.TemplateText()
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if _, err := firsttimer.ParseTemplate(text); err != nil {
		return fmt.Errorf("governor.first_timer_welcome.template_path: %w", err)
	}
	return nil
}

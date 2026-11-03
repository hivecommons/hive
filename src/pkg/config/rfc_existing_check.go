package config

import (
	"strings"
	"time"
)

const (
	// DefaultRFCExistingCheckMaxMatches caps how many settings one
	// existing-coverage comment lists.
	DefaultRFCExistingCheckMaxMatches = 5
	// DefaultRFCExistingCheckMaxAge is how old an issue may be and still get
	// the comment, so turning the check on does not comment on the backlog.
	DefaultRFCExistingCheckMaxAge = 7 * 24 * time.Hour
)

// DefaultRFCExistingCheckLabels are the labels that mark an issue as an
// RFC or feature request.
var DefaultRFCExistingCheckLabels = []string{"rfc", "enhancement", "kind/feature"}

// RFCExistingCheckConfig gates the triage-stage existing-coverage comment
// (hivecommons/hive#13575): new RFC/feature issues are matched by keyword
// against the dashboard's Settings search index and get one comment that
// lists the settings that may already cover the request, each with a link
// that opens Settings with the search pre-filled. It only reads and
// comments; it never labels, closes or decides anything.
type RFCExistingCheckConfig struct {
	// Enabled turns the check on. Default off.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// PostWhenNothingFound also posts a one-time "nothing found" comment
	// when no setting matches. Default off: no match, no comment.
	PostWhenNothingFound bool `yaml:"post_when_nothing_found,omitempty" json:"post_when_nothing_found,omitempty"`
	// Labels select the issues to check (case-insensitive). Empty means
	// DefaultRFCExistingCheckLabels.
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// MaxMatches caps the settings listed. Non-positive means
	// DefaultRFCExistingCheckMaxMatches.
	MaxMatches int `yaml:"max_matches,omitempty" json:"max_matches,omitempty"`
	// MaxAge skips issues opened longer ago than this. Non-positive means
	// DefaultRFCExistingCheckMaxAge.
	MaxAge time.Duration `yaml:"max_age,omitempty" json:"max_age,omitempty"`
}

// EffectiveLabels returns the configured labels, lowercased, or the defaults.
func (c RFCExistingCheckConfig) EffectiveLabels() []string {
	var out []string
	for _, l := range c.Labels {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultRFCExistingCheckLabels...)
	}
	return out
}

// EffectiveMaxMatches returns MaxMatches or the default.
func (c RFCExistingCheckConfig) EffectiveMaxMatches() int {
	if c.MaxMatches <= 0 {
		return DefaultRFCExistingCheckMaxMatches
	}
	return c.MaxMatches
}

// EffectiveMaxAge returns MaxAge or the default.
func (c RFCExistingCheckConfig) EffectiveMaxAge() time.Duration {
	if c.MaxAge <= 0 {
		return DefaultRFCExistingCheckMaxAge
	}
	return c.MaxAge
}

package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Question auto-close (hivecommons/hive#9584) defaults and environment
// overrides. The feature is OFF unless governor.question_autoclose.enabled or
// HIVE_QUESTION_AUTOCLOSE turns it on.
const (
	// QuestionAutocloseEnvVar overrides governor.question_autoclose.enabled
	// when set to a value strconv.ParseBool accepts. Anything else is ignored
	// and the config value stands.
	QuestionAutocloseEnvVar = "HIVE_QUESTION_AUTOCLOSE"
	// QuestionAutocloseHoursEnvVar overrides governor.question_autoclose.hours
	// when set to a positive integer.
	QuestionAutocloseHoursEnvVar = "HIVE_QUESTION_AUTOCLOSE_HOURS"
	// DefaultQuestionAutocloseHours is how long an answered question stays
	// open for the author to object before it is closed.
	DefaultQuestionAutocloseHours = 4
	// DefaultQuestionAutocloseHumanLabel is the label an objected-to answer
	// hands the issue over with.
	DefaultQuestionAutocloseHumanLabel = "needs-human"
)

// defaultQuestionAutocloseLabels are the labels that mark an issue as a
// question when governor.question_autoclose.labels is unset.
var defaultQuestionAutocloseLabels = []string{"question", "kind/question"}

// QuestionAutocloseConfig is `governor.question_autoclose` (#9584): once the
// hive has answered a question issue, close it after a window unless the
// issue author reacts 👎 to the answer.
//
// Off by default. With Enabled false (and HIVE_QUESTION_AUTOCLOSE unset) the
// hive schedules nothing, closes nothing and adds nothing to any kick.
type QuestionAutocloseConfig struct {
	// Enabled turns the feature on.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Hours is the objection window after the answer comment. <= 0 means
	// DefaultQuestionAutocloseHours.
	Hours int `yaml:"hours,omitempty" json:"hours,omitempty"`
	// Labels mark an issue as a question. Empty means the defaults
	// ("question", "kind/question"). Matching is case-insensitive.
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// HumanLabel is added when the author objects. Empty means
	// DefaultQuestionAutocloseHumanLabel.
	HumanLabel string `yaml:"human_label,omitempty" json:"human_label,omitempty"`
}

// questionAutocloseLookup is the environment reader; tests swap it through
// the *With variants rather than mutating the process environment.
type questionAutocloseLookup func(string) (string, bool)

// IsEnabled reports whether the feature is on, honouring
// HIVE_QUESTION_AUTOCLOSE over the config value.
func (q QuestionAutocloseConfig) IsEnabled() bool { return q.isEnabledWith(os.LookupEnv) }

func (q QuestionAutocloseConfig) isEnabledWith(lookup questionAutocloseLookup) bool {
	if v, ok := lookup(QuestionAutocloseEnvVar); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return q.Enabled
}

// EffectiveWindow returns the objection window, honouring
// HIVE_QUESTION_AUTOCLOSE_HOURS over the config value.
func (q QuestionAutocloseConfig) EffectiveWindow() time.Duration {
	return q.effectiveWindowWith(os.LookupEnv)
}

func (q QuestionAutocloseConfig) effectiveWindowWith(lookup questionAutocloseLookup) time.Duration {
	hours := q.Hours
	if v, ok := lookup(QuestionAutocloseHoursEnvVar); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			hours = n
		}
	}
	if hours <= 0 {
		hours = DefaultQuestionAutocloseHours
	}
	return time.Duration(hours) * time.Hour
}

// EffectiveLabels returns the configured question labels, or a copy of the
// defaults.
func (q QuestionAutocloseConfig) EffectiveLabels() []string {
	out := make([]string, 0, len(q.Labels))
	for _, l := range q.Labels {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	if len(out) > 0 {
		return out
	}
	return append([]string(nil), defaultQuestionAutocloseLabels...)
}

// EffectiveHumanLabel returns the label added when the author objects.
func (q QuestionAutocloseConfig) EffectiveHumanLabel() string {
	if l := strings.TrimSpace(q.HumanLabel); l != "" {
		return l
	}
	return DefaultQuestionAutocloseHumanLabel
}

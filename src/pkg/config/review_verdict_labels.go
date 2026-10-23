package config

import (
	"fmt"
	"strings"
)

// maxVerdictLabelLen is GitHub's label name limit.
const maxVerdictLabelLen = 50

// ReviewVerdictLabelsConfig is review.verdict_labels (hivecommons/hive#13833):
// the label each review state maps to. Every key is optional; an empty key
// means that state carries no label, and an all-empty block turns the feature
// off.
type ReviewVerdictLabelsConfig struct {
	// Reviewing is applied while the current head's review is pending.
	Reviewing string `yaml:"reviewing,omitempty" json:"reviewing,omitempty"`
	// Approve is applied when the head's aggregate verdict is approve.
	Approve string `yaml:"approve,omitempty" json:"approve,omitempty"`
	// ChangesRequested is applied when the aggregate is changes_requested.
	ChangesRequested string `yaml:"changes_requested,omitempty" json:"changes_requested,omitempty"`
	// RequiresHuman is applied when the aggregate is requires_human or
	// reject (a recommended close is a person's call on a comment-only hive).
	RequiresHuman string `yaml:"requires_human,omitempty" json:"requires_human,omitempty"`
}

// Enabled reports whether any state maps to a label.
func (v ReviewVerdictLabelsConfig) Enabled() bool {
	return len(v.Labels()) > 0
}

// Labels returns the configured (non-empty, trimmed) label names.
func (v ReviewVerdictLabelsConfig) Labels() []string {
	var out []string
	for _, l := range []string{v.Reviewing, v.Approve, v.ChangesRequested, v.RequiresHuman} {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Validate checks every configured name is a usable, distinct label name:
// one label per state is what lets the governor remove the stale one.
func (v ReviewVerdictLabelsConfig) Validate() error {
	seen := map[string]string{}
	for _, f := range []struct{ key, val string }{
		{"reviewing", v.Reviewing},
		{"approve", v.Approve},
		{"changes_requested", v.ChangesRequested},
		{"requires_human", v.RequiresHuman},
	} {
		name := strings.TrimSpace(f.val)
		if name == "" {
			continue
		}
		if len(name) > maxVerdictLabelLen {
			return fmt.Errorf("review.verdict_labels.%s: label %q is longer than %d characters", f.key, name, maxVerdictLabelLen)
		}
		if strings.Contains(name, ",") {
			return fmt.Errorf("review.verdict_labels.%s: label %q must not contain a comma", f.key, name)
		}
		lower := strings.ToLower(name)
		if prev, dup := seen[lower]; dup {
			return fmt.Errorf("review.verdict_labels.%s: label %q is also used for %s; each state needs its own label", f.key, name, prev)
		}
		seen[lower] = f.key
	}
	return nil
}

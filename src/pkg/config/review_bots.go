package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultProjectYAMLPath is where the deterministic pipeline's project file
// lives on a deployed hive (bin/hive-config.sh and the pre-kick stages read
// the same path; the image copies examples/kubestellar/hive-project.yaml
// there). The Go binary reads ONE key out of it — classification.review_bots —
// so the review-thread reconciler (hivecommons/hive#7360) is configured in the
// same place as its sibling monitor, classification.copilot_check.
const DefaultProjectYAMLPath = "/etc/hive/hive-project.yaml"

// ClassificationConfig is the hive.yaml mirror of hive-project.yaml's
// `classification:` block. Only the keys the Go binary consumes are declared
// here; the rest of that block (complexity tiers, lanes, clustering,
// copilot_check) is read by the bash pipeline directly and stays out of the
// Go schema on purpose, so a Save() of hive.yaml can never round-trip and
// truncate it. Declaring the same key path in both files means one YAML
// snippet works wherever the operator chooses to put it.
type ClassificationConfig struct {
	// ReviewBots names the external review bots whose unresolved inline
	// threads on hive-authored PRs the hive addresses and resolves itself.
	ReviewBots ReviewBotsConfig `yaml:"review_bots,omitempty" json:"review_bots,omitempty"`
}

// ReviewBotsConfig is `classification.review_bots` (hivecommons/hive#7360).
//
// When a hive agent opens a PR on a repo with an external review bot
// installed (Copilot code review, chatgpt-codex-connector, CodeRabbit, …),
// the bot leaves inline review threads a few minutes later. On repos with
// "require conversation resolution before merging" the PR cannot merge until
// every thread is resolved, and nothing in the hive used to notice. This block
// turns that reconciliation on: the review-thread monitor lists unresolved
// threads whose FIRST comment is from one of Logins, the kick builder routes
// each PR back to the agent that opened it, and the review-request watcher
// lets that agent reply in-thread and resolve the thread — and nothing else.
//
// Semantics:
//   - Absent / empty Logins: the feature is OFF. The monitor writes an empty
//     report and the watcher denies every resolve_thread request, so an
//     agent can never resolve anything on a hive that has not opted in.
//   - Logins are matched case-insensitively against the thread's first
//     comment author, ignoring whitespace and a trailing "[bot]" suffix
//     (REST/web include it; GraphQL omits it). A human login listed here would let
//     agents resolve that human's threads; do not do that.
//   - MaxAttemptsPerThread bounds how many times the hive replies in one
//     thread before leaving it for a human. The counter IS the thread's
//     reply list (replies authored by the App bot) — no extra state file.
//   - ResolveAfterFix decides whether the kick tells the agent to resolve the
//     thread after replying (true, the default) or to leave it open for a
//     human to close.
type ReviewBotsConfig struct {
	Logins []string `yaml:"logins,omitempty" json:"logins,omitempty"`
	// MinPriority is P0-P3, with P0 most urgent. Empty or unrecognised
	// values preserve routing of every finding; unknown badges always pass.
	MinPriority string `yaml:"min_priority,omitempty" json:"min_priority,omitempty"`
	// MaxAttemptsPerThread defaults to 1 when unset or non-positive.
	MaxAttemptsPerThread int `yaml:"max_attempts_per_thread,omitempty" json:"max_attempts_per_thread,omitempty"`
	// ResolveAfterFix is a *bool so "unset" (default true) is distinguishable
	// from an explicit false.
	ResolveAfterFix *bool `yaml:"resolve_after_fix,omitempty" json:"resolve_after_fix,omitempty"`
}

// ReviewBotPriorityPattern recognises Codex's Markdown priority badge.
// Keep the reviewer jq predicate on this same pattern and first-match rule.
const ReviewBotPriorityPattern = `!\[P([0-3]) Badge\]`

var reviewBotPriorityRE = regexp.MustCompile(ReviewBotPriorityPattern)

// PriorityThreshold returns the largest numeric priority to route, or -1
// when no recognised threshold is configured (fail open on typos).
func (r ReviewBotsConfig) PriorityThreshold() int {
	p := strings.ToUpper(strings.TrimSpace(r.MinPriority))
	if len(p) == 2 && p[0] == 'P' && p[1] >= '0' && p[1] <= '3' {
		return int(p[1] - '0')
	}
	return -1
}

// IncludesPriority keeps unknown formats so a new bot cannot silently hide
// findings. Only the first comment's first recognised badge sets priority.
func (r ReviewBotsConfig) IncludesPriority(body string) bool {
	threshold := r.PriorityThreshold()
	badge := reviewBotPriorityRE.FindStringSubmatch(body)
	return threshold < 0 || badge == nil || int(badge[1][0]-'0') <= threshold
}

// DefaultReviewBotMaxAttempts is the per-thread reply budget when
// max_attempts_per_thread is unset.
const DefaultReviewBotMaxAttempts = 1

// Enabled reports whether at least one bot login is configured. Everything
// downstream keys off this: disabled means no GraphQL calls, an empty report,
// and every resolve_thread request denied.
func (r ReviewBotsConfig) Enabled() bool {
	for _, l := range r.Logins {
		if strings.TrimSpace(l) != "" {
			return true
		}
	}
	return false
}

// IsBot reports whether login is one of the configured review-bot logins
// (case-insensitive, whitespace-trimmed, ignoring a trailing "[bot]" suffix).
// An empty normalized login never matches.
func (r ReviewBotsConfig) IsBot(login string) bool {
	login = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
	if login == "" {
		return false
	}
	for _, l := range r.Logins {
		if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(l)), "[bot]") == login {
			return true
		}
	}
	return false
}

// MaxAttempts returns max_attempts_per_thread with the default applied.
func (r ReviewBotsConfig) MaxAttempts() int {
	if r.MaxAttemptsPerThread <= 0 {
		return DefaultReviewBotMaxAttempts
	}
	return r.MaxAttemptsPerThread
}

// ResolveAfterFixEnabled returns resolve_after_fix with the default (true)
// applied.
func (r ReviewBotsConfig) ResolveAfterFixEnabled() bool {
	if r.ResolveAfterFix == nil {
		return true
	}
	return *r.ResolveAfterFix
}

// LoadProjectReviewBots reads ONLY `classification.review_bots` out of a
// hive-project.yaml. A missing file is not an error — it is the common case
// on a hive that configures the block in hive.yaml instead (or not at all)
// and yields the zero value, i.e. the feature off. A file that exists but
// does not parse IS an error so a typo does not silently disable the
// reconciler.
func LoadProjectReviewBots(path string) (ReviewBotsConfig, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultProjectYAMLPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReviewBotsConfig{}, nil
		}
		return ReviewBotsConfig{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc struct {
		Classification ClassificationConfig `yaml:"classification"`
	}
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &doc); err != nil {
		return ReviewBotsConfig{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return doc.Classification.ReviewBots, nil
}

// EffectiveReviewBots returns the review-bot block the running hive should
// use: hive.yaml's classification.review_bots when it names any login,
// otherwise the same key read from hive-project.yaml at projectPath ("" =
// DefaultProjectYAMLPath). hive.yaml wins because it is the file the Go
// binary already owns and reloads; the project file is the fallback so the
// block can live next to its sibling monitor's config as #7360 specifies.
// A project file that fails to parse yields the zero value AND the error, so
// the caller can log it — the feature stays off rather than half-configured.
//
// One field crosses that boundary: a hive.yaml min_priority overrides the
// project file's even when hive.yaml names no login. The dashboard edits it
// there (hivecommons/hive#10481) without copying the project file's logins —
// the trust grant — into hive.yaml. "all" is stored for an explicit
// route-everything choice, since empty means "no override".
func (c *Config) EffectiveReviewBots(projectPath string) (ReviewBotsConfig, error) {
	if c != nil && c.Classification.ReviewBots.Enabled() {
		return c.Classification.ReviewBots, nil
	}
	rb, err := LoadProjectReviewBots(projectPath)
	if err == nil && c != nil {
		if p := strings.TrimSpace(c.Classification.ReviewBots.MinPriority); p != "" {
			rb.MinPriority = p
		}
	}
	return rb, err
}

// ReviewBotsMinPriorityAll is the explicit "route every finding" value for
// min_priority. PriorityThreshold treats it like unset (-1); it exists so a
// hive.yaml override can undo a project-file threshold.
const ReviewBotsMinPriorityAll = "all"

// NormalizeReviewBotsMinPriority canonicalises a min_priority value written
// through the dashboard: "" (no override), "all", or P0-P3 (case and
// surrounding whitespace ignored). ok is false for anything else so a typo is
// refused at write time instead of silently routing every finding.
func NormalizeReviewBotsMinPriority(v string) (string, bool) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", true
	case strings.EqualFold(v, ReviewBotsMinPriorityAll):
		return ReviewBotsMinPriorityAll, true
	}
	if t := (ReviewBotsConfig{MinPriority: v}).PriorityThreshold(); t >= 0 {
		return fmt.Sprintf("P%d", t), true
	}
	return "", false
}

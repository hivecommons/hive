package automerge

import (
	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// autoMergeReasonHumanMergeRequired is the one skip reason every lane and
// the merge train report for a PR a person must merge: a breaking-change
// title (⚠️ or an auto_merge.human_merge_title_prefixes prefix) or an
// auto_merge.human_merge_labels label (hivecommons/hive#15442). The
// classifier parks such a PR at 090 Ready for human merge with the
// manual-merge rule; the train never admits or fronts it.
const autoMergeReasonHumanMergeRequired = prstage.AutoMergeReasonHumanMergeRequired

// SetHumanMergeRule installs (or, with nil, resets to the defaults) the
// live human-merge rule. fn is read on every evaluation so a config reload
// applies without a restart.
func (c *Engine) SetHumanMergeRule(fn func() prstage.HumanMergeRule) {
	if c == nil {
		return
	}
	c.humanMergeRuleMu.Lock()
	defer c.humanMergeRuleMu.Unlock()
	c.humanMergeRule = fn
}

func (c *Engine) currentHumanMergeRule() prstage.HumanMergeRule {
	if c == nil {
		return prstage.HumanMergeRule{}
	}
	c.humanMergeRuleMu.RLock()
	fn := c.humanMergeRule
	c.humanMergeRuleMu.RUnlock()
	if fn == nil {
		return prstage.HumanMergeRule{}
	}
	return fn()
}

// RequiresHumanMerge is the single predicate behind the rule "breaking or
// owner-flagged PRs never auto-merge": true when the PR's trimmed title
// starts with a configured prefix (default ⚠️ / ⚠) or the PR wears a
// configured label (default needs-owner-merge, case-insensitive). A nil
// engine applies the defaults.
func (c *Engine) RequiresHumanMerge(pr *gh.PullRequest) bool {
	if pr == nil {
		return false
	}
	ok, _ := c.currentHumanMergeRule().RequiresHumanMerge(pr.GetTitle(), labelNames(pr.Labels))
	return ok
}

// RequiresHumanMerge applies the default rule to a PR; the engine method
// applies the configured one.
func RequiresHumanMerge(pr *gh.PullRequest) bool {
	return (*Engine)(nil).RequiresHumanMerge(pr)
}

// humanMergeRequiredReason is the lane prefilters' one shared check: the
// skip reason for a PR RequiresHumanMerge, "" otherwise. The queued issue
// form of a PR (the merger-queue lane lists issues) goes through the same
// rule.
func (c *Engine) humanMergeRequiredReason(title string, labels []string) string {
	if ok, _ := c.currentHumanMergeRule().RequiresHumanMerge(title, labels); ok {
		return autoMergeReasonHumanMergeRequired
	}
	return ""
}

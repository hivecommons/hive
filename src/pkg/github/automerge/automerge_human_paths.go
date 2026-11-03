package automerge

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// Skip reasons reported by the human-merge-path gate (#11038).
const (
	// autoMergeReasonHumanMergePath: the PR touches a path listed in
	// auto_merge.human_merge_paths for its repo; it is held for a person.
	autoMergeReasonHumanMergePath = prstage.AutoMergeReasonHumanMergePath
	// autoMergeReasonHumanMergePathEvidence: the repo has human-merge paths
	// but the complete changed-file list could not be fetched. Fails CLOSED.
	autoMergeReasonHumanMergePathEvidence = prstage.AutoMergeReasonHumanMergePathEvidence
)

// humanMergePinOnlyConfigKey is the config key named in pin-only log lines.
const humanMergePinOnlyConfigKey = "auto_merge.human_merge_pin_only"

// SetHumanMergePaths installs (or, with nil, removes) the per-repo
// auto_merge.human_merge_paths lookup every sweep lane consults. fn is called
// on every evaluation with "owner/repo", so a config reload takes effect
// without restarting the sweep.
func (c *Engine) SetHumanMergePaths(fn func(repo string) []string) {
	if c == nil {
		return
	}
	c.humanMergePathsMu.Lock()
	defer c.humanMergePathsMu.Unlock()
	c.humanMergePaths = fn
}

func (c *Engine) currentHumanMergePaths(repo string) []string {
	if c == nil {
		return nil
	}
	c.humanMergePathsMu.RLock()
	fn := c.humanMergePaths
	c.humanMergePathsMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(repo)
}

// SetHumanMergePinOnly installs (or, with nil, removes) the per-repo
// auto_merge.human_merge_pin_only lookup (#16266). Like SetHumanMergePaths, fn
// is called on every evaluation so a config reload applies without restart.
func (c *Engine) SetHumanMergePinOnly(fn func(repo string) (config.PinOnlyException, bool)) {
	if c == nil {
		return
	}
	c.humanMergePinOnlyMu.Lock()
	defer c.humanMergePinOnlyMu.Unlock()
	c.humanMergePinOnly = fn
}

func (c *Engine) currentHumanMergePinOnly(repo string) (config.PinOnlyException, bool) {
	if c == nil {
		return config.PinOnlyException{}, false
	}
	c.humanMergePinOnlyMu.RLock()
	fn := c.humanMergePinOnly
	c.humanMergePinOnlyMu.RUnlock()
	if fn == nil {
		return config.PinOnlyException{}, false
	}
	return fn(repo)
}

// humanMergePinOnlyExempt reports whether the opt-in pin-only exception
// waives the human-merge-path matches of pr. files is the complete list the
// caller already fetched for the path check, so a PR the classifier refuses
// costs no further API call. It returns (false, "") when the repo has no
// enabled exception, and (false, reason) when the exception is configured but
// this PR does not qualify.
//
// The classifier receives the FULL changed-file list, so a pin bump that also
// edits any other file (even an unguarded changelog or doc) is not pin-only
// and stays held (#16326). Once the diff is accepted, every commit behind the
// head must itself be provably the trusted author's: the PR author login only
// says who opened the PR, and any principal with write access can push onto
// that branch afterwards. Finally each new action SHA must resolve in its
// upstream repository, not merely somewhere in its fork network. The sweep
// lanes and /approve share this helper so they never disagree.
func (c *Engine) humanMergePinOnlyExempt(ctx context.Context, owner, repo string, pr *gh.PullRequest, files []intent.ChangedFile, matches []string) (bool, string) {
	if len(matches) == 0 || pr == nil {
		return false, ""
	}
	policy, ok := c.currentHumanMergePinOnly(owner + "/" + repo)
	if !ok {
		return false, ""
	}
	author := strings.TrimSpace(hgithub.SafeGetLogin(pr.GetUser()))
	if !pinOnlyTrustedLogin(policy, author) {
		return false, "PR author is not in trusted_authors"
	}
	// ListChangedFiles already refuses a list shorter than GitHub's count;
	// the exception additionally requires that count to be present and exact.
	if reported := pr.GetChangedFiles(); reported <= 0 || reported != len(files) {
		return false, "complete changed-file list not proven"
	}
	ok, why, pins := intent.ClassifyPinOnlyActions(files, matches, policy.Spec())
	if !ok {
		return false, why
	}
	if why := c.pinOnlyCommitsTrusted(ctx, owner, repo, pr, policy); why != "" {
		return false, why
	}
	if why := c.pinOnlyActionPinsUpstream(ctx, pins); why != "" {
		return false, why
	}
	return true, "all changes are approved pin replacements by verified trusted-author commits, anchored upstream"
}

// pinOnlyActionPinsUpstream, pinOnlyCommitsTrusted, pinOnlyTrustedLogin and
// pinOnlyTrustedCommitter delegate to the pkg/github helpers the merge-request
// relay also uses (#16265), so the sweep, /approve and the relay apply one
// provenance policy (#16399).
func (c *Engine) pinOnlyActionPinsUpstream(ctx context.Context, pins []intent.ActionPin) string {
	return hgithub.PinOnlyActionPinsUpstream(ctx, c.gh, pins)
}

func (c *Engine) pinOnlyCommitsTrusted(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy config.PinOnlyException) string {
	return hgithub.PinOnlyCommitsTrusted(ctx, c.gh, owner, repo, pr, policy.TrustedAuthors)
}

func pinOnlyTrustedLogin(policy config.PinOnlyException, login string) bool {
	return hgithub.PinOnlyTrustedLogin(policy.TrustedAuthors, login)
}

func pinOnlyTrustedCommitter(policy config.PinOnlyException, login string) bool {
	return hgithub.PinOnlyTrustedCommitter(policy.TrustedAuthors, login)
}

// humanMergePathGate refuses a PR that touches a path a person must merge.
// It is independent of intent tier and of IntentGate/intent.enforce: the App
// self-merge contract (EvaluateForAppSelfMerge) authorizes Tier 2/3 without a
// person by design (#6258), so this list is the operator's only way to keep a
// path out of every App merge lane.
//
// It returns ("", nil) when the merge may proceed. An unconfigured repo costs
// no API call. When the repo is configured and the complete changed-file list
// cannot be fetched it fails closed with a non-nil error. On a match it adds
// `hold`, posts the single marker-stamped comment (not reposted on later
// ticks) and returns autoMergeReasonHumanMergePath, unless the repo's enabled
// auto_merge.human_merge_pin_only exception qualifies the PR (trusted author,
// complete list, every changed line a pin replacement, every commit a
// verified trusted-author commit, every action SHA anchored in its upstream
// repository) and returns ("", nil).
func (c *Engine) humanMergePathGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) (string, error) {
	patterns := c.currentHumanMergePaths(owner + "/" + repo)
	if len(patterns) == 0 {
		return "", nil
	}
	number := pr.GetNumber()
	files, err := ListChangedFiles(ctx, c.gh, owner, repo, pr)
	if err != nil {
		return autoMergeReasonHumanMergePathEvidence, err
	}
	matches := intent.HumanMergePathMatches(files, patterns)
	if len(matches) == 0 {
		return "", nil
	}
	exempt, why := c.humanMergePinOnlyExempt(ctx, owner, repo, pr, files, matches)
	if exempt {
		c.info("automerge sweep admitted pin-only change on human-merge paths", "repo", displayRepo, "pr", number, "paths", matches, "config", humanMergePinOnlyConfigKey, "reason", why)
		return "", nil
	}
	if why != "" {
		c.info("automerge sweep pin-only exception not applied", "repo", displayRepo, "pr", number, "config", humanMergePinOnlyConfigKey, "reason", why)
	}
	c.info("automerge sweep held PR touching human-merge paths", "repo", displayRepo, "pr", number, "paths", matches, "config", hgithub.HumanMergePathsConfigKey)
	if err := hgithub.HoldForHumanMergePaths(ctx, c.gh, owner, repo, number, matches, pr.GetHead().GetSHA()); err != nil {
		return autoMergeReasonHumanMergePath, err
	}
	return autoMergeReasonHumanMergePath, nil
}

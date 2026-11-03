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
// waives the human-merge-path matches of pr. It needs no API call: files is
// the complete list the caller already fetched for the path check. It returns
// (false, "") when the repo has no enabled exception, and (false, reason) when
// the exception is configured but this PR does not qualify.
//
// The classifier receives the FULL changed-file list, so a pin bump that also
// edits any other file (even an unguarded changelog or doc) is not pin-only
// and stays held (#16326). The sweep lanes and /approve share this helper so
// they never disagree.
func (c *Engine) humanMergePinOnlyExempt(ctx context.Context, owner, repo string, pr *gh.PullRequest, files []intent.ChangedFile, matches []string) (bool, string) {
	if len(matches) == 0 || pr == nil {
		return false, ""
	}
	policy, ok := c.currentHumanMergePinOnly(owner + "/" + repo)
	if !ok {
		return false, ""
	}
	author := strings.TrimSpace(hgithub.SafeGetLogin(pr.GetUser()))
	trusted := false
	for _, login := range policy.TrustedAuthors {
		if author != "" && strings.EqualFold(strings.TrimSpace(login), author) {
			trusted = true
			break
		}
	}
	if !trusted {
		return false, "PR author is not in trusted_authors"
	}
	// ListChangedFiles already refuses a list shorter than GitHub's count;
	// the exception additionally requires that count to be present and exact.
	if reported := pr.GetChangedFiles(); reported <= 0 || reported != len(files) {
		return false, "complete changed-file list not proven"
	}
	if why := c.pinOnlyCommitsUntrusted(ctx, owner, repo, pr, policy.TrustedAuthors); why != "" {
		return false, why
	}
	return intent.ClassifyPinOnly(files, matches, policy.Spec())
}

// pinOnlyCommitsUntrusted proves the pushed commits, not just the PR opener,
// are the trusted author's: anyone with push access to the head branch could
// add a commit under a trusted bot's PR. Every commit's author and committer
// login must be trusted and the list must be complete; it returns a reason
// when that is not proven (including on a listing error, failing closed).
func (c *Engine) pinOnlyCommitsUntrusted(ctx context.Context, owner, repo string, pr *gh.PullRequest, trustedAuthors []string) string {
	trusted := func(u *gh.User) bool {
		login := strings.TrimSpace(hgithub.SafeGetLogin(u))
		if login == "" {
			return false
		}
		for _, t := range trustedAuthors {
			if strings.EqualFold(strings.TrimSpace(t), login) {
				return true
			}
		}
		return false
	}
	opts := &gh.ListOptions{PerPage: 100}
	total := 0
	for page := 0; ; page++ {
		if page >= stageDCOMaxPages {
			return "complete commit list not proven"
		}
		commits, resp, err := c.gh.PullRequests.ListCommits(ctx, owner, repo, pr.GetNumber(), opts)
		if err != nil {
			return "commit list unavailable"
		}
		for _, rc := range commits {
			if rc == nil || !trusted(rc.Author) || !trusted(rc.Committer) {
				return "PR has a commit not authored and committed by a trusted author"
			}
		}
		total += len(commits)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if total == 0 || (pr.GetCommits() > 0 && pr.GetCommits() != total) {
		return "complete commit list not proven"
	}
	return ""
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
// complete list, every changed line a pin replacement), which needs no extra
// API call and returns ("", nil).
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

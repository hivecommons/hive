package automerge

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
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

// pinOnlyMaxTagDepth bounds annotated-tag dereferencing (a tag of a tag).
const pinOnlyMaxTagDepth = 3

// pinOnlyActionPinsUpstream returns "" when every accepted action-sha pin is
// a commit of its upstream action repository. GitHub resolves a `uses:` SHA
// through the whole fork network, so a 40-hex commit pushed to any fork of
// owner/repo is a valid pin; the classifier sees syntax only. A pin with a
// version comment must be exactly what tags/<version> (dereferenced through
// annotated tags) points at; one without must be an ancestor of the upstream
// default branch. Any API error, 404 or mismatch fails closed with a reason.
// Image digests are content-addressed and need no such anchor.
func (c *Engine) pinOnlyActionPinsUpstream(ctx context.Context, pins []intent.ActionPin) string {
	if len(pins) == 0 {
		return ""
	}
	ctx = hgithub.WithRESTCaller(ctx, "hive:automerge_pin_only")
	for _, pin := range pins {
		if why := c.pinOnlyActionPinUpstream(ctx, pin); why != "" {
			return fmt.Sprintf("action %s@%s: %s", pin.Repo, stagelabels.ShortSHA(pin.SHA), why)
		}
	}
	return ""
}

func (c *Engine) pinOnlyActionPinUpstream(ctx context.Context, pin intent.ActionPin) string {
	owner, repo, ok := strings.Cut(pin.Repo, "/")
	if !ok || owner == "" || repo == "" || pin.SHA == "" {
		return "upstream repository or commit unknown"
	}
	if pin.Version != "" {
		ref, _, err := c.gh.Git.GetRef(ctx, owner, repo, "tags/"+pin.Version)
		if err != nil {
			return fmt.Sprintf("tag %s could not be resolved upstream: %v", pin.Version, err)
		}
		obj := ref.GetObject()
		for depth := 0; obj.GetType() == "tag"; depth++ {
			if depth == pinOnlyMaxTagDepth {
				return fmt.Sprintf("tag %s nests too deeply", pin.Version)
			}
			tag, _, err := c.gh.Git.GetTag(ctx, owner, repo, obj.GetSHA())
			if err != nil {
				return fmt.Sprintf("annotated tag %s could not be resolved upstream: %v", pin.Version, err)
			}
			obj = tag.GetObject()
		}
		if obj.GetType() != "commit" || !strings.EqualFold(obj.GetSHA(), pin.SHA) {
			return fmt.Sprintf("upstream tag %s does not point at the pinned commit", pin.Version)
		}
		return ""
	}
	upstream, _, err := c.gh.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Sprintf("upstream repository could not be read: %v", err)
	}
	branch := upstream.GetDefaultBranch()
	if branch == "" {
		return "upstream default branch unknown"
	}
	// base=pin, head=default: "ahead" or "identical" means the pinned commit
	// is an ancestor of (reachable from) the upstream default branch.
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, pin.SHA, branch, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return fmt.Sprintf("pinned commit could not be compared with upstream %s: %v", branch, err)
	}
	if status := cmp.GetStatus(); status != "ahead" && status != "identical" {
		return fmt.Sprintf("pinned commit is not on upstream %s (compare status %q)", branch, status)
	}
	return ""
}

func pinOnlyTrustedLogin(policy config.PinOnlyException, login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	for _, trusted := range policy.TrustedAuthors {
		if strings.EqualFold(strings.TrimSpace(trusted), login) {
			return true
		}
	}
	return false
}

// pinOnlyCommitsTrusted returns "" when every commit of pr is attributed to a
// trusted author, was committed by a trusted login or by GitHub itself
// (web-flow), carries a signature GitHub verified, and the current head is
// the newest listed commit. GitHub verifies a signature against the
// committer's email only, so a verified commit proves the committer, not the
// author: a write-access principal with their own signing key can craft a
// commit whose author is a bot's noreply address and sign it as themselves
// (see pr_merge_state.go on verification reasons). Requiring the committer to
// be web-flow (GitHub-created and GitHub-signed, so GitHub set the author) or
// a trusted login (a human trusted author signing locally) closes that; a
// spoofed committer email fails verification because it is not on the
// signer's account. Any listing error, an empty or truncated list, or a head
// not in the list fails closed with a reason.
func (c *Engine) pinOnlyCommitsTrusted(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy config.PinOnlyException) string {
	head := pr.GetHead().GetSHA()
	if head == "" {
		return "PR head SHA unknown"
	}
	ctx = hgithub.WithRESTCaller(ctx, "hive:automerge_pin_only")
	all, complete, err := hgithub.ListPRRepositoryCommits(ctx, c.gh, owner, repo, pr.GetNumber())
	if err != nil {
		return fmt.Sprintf("PR commits could not be listed: %v", err)
	}
	if !complete {
		return "PR commit list too long to verify"
	}
	if len(all) == 0 {
		return "PR has no commits to verify"
	}
	if all[len(all)-1].GetSHA() != head {
		return "PR commit list does not end at the current head"
	}
	for _, rc := range all {
		sha := rc.GetSHA()
		if sha == "" {
			return "PR commit without a SHA"
		}
		if login := hgithub.SafeGetLogin(rc.GetAuthor()); !pinOnlyTrustedLogin(policy, login) {
			return fmt.Sprintf("commit %s author %q is not in trusted_authors", stagelabels.ShortSHA(sha), login)
		}
		if committer := hgithub.SafeGetLogin(rc.GetCommitter()); !pinOnlyTrustedCommitter(policy, committer) {
			return fmt.Sprintf("commit %s committer %q is neither GitHub nor in trusted_authors", stagelabels.ShortSHA(sha), committer)
		}
		if !rc.GetCommit().GetVerification().GetVerified() {
			return fmt.Sprintf("commit %s has no GitHub-verified signature", stagelabels.ShortSHA(sha))
		}
	}
	return ""
}

// pinOnlyWebFlowLogin is the login GitHub attaches as committer to commits it
// creates and signs itself (web UI, Apps such as Dependabot and Renovate).
const pinOnlyWebFlowLogin = "web-flow"

// pinOnlyTrustedCommitter accepts GitHub's own committer identity or a
// trusted author committing (and signing) locally; the signature check then
// binds to this identity.
func pinOnlyTrustedCommitter(policy config.PinOnlyException, login string) bool {
	return strings.EqualFold(strings.TrimSpace(login), pinOnlyWebFlowLogin) || pinOnlyTrustedLogin(policy, login)
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

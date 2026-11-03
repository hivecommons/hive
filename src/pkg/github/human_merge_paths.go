package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// Human-merge paths (#11027): auto_merge.human_merge_paths lists, per repo,
// paths a person must merge. Every App merge path (the automerge sweep lanes
// and the merge-request relay) refuses a PR that touches one, adds `hold`,
// and leaves one marker-stamped comment naming the path(s), unless the
// configured pin-only exception is proven.
const (
	// HumanMergePathsConfigKey is the config key named in every refusal.
	HumanMergePathsConfigKey = "auto_merge.human_merge_paths"
	// HumanMergePathMarker stamps the single hold comment so later ticks
	// find it and do not repost.
	HumanMergePathMarker = "<!-- hive-human-merge-path -->"
	// HumanMergePathHoldLabel is the label applied on a match.
	HumanMergePathHoldLabel = "hold"
)

// ErrIncompletePRFileList is wrapped by ListPRChangedFiles when GitHub
// reported more changed files than its files API returned (the API lists at
// most 3000). Unlike a transport failure it does not clear up on retry, so
// callers may treat it as a standing verdict rather than a transient fault.
var ErrIncompletePRFileList = errors.New("incomplete PR file list")

// ListPRChangedFiles returns the complete changed-file list of pr. It pages
// through the files API and refuses an incomplete list (GitHub reported more
// changed files than the API returned): a policy check on a partial list could
// miss a guarded path, so callers must treat the error as "unknown" and fail
// closed. The incomplete case wraps ErrIncompletePRFileList.
func ListPRChangedFiles(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest) ([]intent.ChangedFile, error) {
	if client == nil || pr == nil {
		return nil, fmt.Errorf("listing PR files: no client or PR")
	}
	number := pr.GetNumber()
	var files []intent.ChangedFile
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, number, fileOpts)
		if err != nil {
			return nil, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, intent.ChangedFile{
				Filename:  f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
				Patch:     f.GetPatch(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return nil, fmt.Errorf("%w: GitHub reported %d changed files but API returned %d; intent alignment requires the complete changed-file list", ErrIncompletePRFileList, reported, len(files))
	}
	return files, nil
}

func quotedPaths(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, "`"+p+"`")
	}
	return strings.Join(quoted, ", ")
}

// HumanMergePathReason is the refusal reason for a PR that touches matches.
// It names the path(s) and the config key so the operator can find both.
func HumanMergePathReason(matches []string) string {
	return fmt.Sprintf("PR touches %s, listed in %s for this repo; a person must merge it", quotedPaths(matches), HumanMergePathsConfigKey)
}

// HumanMergePathComment is the body of the single hold comment.
func HumanMergePathComment(matches []string) string {
	var b strings.Builder
	b.WriteString(HumanMergePathMarker + "\n")
	fmt.Fprintf(&b, "Hive will not merge this PR: it changes path(s) listed in `%s` for this repo, so a person must merge it.\n\n", HumanMergePathsConfigKey)
	for _, p := range matches {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	fmt.Fprintf(&b, "\nHive added the `%s` label. A maintainer should review and merge this PR by hand.\n", HumanMergePathHoldLabel)
	return b.String()
}

// HoldForHumanMergePaths adds the `hold` label to PR number and posts the
// marker-stamped comment naming matches, unless a comment carrying
// HumanMergePathMarker already exists. Shared by the sweep lanes and the
// merge-request relay so both leave exactly one comment.
func HoldForHumanMergePaths(ctx context.Context, client *gh.Client, owner, repo string, number int, matches []string, head ...string) error {
	if client == nil {
		return fmt.Errorf("human-merge-path hold: no client")
	}
	// Never attribute an existing hold to this policy. Older callers without an
	// exact head retain the legacy, non-releasable marker.
	attributedHead := ""
	if len(head) == 1 && validHumanMergeHead(head[0]) {
		pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return fmt.Errorf("fetching human-merge-path head: %w", err)
		}
		if pr.GetHead().GetSHA() == head[0] {
			attributedHead = head[0]
			for _, label := range pr.Labels {
				if strings.EqualFold(label.GetName(), HumanMergePathHoldLabel) {
					attributedHead = ""
				}
			}
		}
	}
	if _, _, err := client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanMergePathHoldLabel}); err != nil {
		return fmt.Errorf("adding %s label: %w", HumanMergePathHoldLabel, err)
	}
	posted, err := hasHumanMergePathComment(ctx, client, owner, repo, number)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}
	body := HumanMergePathComment(matches)
	if attributedHead != "" {
		body += humanMergeHeadMarker(attributedHead) + "\n"
	}
	if _, _, err := client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("posting human-merge-path comment: %w", err)
	}
	return nil
}

func hasHumanMergePathComment(ctx context.Context, client *gh.Client, owner, repo string, number int) (bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing comments for human-merge-path marker: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), HumanMergePathMarker) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

// SetHumanMergePaths installs auto_merge.human_merge_paths (owner/repo →
// patterns) for the merge-request relay. Safe to call on every config
// reload; nil/empty clears it.
func (c *Client) SetHumanMergePaths(paths map[string][]string) {
	if c == nil {
		return
	}
	var normalized map[string][]string
	for repo, patterns := range paths {
		key := repoPolicyKey(c.org, repo)
		if key == "" || len(patterns) == 0 {
			continue
		}
		if normalized == nil {
			normalized = make(map[string][]string, len(paths))
		}
		normalized[key] = append(normalized[key], patterns...)
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.humanMergePaths = normalized
}

func (c *Client) humanMergePathsFor(repo string) []string {
	if c == nil {
		return nil
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	if len(c.humanMergePaths) == 0 {
		return nil
	}
	return c.humanMergePaths[repoPolicyKey(c.org, repo)]
}

// PinOnlySpec is the relay's config-independent pin-only exception policy.
// It only lifts the human-merge-path gate, never any other merge requirement.
type PinOnlySpec struct {
	Enabled        bool
	Paths          []string
	Kinds          []string
	TrustedAuthors []string
}

func (p PinOnlySpec) clone() PinOnlySpec {
	p.Paths = append([]string(nil), p.Paths...)
	p.Kinds = append([]string(nil), p.Kinds...)
	p.TrustedAuthors = append([]string(nil), p.TrustedAuthors...)
	return p
}

// SetHumanMergePinOnly replaces the relay policy on config reload; nil clears it.
// Duplicate normalized repository keys fail closed.
func (c *Client) SetHumanMergePinOnly(policies map[string]PinOnlySpec) {
	if c == nil {
		return
	}
	normalized := make(map[string]PinOnlySpec, len(policies))
	for repo, policy := range policies {
		key := repoPolicyKey(c.org, repo)
		if key == "" {
			continue
		}
		if _, duplicate := normalized[key]; duplicate {
			normalized[key] = PinOnlySpec{}
			continue
		}
		normalized[key] = policy.clone()
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.humanMergePinOnly = normalized
}

func (c *Client) humanMergePinOnlyFor(repo string) PinOnlySpec {
	if c == nil {
		return PinOnlySpec{}
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.humanMergePinOnly[repoPolicyKey(c.org, repo)].clone()
}

// mergeRequestHumanMergePathRefusal checks a merge request against the
// repo's human-merge paths. It returns a non-empty refusal (terminal) when
// the PR touches one without a proven pin-only exception — after adding
// `hold` and the single marker comment —
// and an error when the repo has paths configured but the complete
// changed-file list cannot be established (fail closed). An unconfigured
// repo costs no API call.
func (c *Client) mergeRequestHumanMergePathRefusal(ctx context.Context, req MergeRequest) (string, error) {
	patterns := c.humanMergePathsFor(req.Repo)
	if len(patterns) == 0 {
		return "", nil
	}
	owner, name := c.splitRepo(req.Repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, req.Number)
	if err != nil {
		return "", fmt.Errorf("%s is set for %s but the PR could not be fetched to check its changed files (fail closed): %w", HumanMergePathsConfigKey, req.Repo, err)
	}
	files, err := ListPRChangedFiles(ctx, c.client, owner, name, pr)
	if err != nil {
		return "", fmt.Errorf("%s is set for %s but the complete changed-file list is unavailable (fail closed): %w", HumanMergePathsConfigKey, req.Repo, err)
	}
	matches := intent.HumanMergePathMatches(files, patterns)
	if len(matches) == 0 {
		return "", nil
	}
	if ex := c.humanMergePinOnlyFor(req.Repo); ex.Enabled {
		reason := "PR author is not trusted"
		if PinOnlyTrustedLogin(ex.TrustedAuthors, SafeGetLogin(pr.GetUser())) {
			switch {
			case req.ExpectSHA == "" || pr.GetHead().GetSHA() != req.ExpectSHA:
				reason = "PR head does not match requested head"
			case pr.ChangedFiles == nil || pr.GetChangedFiles() != len(files):
				reason = "complete changed-file count is unavailable"
			default:
				reason = c.pinOnlyProvenance(ctx, owner, name, req, pr, files, matches, ex)
				if reason == "" {
					return "", nil
				}
			}
		}
		c.warn("merge-request watcher: pin-only exception rejected", "repo", req.Repo, "pr", req.Number, "reason", reason)
	}
	if err := HoldForHumanMergePaths(ctx, c.client, owner, name, req.Number, matches, pr.GetHead().GetSHA()); err != nil {
		c.warn("merge-request watcher: human-merge-path hold failed", "repo", req.Repo, "pr", req.Number, "error", err)
	}
	return HumanMergePathReason(matches), nil
}

// pinOnlyProvenance applies the relay's share of the pin-only exception once
// the author, requested head and file count have qualified: the whole diff
// must classify as pin replacements, every commit must be a verified
// trusted-author commit, every new action SHA must be anchored upstream
// (#16399, the same checks the sweep applies), and the head must still be the
// requested SHA after the file listing. It returns "" when the PR qualifies
// and the refusal reason otherwise.
func (c *Client) pinOnlyProvenance(ctx context.Context, owner, name string, req MergeRequest, pr *gh.PullRequest, files []intent.ChangedFile, matches []string, ex PinOnlySpec) string {
	ok, reason, pins := intent.ClassifyPinOnlyActions(files, matches, intent.PinOnlySpec{Paths: ex.Paths, Kinds: ex.Kinds})
	if !ok {
		return reason
	}
	if why := PinOnlyCommitsTrusted(ctx, c.client, owner, name, pr, ex.TrustedAuthors); why != "" {
		return why
	}
	if why := PinOnlyActionPinsUpstream(ctx, c.client, pins); why != "" {
		return why
	}
	// The files endpoint is not SHA-addressable. Ensure the head did not
	// move while paging; the eventual merge also uses req.ExpectSHA.
	current, _, err := c.client.PullRequests.Get(ctx, owner, name, req.Number)
	if err != nil || current.GetHead().GetSHA() != req.ExpectSHA {
		return "PR head could not be confirmed after listing files"
	}
	return ""
}

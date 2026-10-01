package github

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const githubCompareFileLimit = 300

// prRequestMaxBaseDriftCommits caps how far a candidate head may sit BEHIND
// its PR base at open time. A working branch cut from the base's own tip is
// behind by however many commits landed since the cut — minutes to hours on
// this path, tens at the outside. A branch cut from a DIFFERENT branch (the
// repository default instead of the PR target, hivecommons/hive#6807) is
// behind by the full divergence between the two lines — observed at 546
// commits, producing 170+-file unmergeable PRs whose diff is mostly the
// target branch's own work rendered as deletions. Opening such a PR silently
// is strictly worse than failing the request: nobody can review it, and
// merging it would revert the target. The limit converts that mistake into a
// loud agent-side rejection with the re-cut instructions in the error.
const prRequestMaxBaseDriftCommits = 100

// prBaseDriftError is a permanent policy mismatch, like
// prContentMetadataError: no change to the request metadata can make a
// wrongly-cut branch mergeable — only re-cutting the branch can.
type prBaseDriftError struct {
	base     string
	head     string
	behindBy int
}

func (e *prBaseDriftError) Error() string {
	return fmt.Sprintf(
		"head %q is %d commits behind base %q (limit %d) — the working branch appears to be cut from a different branch than the PR target; re-create it from the target tip ('git fetch origin %s && git checkout -b <branch> origin/%s'), re-apply your commits, and push again",
		e.head, e.behindBy, e.base, prRequestMaxBaseDriftCommits, e.base, e.base)
}

func prBaseDriftReason(err error) (string, bool) {
	var driftErr *prBaseDriftError
	if !errors.As(err, &driftErr) {
		return "", false
	}
	return driftErr.Error(), true
}

var compareHunkRE = regexp.MustCompile(`^@@ -(?:\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// prContentMetadataError is a permanent policy mismatch: changing the request
// metadata cannot make the candidate branch safe, but changing the branch can.
// Forge/API errors remain ordinary errors so the watcher retries them.
type prContentMetadataError struct {
	file string
	line int
	kind string
}

func (e *prContentMetadataError) Error() string {
	return fmt.Sprintf("internal %s metadata in %s:%d; attribution and run metadata belong in the PR body or commit trailer, not committed files", e.kind, e.file, e.line)
}

func prContentMetadataReason(err error) (string, bool) {
	var metadataErr *prContentMetadataError
	if !errors.As(err, &metadataErr) {
		return "", false
	}
	return metadataErr.Error(), true
}

// prRequestPrecheckError is a deterministic pre-PR failure: the branch can be
// fixed by changing the head, so retrying the same request should not open a PR.
type prRequestPrecheckError struct {
	reasons []string
}

func (e *prRequestPrecheckError) Error() string {
	if e == nil || len(e.reasons) == 0 {
		return "precheck failed"
	}
	return "precheck failed: " + strings.Join(e.reasons, "; ")
}

func prRequestPrecheckReason(err error) (string, bool) {
	var precheckErr *prRequestPrecheckError
	if !errors.As(err, &precheckErr) {
		return "", false
	}
	return precheckErr.Error(), true
}

// validatePRRequestContent checks only lines added by the candidate branch.
// Existing repository prose and deleted metadata do not block a cleanup PR.
func (c *Client) validatePRRequestContent(ctx context.Context, req PRRequest) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}

	owner, repo := c.splitRepo(strings.TrimSpace(req.Repo))
	base := strings.TrimSpace(req.Base)
	if base == "" {
		resolved, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return fmt.Errorf("validating PR content metadata: %w", err)
		}
		base = resolved
	}
	head := strings.TrimSpace(req.Head)
	comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		// #5343: a 404 here means the head ref is not on the remote, which on
		// this path almost always means the agent's PUSH failed to
		// authenticate. Report that cause instead of the downstream symptom —
		// the raw error sends an operator to investigate branch creation.
		return fmt.Errorf("validating PR content metadata: %w",
			c.diagnoseCompareFailure(ctx, owner, repo, base, head, err))
	}
	if comparison == nil {
		return fmt.Errorf("validating PR content metadata in %s/%s diff %s...%s: GitHub returned an empty comparison", owner, repo, base, head)
	}
	// Base-drift gate (hivecommons/hive#6807), checked BEFORE the file-count
	// cap below: a branch cut from the wrong line usually also blows past the
	// 300-file scan limit, and that error is retried as transient forever,
	// while this one is a permanent rejection with the actual cause.
	if behind := comparison.GetBehindBy(); behind > prRequestMaxBaseDriftCommits {
		return &prBaseDriftError{base: base, head: head, behindBy: behind}
	}
	// GitHub exposes changed files only on the first compare page and caps that
	// list at 300. Do not claim a clean scan when later files may be invisible.
	if len(comparison.Files) >= githubCompareFileLimit {
		return fmt.Errorf("validating PR content metadata in %s/%s diff %s...%s: comparison reached GitHub's %d-file scan limit", owner, repo, base, head, githubCompareFileLimit)
	}
	for _, file := range comparison.Files {
		if file == nil || file.GetPatch() == "" {
			continue
		}
		line, kind, found := addedInternalMetadata(file.GetPatch())
		if found {
			return &prContentMetadataError{file: file.GetFilename(), line: line, kind: kind}
		}
	}
	outcome := c.prRequestPrecheckFailures(ctx, owner, repo, head, comparison)
	if len(outcome.Reject) > 0 {
		c.recordPRPrecheckSkipped(req, nil)
		return &prRequestPrecheckError{reasons: outcome.Reject}
	}
	c.recordPRPrecheckSkipped(req, outcome.Skipped)
	return nil
}

// addedInternalMetadata scans a unified patch and returns the new-file line of
// the first leaked attribution marker. Marker strings are assembled from
// fragments so this detector's own source is not itself a match.
func addedInternalMetadata(patch string) (line int, kind string, found bool) {
	filedPrefix := strings.ToLower("Filed" + " by ")
	hivePrefix := strings.ToLower("hive" + ":")
	newLine := 0
	inHunk := false

	scanner := bufio.NewScanner(strings.NewReader(patch))
	for scanner.Scan() {
		text := scanner.Text()
		if match := compareHunkRE.FindStringSubmatch(text); match != nil {
			newLine, _ = strconv.Atoi(match[1])
			inHunk = true
			continue
		}
		if !inHunk || text == "" {
			continue
		}
		switch text[0] {
		case '+':
			if strings.HasPrefix(text, "+++") {
				continue
			}
			added := strings.TrimSpace(text[1:])
			lower := strings.ToLower(added)
			if filed := strings.Index(lower, filedPrefix); filed >= 0 &&
				strings.Contains(lower[filed+len(filedPrefix):], " agent (acmm") {
				return newLine, "agent attribution", true
			}
			trimmed := strings.TrimSpace(strings.TrimLeft(added, "—-"))
			if strings.HasPrefix(strings.ToLower(trimmed), hivePrefix) {
				return newLine, "hive run", true
			}
			newLine++
		case '-':
			// Removed lines do not exist in the candidate tree.
		case ' ':
			newLine++
		case '\\':
			// "No newline at end of file" marker.
		}
	}
	return 0, "", false
}

func (c *Client) prRequestPrecheckFailures(ctx context.Context, owner, repo, head string, comparison *gh.CommitsComparison) prPrecheckOutcome {
	if comparison == nil {
		return prPrecheckOutcome{}
	}
	var outcome prPrecheckOutcome
	if reason := prRequestChangelogPrecheck(ctx, c, owner, repo, head, comparison.Files); reason != "" {
		outcome.Reject = append(outcome.Reject, reason)
	}
	if reason := prRequestDCOPrecheck(comparison.Commits); reason != "" {
		outcome.Reject = append(outcome.Reject, reason)
	}
	external := c.runPRRequestExternalPrechecks(ctx, owner, repo, head, comparison.Files)
	outcome.Reject = append(outcome.Reject, external.Reject...)
	outcome.Skipped = append(outcome.Skipped, external.Skipped...)
	return outcome
}

func prRequestChangelogPrecheck(ctx context.Context, c *Client, owner, repo, head string, files []*gh.CommitFile) string {
	var codeFiles, fragments []string
	for _, file := range files {
		if file == nil {
			continue
		}
		name := file.GetFilename()
		if name == "" {
			continue
		}
		removed := file.GetStatus() == "removed"
		if isChangelogFragmentPath(name) && !removed {
			fragments = append(fragments, name)
		}
		if isChangelogRelevantCodePath(name) {
			codeFiles = append(codeFiles, name)
		}
	}
	for _, fragment := range fragments {
		if reason := validateChangelogFragmentName(fragment); reason != "" {
			return reason
		}
		if c != nil && c.client != nil {
			content, err := c.repositoryFileContent(ctx, owner, repo, head, fragment)
			if err != nil {
				return fmt.Sprintf("could not read changelog fragment %s at %s: %v", fragment, head, err)
			}
			if reason := validateChangelogFragmentBody(fragment, content); reason != "" {
				return reason
			}
		}
	}
	if len(codeFiles) == 0 || len(fragments) > 0 {
		return ""
	}
	// The fragment convention is hive's own. A target repo that has no
	// changelog.d/ directory at the PR head (docs, pluk, a Java or C project)
	// must not be rejected for not following it; its CI owns that call.
	if !repoUsesChangelogFragments(ctx, c, owner, repo, head) {
		return ""
	}
	return fmt.Sprintf("src/ code changed without a changelog.d fragment: add changelog.d/<added|changed|deprecated|fixed|security>-<slug>.md containing one '- ' bullet, or ask a maintainer to apply the no-changelog label (first code path: %s)", codeFiles[0])
}

// repoUsesChangelogFragments reports whether the target repo carries a
// changelog.d/ directory at ref. Without a client it answers true so the
// in-memory tests of the primary repo keep their strict behaviour.
func repoUsesChangelogFragments(ctx context.Context, c *Client, owner, repo, ref string) bool {
	if c == nil || c.client == nil {
		return true
	}
	_, dir, _, err := c.client.Repositories.GetContents(ctx, owner, repo, "changelog.d", &gh.RepositoryContentGetOptions{Ref: ref})
	return err == nil && dir != nil
}

func (c *Client) repositoryFileContent(ctx context.Context, owner, repo, ref, file string) (string, error) {
	content, dir, _, err := c.client.Repositories.GetContents(ctx, owner, repo, file, &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return "", err
	}
	if content == nil || len(dir) > 0 {
		return "", fmt.Errorf("not a file")
	}
	return content.GetContent()
}

func isChangelogRelevantCodePath(name string) bool {
	if !strings.HasPrefix(name, "src/") || strings.HasPrefix(name, "src/docs/") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	base := path.Base(name)
	if (strings.HasPrefix(name, "src/deploy/") || strings.HasPrefix(name, "src/scripts/")) &&
		(strings.HasPrefix(base, "test-") || strings.HasPrefix(base, "test_")) && strings.HasSuffix(base, ".sh") {
		return false
	}
	return true
}

func isChangelogFragmentPath(name string) bool {
	return strings.HasPrefix(name, "changelog.d/") && !strings.Contains(strings.TrimPrefix(name, "changelog.d/"), "/") && strings.HasSuffix(name, ".md") && name != "changelog.d/README.md"
}

var changelogFragmentNameRE = regexp.MustCompile(`^changelog\.d/(added|changed|deprecated|fixed|security)-[A-Za-z0-9][A-Za-z0-9._-]*\.md$`)

func validateChangelogFragmentName(name string) string {
	if changelogFragmentNameRE.MatchString(name) {
		return ""
	}
	return fmt.Sprintf("%s: fragment name must be changelog.d/<category>-<slug>.md with category added, changed, deprecated, fixed, or security", name)
}

func validateChangelogFragmentBody(name, content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "<!-- release:") || strings.HasPrefix(trimmed, "<!--release:") {
			return ""
		}
		return fmt.Sprintf("%s: a fragment must start with a '- ' entry bullet (or a '<!-- release: ... -->' marker)", name)
	}
	return fmt.Sprintf("%s: a fragment must start with a '- ' entry bullet", name)
}

var commitSignoffRE = regexp.MustCompile(`(?mi)^Signed-off-by:\s*(?:.*?)<([^<>\s]+@[^<>\s]+)>\s*$`)

func githubNoreplyMatchesLogin(email, login string) bool {
	if email == "" || login == "" || !strings.HasSuffix(email, "@users.noreply.github.com") {
		return false
	}
	local := strings.TrimSuffix(email, "@users.noreply.github.com")
	if local == login {
		return true
	}
	plus := strings.LastIndex(local, "+")
	if plus <= 0 || plus == len(local)-1 || local[plus+1:] != login {
		return false
	}
	for _, r := range local[:plus] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func prRequestDCOPrecheck(commits []*gh.RepositoryCommit) string {
	var failures []string
	for _, commit := range commits {
		if commit == nil || len(commit.Parents) > 1 {
			continue
		}
		sha := commit.GetSHA()
		if sha == "" {
			sha = "unknown"
		}
		author := commit.GetCommit().GetAuthor().GetEmail()
		if author == "" {
			continue
		}
		author = strings.ToLower(strings.TrimSpace(author))
		login := strings.ToLower(strings.TrimSpace(commit.GetAuthor().GetLogin()))
		ok := false
		for _, match := range commitSignoffRE.FindAllStringSubmatch(commit.GetCommit().GetMessage(), -1) {
			if len(match) <= 1 {
				continue
			}
			signoff := strings.ToLower(strings.TrimSpace(match[1]))
			if signoff == author || (githubNoreplyMatchesLogin(signoff, login) && githubNoreplyMatchesLogin(author, login)) {
				ok = true
				break
			}
		}
		if !ok {
			failures = append(failures, shortSHA(sha))
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return fmt.Sprintf("DCO precheck failed: non-merge commit(s) %s lack a Signed-off-by trailer matching the commit author email", strings.Join(failures, ", "))
}

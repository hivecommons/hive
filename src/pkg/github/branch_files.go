package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// OpenPRForHead reports the open pull request whose head is branch in repo
// ("owner/repo" or a bare name under the hive org). ok is false when none is
// open.
func (c *Client) OpenPRForHead(ctx context.Context, repo, branch string) (number int, url string, ok bool, err error) {
	if c == nil || c.client == nil {
		return 0, "", false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	pr, err := c.findOpenPRForHead(ctx, owner, name, strings.TrimSpace(branch))
	if err != nil {
		return 0, "", false, fmt.Errorf("listing open PRs for %s/%s head %s: %w", owner, name, branch, err)
	}
	if pr == nil {
		return 0, "", false, nil
	}
	return pr.GetNumber(), pr.GetHTMLURL(), true, nil
}

// ResetBranch points refs/heads/branch at base's current tip, creating the
// branch when it does not exist, and returns that tip SHA. It is meant for
// hive-owned publishing branches whose previous PR is no longer open; it
// refuses to move base itself.
func (c *Client) ResetBranch(ctx context.Context, repo, branch, base string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	branch, base = strings.TrimSpace(branch), strings.TrimSpace(base)
	if branch == "" || base == "" {
		return "", errors.New("ResetBranch: branch and base are required")
	}
	if branch == base {
		return "", fmt.Errorf("ResetBranch: refusing to reset base branch %s onto itself", base)
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	baseRef, _, err := c.client.Git.GetRef(ctx, owner, name, "heads/"+base)
	if err != nil {
		return "", fmt.Errorf("reading ref heads/%s: %w", base, err)
	}
	sha := baseRef.GetObject().GetSHA()
	if sha == "" {
		return "", fmt.Errorf("reading ref heads/%s: empty sha", base)
	}
	ref := &gh.Reference{Ref: gh.Ptr("refs/heads/" + branch), Object: &gh.GitObject{SHA: gh.Ptr(sha)}}
	if _, _, err := c.client.Git.GetRef(ctx, owner, name, "heads/"+branch); err != nil {
		if !githubStatusError(err, http.StatusNotFound) {
			return "", fmt.Errorf("reading ref heads/%s: %w", branch, err)
		}
		if _, _, err := c.client.Git.CreateRef(ctx, owner, name, ref); err != nil {
			return "", fmt.Errorf("creating ref heads/%s at %s: %w", branch, shortSHA(sha), err)
		}
		return sha, nil
	}
	if _, _, err := c.client.Git.UpdateRef(ctx, owner, name, ref, true); err != nil {
		return "", fmt.Errorf("resetting heads/%s to %s: %w", branch, shortSHA(sha), err)
	}
	return sha, nil
}

// CommitFilesOnBranch writes files (repository path -> contents) to branch as
// one GitHub-signed commit whose parent must be expectedHeadSHA, and returns
// the new commit SHA. The App's DCO sign-off is appended to the message when
// the App login is known.
func (c *Client) CommitFilesOnBranch(ctx context.Context, repo, branch, expectedHeadSHA, headline, body string, files map[string]string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	branch, headline = strings.TrimSpace(branch), strings.TrimSpace(headline)
	if branch == "" || expectedHeadSHA == "" || headline == "" {
		return "", errors.New("CommitFilesOnBranch: branch, expected head and headline are required")
	}
	if len(files) == 0 {
		return "", errors.New("CommitFilesOnBranch: no files to commit")
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes signedFileChanges
	total := 0
	for _, p := range paths {
		total += len(files[p])
		if total > signedCommitMaxBytes {
			return "", fmt.Errorf("CommitFilesOnBranch: files exceed %d bytes", signedCommitMaxBytes)
		}
		changes.Additions = append(changes.Additions, signedFileAddition{Path: p, Contents: base64.StdEncoding.EncodeToString([]byte(files[p]))})
	}
	body = strings.TrimSpace(body)
	if login := strings.TrimSpace(c.AppBotLogin()); login != "" {
		if body != "" {
			body += "\n\n"
		}
		body += BotSignedOffByTrailer(login)
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	oid, err := c.createCommitOnBranch(ctx, owner+"/"+name, branch, expectedHeadSHA, headline, body, changes)
	if err != nil {
		return "", fmt.Errorf("committing %d file(s) to %s/%s@%s: %w", len(paths), owner, name, branch, err)
	}
	return oid, nil
}

package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/reviewpreflight"
)

// ReviewPreflightTree lists the repository's files at ref (the PR's base
// commit) for rule-document discovery. known is false on any failure. A tree
// GitHub reports as truncated is still returned: discovery works on what it
// has, and the rule documents the patterns look for sit near the root.
func (c *Client) ReviewPreflightTree(ctx context.Context, repo, ref string) (entries []reviewpreflight.TreeEntry, known bool) {
	ref = strings.TrimSpace(ref)
	if c == nil || c.client == nil || ref == "" {
		return nil, false
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	tree, _, err := c.client.Git.GetTree(WithRESTCaller(ctx, "hive:review_preflight_tree"), owner, name, ref, true)
	if err != nil || tree == nil {
		return nil, false
	}
	for _, e := range tree.Entries {
		if e.GetType() != "blob" {
			continue
		}
		entries = append(entries, reviewpreflight.TreeEntry{Path: e.GetPath(), Size: e.GetSize()})
	}
	return entries, true
}

// reviewPreflightMaxFileBytes caps one rule document read for reference
// discovery; a larger file is skipped rather than downloaded.
const reviewPreflightMaxFileBytes = 1 << 20

// ReviewPreflightFile reads one file at ref so rule-document discovery can
// follow the links and file mentions in it. ok is false on any failure, for a
// directory, and for a file over reviewPreflightMaxFileBytes.
func (c *Client) ReviewPreflightFile(ctx context.Context, repo, ref, path string) (content []byte, ok bool) {
	ref, path = strings.TrimSpace(ref), strings.TrimSpace(path)
	if c == nil || c.client == nil || ref == "" || path == "" {
		return nil, false
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	fc, _, _, err := c.client.Repositories.GetContents(WithRESTCaller(ctx, "hive:review_preflight_file"), owner, name, path, &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil || fc == nil || fc.GetSize() > reviewPreflightMaxFileBytes {
		return nil, false
	}
	text, err := fc.GetContent()
	if err != nil {
		return nil, false
	}
	return []byte(text), true
}

// ReviewPreflightFiles lists a PR's changed files with their patches for the
// dependency checks. known is false on any failure or an incomplete list.
func (c *Client) ReviewPreflightFiles(ctx context.Context, repo string, number int) (files []reviewpreflight.ChangedFile, known bool) {
	if c == nil || c.client == nil || number <= 0 {
		return nil, false
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	list, err := ListPRChangedFiles(WithRESTCaller(ctx, "hive:review_preflight_files"), c.client, owner, name, &gh.PullRequest{Number: gh.Ptr(number)})
	if err != nil {
		return nil, false
	}
	for _, f := range list {
		files = append(files, reviewpreflight.ChangedFile{Path: f.Filename, Patch: f.Patch})
	}
	return files, true
}

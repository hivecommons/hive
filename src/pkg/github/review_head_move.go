package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

// Verdicts across a head move (hivecommons/hive#15823).
//
// A review is dispatched against head H1. Before the verdict lands the PR is
// pushed — most often a rebase or a merge-from-base that changes nothing the
// reviewer read — and the PR's head is now H2. The verdict either names H2
// (the reviewer read the live PR) or H1 (it reported the head it was asked
// about). Either way the reviewer's judgement is about the same patch, and
// throwing it away costs a reviewer slot for nothing: the perspective stays
// owed, the planner re-dispatches it, and a second session re-reads the
// same change.
//
// ReviewHeadsSamePatch is the "same patch" test: the PR's diff against its
// base is the same at both heads. GitHub exposes no patch-id, so the two
// base...head compares are read and their files matched by path, status and
// hunk body with the `@@` headers stripped — what git's patch-id ignores too,
// since a rebase shifts line numbers without changing the change.

// patchIDEqual reports whether two compare file lists describe the same
// patch: same paths, same statuses, same hunk bodies modulo hunk headers.
func patchIDEqual(a, b []*gh.CommitFile) bool {
	if len(a) != len(b) {
		return false
	}
	index := func(files []*gh.CommitFile) map[string]string {
		out := make(map[string]string, len(files))
		for _, f := range files {
			if f == nil {
				continue
			}
			out[f.GetFilename()] = f.GetStatus() + "\x00" + f.GetPreviousFilename() + "\x00" + normalizeHunks(f.GetPatch())
		}
		return out
	}
	ia, ib := index(a), index(b)
	if len(ia) != len(ib) {
		return false
	}
	for path, fa := range ia {
		if fb, ok := ib[path]; !ok || fa != fb {
			return false
		}
	}
	return true
}

// normalizeHunks drops the `@@ -l,n +l,n @@` hunk headers from a unified
// patch so a line-number shift from a rebase does not read as a change.
func normalizeHunks(patch string) string {
	if patch == "" {
		return ""
	}
	lines := strings.Split(patch, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "@@") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// ReviewHeadsSamePatch reports whether heads h1 and h2 of a PR targeting
// base carry the same patch relative to that base. Unknown (a failed or
// oversized compare) is false: a verdict is never carried across a head move
// the hive could not verify.
func (c *Client) ReviewHeadsSamePatch(ctx context.Context, repo, base, h1, h2 string) bool {
	base, h1, h2 = strings.TrimSpace(base), strings.TrimSpace(h1), strings.TrimSpace(h2)
	if c == nil || c.client == nil || base == "" || h1 == "" || h2 == "" {
		return false
	}
	if review.SameHeadSHA(h1, h2) {
		return true
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if owner == "" || name == "" {
		return false
	}
	files := func(head string) ([]*gh.CommitFile, bool) {
		comparison, resp, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:review_head_move"), owner, name, base, head, &gh.ListOptions{PerPage: githubCompareFileLimit})
		if err != nil || comparison == nil {
			return nil, false
		}
		if (resp != nil && resp.NextPage != 0) || len(comparison.Files) >= githubCompareFileLimit {
			return nil, false
		}
		return comparison.Files, true
	}
	a, ok := files(h1)
	if !ok {
		return false
	}
	b, ok := files(h2)
	if !ok {
		return false
	}
	return patchIDEqual(a, b)
}

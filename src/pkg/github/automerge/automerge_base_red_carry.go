package automerge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Base red on a head the enumeration has not classified
// (projectbluefin/chairlift#811, common#1684).
//
// StageLabelPolicy.BaseBranchRed answers only for the head the last
// CI-enriched enumeration saw, so a new head (an author's push, a merge of
// the base) read as plain red CI until the next enumeration: the PR went to
// 072, its author was nudged and a fix queued in their relay, and minutes
// later the PR went back to 050 and the nudge was withdrawn. On chairlift#811
// that happened on both heads while main stayed red on `Unit Tests`.
//
// The enumeration's client remembers each base's red checks
// (hgithub.Client.BaseRedSnapshot). A head with no answer from the index is
// read the way the index would read it: every check red on the head (its own
// check digest) is remembered red on the base, and the head touches no
// CI-owning path (hgithub.TouchesCIOwningPath). Any other red check, a
// CI-owning path, or a file list that could not be read keeps today's
// author-facing route.

// baseRedFilesPages bounds the head's file listing (100 files per page),
// like the enumeration's own (hgithub.baseRedMaxFilePages): a longer list is
// treated as possibly owning CI.
const baseRedFilesPages = 3

var errBaseRedFilesTruncated = errors.New("file list longer than the base-red bound")

// baseRedPathsEntry is one head's CI-owning-path answer.
type baseRedPathsEntry struct {
	head    string
	touches bool
}

// stageBaseRedRemembered asks the policy which checks are remembered red on
// the PR's base branch.
func stageBaseRedRemembered(policy StageLabelPolicy, owner, repo, baseRef string) []string {
	if policy.BaseRedRemembered == nil || strings.TrimSpace(baseRef) == "" {
		return nil
	}
	return policy.BaseRedRemembered(owner+"/"+repo, baseRef)
}

// foldBaseRed marks a red PR head base red when the index has no answer for
// it and every check red on it is remembered red on its base.
func (c *Engine) foldBaseRed(ctx context.Context, tracker *StageTracker, list []stageObservation, now time.Time) {
	var o *stageObservation
	for i := range list {
		if len(list[i].BaseRedChecks) > 0 || len(list[i].ForkGuardChecks) > 0 || list[i].SharedCIIncident > 0 {
			return
		}
		if o == nil && strings.TrimSpace(list[i].HeadSHA) != "" {
			o = &list[i]
		}
	}
	if o == nil || o.Merged || strings.TrimSpace(o.BaseRef) == "" {
		return
	}
	head := o.HeadSHA
	red := false
	for _, l := range list {
		if l.HeadSHA == head && (classifyCIReason(l.Reason) == ciReasonRed || l.Detail.RedCheck(head) != "") {
			red = true
			break
		}
	}
	if !red {
		return
	}
	remembered := stageBaseRedRemembered(c.stagePolicy(), o.Owner, o.Repo, o.BaseRef)
	if len(remembered) == 0 {
		return
	}
	key := stageKey(o.Owner, o.Repo, o.Number)
	summary, known := c.headChecks(ctx, tracker, key, *o, now)
	if !known || len(summary.Red) == 0 {
		return
	}
	for _, name := range summary.Red {
		if !slices.Contains(remembered, name) {
			return
		}
	}
	touches, known := c.headTouchesCIOwningPath(ctx, tracker, key, *o)
	if !known || touches {
		return
	}
	checks := slices.Clone(summary.Red)
	slices.Sort(checks)
	checks = slices.Compact(checks)
	for i := range list {
		if list[i].HeadSHA == head {
			list[i].BaseRedChecks = append([]string(nil), checks...)
		}
	}
	c.debug("stage labels: base-branch red read for a head the enumeration has not classified", "repo", o.Display, "pr", o.Number, "head", head, "base", o.BaseRef, "checks", checks)
}

// headTouchesCIOwningPath reports whether obs's head changes a CI-owning
// path, from the duplicate sweep's cached file list or one listing, cached
// per head. known is false when the list could not be read in full; nothing
// is cached then, so the next pass tries again.
func (c *Engine) headTouchesCIOwningPath(ctx context.Context, tracker *StageTracker, key string, obs stageObservation) (touches, known bool) {
	tracker.mu.Lock()
	entry, ok := tracker.baseRedPaths[key]
	tracker.mu.Unlock()
	if ok && entry.head == obs.HeadSHA {
		return entry.touches, true
	}
	files := hgithub.CachedPRChangedPaths(obs.Owner+"/"+obs.Repo, obs.Number, obs.HeadSHA)
	if len(files) == 0 {
		var err error
		files, err = c.baseRedHeadFiles(ctx, obs.Owner, obs.Repo, obs.Number)
		if err != nil {
			c.debug("stage labels: PR file list unavailable; a red head stays author-facing", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "error", err)
			return false, false
		}
	}
	touches = hgithub.TouchesCIOwningPath(files)
	tracker.mu.Lock()
	if tracker.baseRedPaths == nil {
		tracker.baseRedPaths = map[string]baseRedPathsEntry{}
	}
	tracker.baseRedPaths[key] = baseRedPathsEntry{head: obs.HeadSHA, touches: touches}
	tracker.mu.Unlock()
	return touches, true
}

// baseRedHeadFiles lists the PR's changed paths (renames: both names), or
// errBaseRedFilesTruncated past baseRedFilesPages.
func (c *Engine) baseRedHeadFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if c == nil || c.gh == nil {
		return nil, hgithub.ErrNoGitHubClient
	}
	var files []string
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < baseRedFilesPages; page++ {
		list, resp, err := c.gh.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, err
		}
		for _, f := range list {
			if name := f.GetFilename(); name != "" {
				files = append(files, name)
			}
			if prev := f.GetPreviousFilename(); prev != "" {
				files = append(files, prev)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return files, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, errBaseRedFilesTruncated
}

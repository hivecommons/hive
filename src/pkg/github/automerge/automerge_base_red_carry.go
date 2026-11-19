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
//
// After a restart (projectbluefin/common#1689). The memory and the index
// live in the process and are published only when the first CI-enriched
// enumeration finishes, which took up to 27 minutes on knuckle on
// 2026-11-18. In that window every base read as not red: common's PRs red
// only on main's own E2E check went to 070/072, and #1689 (and #1690's
// first adoption), past the 072 grace, was adopted at 1:42 PM EST and
// superseded, losing a maintainer's approval, into a copy red for the same
// reason. Until the memory is published
// (StageLabelPolicy.BaseRedRememberedReady), a red head whose red checks the
// memory does not cover reads its base tip directly, once per base per
// humanChecksTTL (baseTipRedChecks), and is base red when every check red
// on it is red there too. That read can still miss the base's red (a run in
// progress on the tip, a tip without the check, a failed read), so
// automatic adoption also waits for the publish (adoptContributorPR); an
// operator's adoption does not.

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

// stageBaseRedRememberedReady reports whether the policy's base-red memory
// has been published since the hive started; nil reads as ready.
func stageBaseRedRememberedReady(policy StageLabelPolicy) bool {
	return policy.BaseRedRememberedReady == nil || policy.BaseRedRememberedReady()
}

// baseTipRedErrorTTL bounds how long a failed base tip read is reused: a
// minute, not humanChecksTTL, so one API error does not leave the base
// author-facing for the rest of a short startup window.
const baseTipRedErrorTTL = time.Minute

// baseTipRedEntry is one base branch tip's red checks as baseTipRedChecks
// read them, and when; ok is false when the read failed.
type baseTipRedEntry struct {
	at  time.Time
	red []string
	ok  bool
}

// baseTipRedChecks lists the checks red on obs's base branch tip (its
// combined status and latest check run per name and app, as the head's own
// digest reads them: commit statuses as well as check runs), cached per
// base for humanChecksTTL and a failed read for baseTipRedErrorTTL, so a
// pass costs at most one read per base. A check still queued or in progress
// on the tip, or with no run there, is not red. ok is false when the read
// failed.
func (c *Engine) baseTipRedChecks(ctx context.Context, tracker *StageTracker, obs stageObservation, now time.Time) ([]string, bool) {
	key := hgithub.BaseRedRememberedKey(obs.Owner+"/"+obs.Repo, obs.BaseRef)
	tracker.mu.Lock()
	entry, cached := tracker.baseTipRed[key]
	tracker.mu.Unlock()
	ttl := humanChecksTTL
	if !entry.ok {
		ttl = baseTipRedErrorTTL
	}
	if cached && now.Sub(entry.at) < ttl {
		return entry.red, entry.ok
	}
	summary, err := c.summarizeChecks(ctx, obs.Owner, obs.Repo, obs.BaseRef, func() map[string]bool { return nil })
	if err != nil {
		c.debug("stage labels: base tip checks unavailable; a red head stays author-facing", "repo", obs.Display, "base", obs.BaseRef, "error", err)
	}
	entry = baseTipRedEntry{at: now, red: slices.Clone(summary.Red), ok: err == nil}
	tracker.mu.Lock()
	if tracker.baseTipRed == nil {
		tracker.baseTipRed = map[string]baseTipRedEntry{}
	}
	tracker.baseTipRed[key] = entry
	tracker.mu.Unlock()
	return entry.red, entry.ok
}

// allIn reports whether every name in names is in set.
func allIn(names, set []string) bool {
	for _, name := range names {
		if !slices.Contains(set, name) {
			return false
		}
	}
	return true
}

// foldBaseRed marks a red PR head base red when the index has no answer for
// it and every check red on it is remembered red on its base, or, before the
// memory is published, red on the base tip.
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
	policy := c.stagePolicy()
	remembered := stageBaseRedRemembered(policy, o.Owner, o.Repo, o.BaseRef)
	ready := stageBaseRedRememberedReady(policy)
	if len(remembered) == 0 && ready {
		return
	}
	key := stageKey(o.Owner, o.Repo, o.Number)
	summary, known := c.headChecks(ctx, tracker, key, *o, now)
	if !known || len(summary.Red) == 0 {
		return
	}
	source := "remembered"
	if !ready && !allIn(summary.Red, remembered) {
		// Nothing published since the hive started: ask the base tip.
		tip, ok := c.baseTipRedChecks(ctx, tracker, *o, now)
		if !ok {
			return
		}
		remembered = append(slices.Clone(remembered), tip...)
		source = "base-tip"
	}
	if !allIn(summary.Red, remembered) {
		return
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
	c.debug("stage labels: base-branch red read for a head the enumeration has not classified", "repo", o.Display, "pr", o.Number, "head", head, "base", o.BaseRef, "checks", checks, "source", source)
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

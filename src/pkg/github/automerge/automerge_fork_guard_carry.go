package automerge

import (
	"context"
	"slices"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Fork guard across heads (projectbluefin/utah-packages#869, #862).
//
// The fork guard (hgithub.PullRequest.ForkGuardChecks) is read from the
// failing check's annotation, which the shared-CI index holds per head and
// only after the next enumeration reads it. A new head on a fork PR parked on
// the guard (a merge of the base, or the author's own push) therefore read as
// an ordinary red check for a few minutes: the PR dropped to 072, the author
// was nudged to fix a check only a maintainer can run, and minutes later it
// went back to 085. #862 did that on each of 13 merge-of-main heads.
//
// A fork stays a fork, so the record's `fork-guard:<checks>` reason is carried
// onto the new head while the lane's red check is one of those checks.
// ForkGuardStage still requires a red CI reason and every red optional check
// in the list, so any other failure sends the PR to its author as before.

// laneRedCheck is the check a lane reason names as red, or "".
func laneRedCheck(reason string) string {
	r := strings.TrimSpace(reason)
	for _, prefix := range []string{"required-check-failing:", "check-failing:", stageReasonOptionalCheckFailing} {
		if name, ok := strings.CutPrefix(r, prefix); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// recordedForkGuard is the checks a stage record parked the PR on, or nil.
func recordedForkGuard(rec stagelabels.Record, ok bool) []string {
	if !ok {
		return nil
	}
	list, found := strings.CutPrefix(strings.TrimSpace(rec.Reason), stageReasonForkGuard)
	if !found {
		return nil
	}
	var out []string
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func obsFromFork(obs stageObservation) bool {
	return obs.HeadRepo != "" && obs.Owner != "" && !strings.EqualFold(obs.HeadRepo, obs.Owner+"/"+obs.Repo)
}

// carriedForkGuard offers the record's fork guard for a fork PR head the
// index has not read yet, when the lane's red check is one of its checks.
func carriedForkGuard(rec stagelabels.Record, ok bool, obs stageObservation) []string {
	if !obsFromFork(obs) {
		return nil
	}
	checks := recordedForkGuard(rec, ok)
	name := laneRedCheck(obs.Reason)
	for _, c := range checks {
		if c == name {
			return checks
		}
	}
	return nil
}

// waitingForkGuard is the fork guard a fork PR head waits on: the check is
// red on it and only a maintainer's exact-head run turns it green. A branch
// update would discard that run, so the caller leaves the branch alone while
// this is non-empty; once the check is green updates resume before merge.
//
// The index for this exact head answers first. It is process memory rebuilt
// from each CI-enriched enumeration, so it is empty after a restart and has
// no entry after a pass that could not read the check's annotations; on
// projectbluefin/utah-packages#917 each such gap let a merge of main through
// (five in 22 hours). When it has nothing, the stage record's fork guard
// (ForkGuardChecks, or a `fork-guard:` reason) is consulted. A remembered
// guard may already be green, so it counts only for the checks whose latest
// run on this head is red (failure or action_required); with every one green
// the update goes ahead. When the head's checks cannot be read the guard
// stands for this tick: skipping costs a tick, an update costs the
// maintainer's run.
func (c *Engine) waitingForkGuard(ctx context.Context, owner, repo string, number int, head string) []string {
	if checks := stageForkGuard(c.stagePolicy(), owner, repo, number, head); len(checks) > 0 {
		return checks
	}
	rec, ok := c.stageStoreOrShared().Get(stageKey(owner, repo, number))
	guard := rec.ForkGuardChecks
	if ok && len(guard) == 0 {
		guard = recordedForkGuard(rec, ok)
	}
	if !ok || len(guard) == 0 || strings.TrimSpace(head) == "" {
		return nil
	}
	red, err := c.forkGuardRedOnHead(ctx, owner, repo, head, guard)
	if err != nil {
		c.warn("automerge: fork guard check state unreadable; holding the branch update this tick", "repo", owner+"/"+repo, "pr", number, "head", head, "checks", guard, "error", err)
		return guard
	}
	return red
}

// forkGuardRedOnHead is the guard checks whose latest run on head completed
// failure or action_required, in guard order.
func (c *Engine) forkGuardRedOnHead(ctx context.Context, owner, repo, head string, guard []string) ([]string, error) {
	if c == nil || c.gh == nil {
		return nil, hgithub.ErrNoGitHubClient
	}
	ctx = hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	latest := map[string]*gh.CheckRun{}
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, head, opts)
		if err != nil {
			return nil, err
		}
		if runs != nil {
			for _, cr := range runs.CheckRuns {
				if cr == nil || !slices.Contains(guard, cr.GetName()) {
					continue
				}
				if prev := latest[cr.GetName()]; prev == nil || cr.GetID() > prev.GetID() {
					latest[cr.GetName()] = cr
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	var red []string
	for _, name := range guard {
		cr := latest[name]
		if cr == nil || cr.GetStatus() != "completed" {
			continue
		}
		if conclusion := cr.GetConclusion(); conclusion == "failure" || conclusion == "action_required" {
			red = append(red, name)
		}
	}
	return red, nil
}

// forkGuardEntryStale reports a PR parked at 085 on a fork guard whose 085
// entry comment was written for another head. The comment asks a
// maintainer to run the check for one exact head, and a run proves only the
// head it ran on: on projectbluefin/utah-packages#917 the maintainer's
// Canary passed on 8d7a655, the author rebased to 82ef790 with the same
// patch onto a newer base, and the carried guard kept the PR at 085 while
// the comment still named an earlier head. Such a comment is rewritten in
// place (an edit notifies nobody) so it always names the current head.
func forkGuardEntryStale(rec stagelabels.Record, obs stageObservation, route stagelabels.Route) bool {
	head := strings.TrimSpace(obs.HeadSHA)
	return route.Stage == stagelabels.NeedsDecision && head != "" &&
		strings.HasPrefix(strings.TrimSpace(obs.Reason), stageReasonForkGuard) &&
		rec.EntryCommentedStage == stagelabels.NeedsDecision &&
		rec.EntryCommentedHead != "" && rec.EntryCommentedHead != head
}

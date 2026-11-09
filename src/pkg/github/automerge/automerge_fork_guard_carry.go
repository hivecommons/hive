package automerge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

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
// run on this head is red (failure or action_required) or still running
// (queued or in_progress: that is the maintainer's run, and an update would
// discard it); with every one green the update goes ahead. When the head's
// checks cannot be read the guard stands for this tick: skipping costs a
// tick, an update costs the maintainer's run.
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
	held, err := c.forkGuardHeldOnHead(ctx, owner, repo, head, guard)
	if err != nil {
		c.warn("automerge: fork guard check state unreadable; holding the branch update this tick", "repo", owner+"/"+repo, "pr", number, "head", head, "checks", guard, "error", err)
		return guard
	}
	return held
}

// forkGuardHeldOnHead is the guard checks whose latest run on head has not
// completed, or completed failure or action_required, in guard order. A
// guard with no run on the head is not held.
func (c *Engine) forkGuardHeldOnHead(ctx context.Context, owner, repo, head string, guard []string) ([]string, error) {
	latest, err := c.latestCheckRunsOnHead(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, head, guard)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, name := range guard {
		cr := latest[name]
		if cr == nil {
			continue
		}
		if cr.GetStatus() != "completed" {
			held = append(held, name)
			continue
		}
		if conclusion := cr.GetConclusion(); conclusion == "failure" || conclusion == "action_required" {
			held = append(held, name)
		}
	}
	return held, nil
}

// latestCheckRunsOnHead is the latest run (highest id) of each named check
// on head.
func (c *Engine) latestCheckRunsOnHead(ctx context.Context, owner, repo, head string, names []string) (map[string]*gh.CheckRun, error) {
	if c == nil || c.gh == nil {
		return nil, hgithub.ErrNoGitHubClient
	}
	latest := map[string]*gh.CheckRun{}
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, head, opts)
		if err != nil {
			return nil, err
		}
		if runs != nil {
			for _, cr := range runs.CheckRuns {
				if cr == nil || !slices.Contains(names, cr.GetName()) {
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
	return latest, nil
}

// Fork guard on a head nothing has classified yet
// (projectbluefin/utah-packages#972).
//
// The index learns a fork guard only from a CI-enriched enumeration that
// read the failing check's annotation, and the record carries one only
// once the PR was parked on it. A fork PR's first red head has neither:
// #972 was opened from Karibhou's fork at 14:41:50Z, its Canary refused it
// at 14:42:24Z ("This is expected, not something the author can fix"), and
// at 14:45:32Z the stage pass read required-check-failing:Canary as the
// author's to fix. It nudged Karibhou and queued relay rework for a check
// only a maintainer can run, five minutes before the index caught up and
// parked the PR at 085. Such a head is now classified from its own check
// annotations with the index's rule (hgithub.ForkGuardChecksFor), once per
// head and red set.

// errForkGuardUnsettled: a red check's latest run is still in progress (a
// re-run), so the head cannot be classified yet; nothing is remembered.
var errForkGuardUnsettled = errors.New("a red check is being re-run")

// forkGuardRead is one head's classification from its check annotations.
type forkGuardRead struct {
	head, red string
	guard     []string
}

// foldUnclassifiedForkGuard parks a fork PR head whose every red check is
// a fork guard when neither the index nor the stage record knows the guard:
// the observations for the head carry it (so the stage is 085, with no
// nudge and no adoption) and the record keeps it for later heads.
func (c *Engine) foldUnclassifiedForkGuard(ctx context.Context, tracker *StageTracker, store *stagelabels.Store, key string, list []stageObservation, o stageObservation, now time.Time) {
	if tracker.knownNotForkGuard(key, o.HeadSHA) {
		return
	}
	summary, known := c.headChecks(ctx, tracker, key, o, now)
	if !known || len(summary.Red) == 0 {
		return
	}
	guard, fresh := c.headForkGuard(ctx, tracker, key, o, summary.Red)
	if len(guard) == 0 {
		return
	}
	for i := range list {
		if list[i].HeadSHA == o.HeadSHA {
			list[i].ForkGuardChecks = append([]string(nil), guard...)
			list[i].ForkGuardCarried = false
		}
	}
	store.NoteForkGuard(key, guard)
	if fresh {
		c.info("stage labels: fork guard read from the head's check annotations; a maintainer's step, not the author's", "repo", o.Display, "pr", o.Number, "head", o.HeadSHA, "checks", guard)
	}
}

// knownNotForkGuard reports a head already read and found red on something
// other than a fork guard, so the stage pass need not read its digest again.
func (t *StageTracker) knownNotForkGuard(key, head string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.forkGuards[key]
	return ok && entry.head == head && len(entry.guard) == 0
}

// headForkGuard classifies head's red checks from their annotations, once
// per head and red set. fresh is true when this call read GitHub. A failed
// or unsettled read is not remembered, so the next pass reads again.
func (c *Engine) headForkGuard(ctx context.Context, tracker *StageTracker, key string, o stageObservation, red []string) (guard []string, fresh bool) {
	sorted := slices.Clone(red)
	slices.Sort(sorted)
	redKey := strings.Join(sorted, "\x00")
	tracker.mu.Lock()
	entry, ok := tracker.forkGuards[key]
	tracker.mu.Unlock()
	if ok && entry.head == o.HeadSHA && entry.red == redKey {
		return entry.guard, false
	}
	guard, err := c.forkGuardFromAnnotations(ctx, o.Owner, o.Repo, o.HeadSHA, red)
	if err != nil {
		c.debug("stage labels: could not classify the head's red checks as a fork guard", "repo", o.Display, "pr", o.Number, "head", o.HeadSHA, "red", red, "error", err)
		return nil, false
	}
	tracker.mu.Lock()
	if tracker.forkGuards == nil {
		tracker.forkGuards = map[string]forkGuardRead{}
	}
	tracker.forkGuards[key] = forkGuardRead{head: o.HeadSHA, red: redKey, guard: guard}
	tracker.mu.Unlock()
	return guard, true
}

// forkGuardFromAnnotations is red when every one of those checks' latest
// run on head carries a fork-guard annotation, else nil. It stops at the
// first check that does not, so a head with a real failure costs one
// annotation read.
func (c *Engine) forkGuardFromAnnotations(ctx context.Context, owner, repo, head string, red []string) ([]string, error) {
	latest, err := c.latestCheckRunsOnHead(ctx, owner, repo, head, red)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, name := range red {
		cr := latest[name]
		if cr == nil {
			// A commit status, not a check run: it has no annotations.
			return nil, nil
		}
		if cr.GetStatus() != "completed" {
			return nil, errForkGuardUnsettled
		}
		anns, _, err := c.gh.Checks.ListCheckRunAnnotations(ctx, owner, repo, cr.GetID(), &gh.ListOptions{PerPage: 50})
		if err != nil {
			return nil, err
		}
		for _, a := range anns {
			if level := a.GetAnnotationLevel(); level != "failure" && level != "warning" {
				continue
			}
			for _, line := range strings.Split(a.GetMessage(), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					lines = append(lines, name+": "+line)
				}
			}
		}
		if len(hgithub.ForkGuardChecksFor(true, []string{name}, strings.Join(lines, "\n"))) == 0 {
			return nil, nil
		}
	}
	return hgithub.ForkGuardChecksFor(true, red, strings.Join(lines, "\n")), nil
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

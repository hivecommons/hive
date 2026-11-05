package automerge

import (
	"strings"

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

// waitingForkGuard is the fork guard a fork PR head waits on, from the index
// for this exact head: the check is red on it and only a maintainer's
// exact-head run turns it green. A branch update would discard that run, so
// the caller leaves the branch alone while this is non-empty; once the check
// is green the index drops it and updates resume before merge. The stage
// record is not consulted here: a guard it remembers may already be green,
// and skipping on it would keep the PR parked on a check that passed.
func (c *Engine) waitingForkGuard(owner, repo string, number int, head string) []string {
	return stageForkGuard(c.stagePolicy(), owner, repo, number, head)
}

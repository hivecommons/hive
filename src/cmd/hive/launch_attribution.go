package main

import (
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/tokens"
)

// launchTriggerGovernor labels kicks assembled by the governor eval cycle
// (issue-list kicks); review swarm kicks are labelled "review-<kind>".
const launchTriggerGovernor = "governor-eval"

// launchRecordForKick builds the attribution record for one delivered kick
// (#12128). A review-swarm kick names exactly one PR with its head SHA; a
// governor kick names the issue refs the scheduler put in the message.
// Pure so the shape can be unit-tested without a live dispatcher.
func launchRecordForKick(msg scheduler.KickMessage, reviewKick review.DispatchKick, isReview bool, now time.Time) tokens.LaunchRecord {
	rec := tokens.LaunchRecord{
		Agent:        msg.Agent,
		Trigger:      launchTriggerGovernor,
		Repo:         msg.Repo,
		LaunchedAtMs: now.UnixMilli(),
	}
	if isReview {
		rec.Trigger = "review-" + reviewKick.Kind
		rec.Repo = reviewKick.Repo
		if reviewKick.Repo != "" && reviewKick.Number > 0 {
			rec.Refs = []tokens.LaunchRef{tokens.PRLaunchRef(reviewKick.Repo, reviewKick.Number, reviewKick.HeadSHA)}
			return rec
		}
	}
	seen := map[string]bool{}
	for _, key := range msg.IssueRefs {
		ref, ok := tokens.ParseLaunchRef(key)
		if !ok || seen[ref.Ref] {
			continue
		}
		seen[ref.Ref] = true
		if isReview {
			ref.Kind = tokens.LaunchRefKindPR
			ref.HeadSHA = reviewKick.HeadSHA
		}
		rec.Refs = append(rec.Refs, ref)
	}
	return rec
}

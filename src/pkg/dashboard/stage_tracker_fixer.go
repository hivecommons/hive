package dashboard

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Fixer standing on a hive PR row (hivecommons/hive#15744). Hive's own
// #15308 read "Fixing · @hivecommons-hive[bot]" for 13h while the fix
// dispatcher refused it every cycle (fix-attempts-exhausted, 6 attempts):
// the row borrowed the author as the actor. A 070 row now says why nobody
// is fixing when the dispatcher bars a fix, and the 085 row the stage
// engine parks an exhausted PR on captions the reviewer's last finding.

const stageTrackerFixerExhaustedSummaryMax = 140

// stageTrackerApplyFixerStatus rewrites the caption and stage actor of a
// hive-authored PR row when the fix dispatcher has no job on its head and
// refuses to start one. Rows with an agent on them, non-hive rows and rows
// off the fixing stage are left alone.
func stageTrackerApplyFixerStatus(it *StageTrackerItem, fixer review.FixDispatchStatus, workers map[string]stageTrackerWorker) {
	if it == nil || it.Kind != "pr" || it.Stage != trackerNumberFixing || !fixer.Excluded() {
		return
	}
	if it.StageActor == nil || it.StageActor.Kind != StageActorHive {
		return
	}
	if _, busy := stageTrackerWorkerOn(workers, it.Repo, it.Number); busy {
		return
	}
	why := review.FixDispatchExclusionText(fixer)
	if why == "" {
		return
	}
	if slices.Contains(it.Modifiers, modNeedsRebase) {
		why += "; conflicting with the base branch"
	}
	it.Caption = "Fixing paused — " + why + "; no fix job is dispatched"
	it.StageActor.Kind = StageActorNone
	it.StageActor.Login = ""
	it.StageActor.Why = "nobody — " + why + "; the hive will not re-kick it"
	if fixer.Exhausted() {
		it.Actor = "maintainer"
		it.StageActor.Why += " — a person decides"
	}
}

// stageTrackerFixerExhaustedCaption is the 085 caption for a PR parked on
// stagelabels.FixerExhaustedReason: "fixer gave up after N attempts — <first
// line of the reviewer's finding>". summary "" drops the dash clause.
func stageTrackerFixerExhaustedCaption(attempts int, summary string) string {
	c := "fixer gave up after " + strconv.Itoa(attempts) + " attempts"
	if summary = stageTrackerFirstLine(summary); summary != "" {
		c += " — " + summary
	}
	return c
}

// stageTrackerFixerExhaustedSummary is the reviewer's one-line finding for
// the exhausted caption, read from the head's review aggregate.
func stageTrackerFixerExhaustedSummary(agg *review.Aggregate) string {
	if agg == nil {
		return ""
	}
	for _, s := range agg.HumanSummaries {
		if s = strings.TrimSpace(s); s != "" {
			return stageTrackerFirstLine(s)
		}
	}
	if q := strings.TrimSpace(agg.DecisionQuestion); q != "" {
		return stageTrackerFirstLine(q)
	}
	for _, f := range agg.Findings {
		if t := strings.TrimSpace(f.Finding.Title); t != "" {
			return stageTrackerFirstLine(t)
		}
	}
	for _, r := range agg.Reasons {
		if r = strings.TrimSpace(r); r != "" {
			return stageTrackerFirstLine(r)
		}
	}
	return ""
}

func stageTrackerFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > stageTrackerFixerExhaustedSummaryMax {
		s = strings.TrimSpace(s[:stageTrackerFixerExhaustedSummaryMax-1]) + "…"
	}
	return s
}

// stageTrackerFixerExhaustedAttempts reads the attempt count from a row's
// 085 reason; ok is false for any other reason.
func stageTrackerFixerExhaustedAttempts(reason string) (int, bool) {
	return stagelabels.ParseFixerExhausted(reason)
}

package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Hive self re-authoring on the tracker. When the fixer gives up on a
// HIVE-authored PR the engine no longer parks it for a person: it closes
// the PR as superseded and lets the issue requeue so a fresh PR is opened
// from the current base (automerge.reauthorExhaustedHivePR; one automatic
// re-author per issue). The tracker shows that as motion, not as a gap:
//
//   - the issue row, until the fresh hive PR is listed, sits in the
//     PR-in-progress lane with "Re-authoring · superseding #<old>" and a
//     `remediation` of kind kick (the evidence is the engine's record);
//   - the fresh hive PR carries lineage {kind: "re-authored", from_number}
//     so its chip reads "re-authored from #<old>" (never "adopted from",
//     which is for a contributor's PR the hive took over);
//   - an old PR still listed with the re-authoring reason (the sweep closes
//     it in the same pass, so rarely) reads the same caption.

const (
	lineageReauthored          = "re-authored"
	lineageReasonFixerExhaust  = "fixer-exhausted"
	stageTrackerReauthorSource = "re-author"
	// stageTrackerReauthorWindow bounds how long an issue row claims
	// "Re-authoring" on the record alone: past it, with no fresh PR
	// listed, the row falls back to its native words rather than
	// promising motion the scheduler has not produced.
	stageTrackerReauthorWindow = 24 * time.Hour
)

// stageTrackerReauthorRecord is one re-author the engine recorded: the
// superseded hive PR and when.
type stageTrackerReauthorRecord struct {
	Repo     string
	Issue    int
	OldPR    int
	Attempts int
	At       time.Time
}

// stageTrackerReauthorIndex lists the engine's re-author records by
// issue key (repo#issue). Several records for one issue keep the latest.
func stageTrackerReauthorIndex(store *stagelabels.Store) map[string]stageTrackerReauthorRecord {
	out := map[string]stageTrackerReauthorRecord{}
	if store == nil {
		return out
	}
	for _, key := range store.Keys() {
		rec, ok := store.Get(key)
		if !ok || !rec.Reauthored() {
			continue
		}
		repo, numText, found := strings.Cut(key, "#")
		old, err := strconv.Atoi(numText)
		if !found || err != nil || old <= 0 || repo == "" {
			continue
		}
		ik := stagelabels.Key(repo, rec.ReauthorIssue)
		if prev, dup := out[ik]; dup && prev.At.After(rec.ReauthoredAt) {
			continue
		}
		out[ik] = stageTrackerReauthorRecord{Repo: repo, Issue: rec.ReauthorIssue, OldPR: old, Attempts: rec.ReauthorAttempts, At: rec.ReauthoredAt}
	}
	return out
}

func stageTrackerReauthorCaption(oldPR int) string {
	return "Re-authoring · superseding #" + strconv.Itoa(oldPR)
}

// stageTrackerReauthorLineage is the fresh hive PR's lineage back to the
// superseded one.
func stageTrackerReauthorLineage(rec stageTrackerReauthorRecord, baseURL string) *StageTrackerLineage {
	at := rec.At
	return &StageTrackerLineage{Kind: lineageReauthored, FromNumber: rec.OldPR, FromURL: overviewItemURL("", rec.Repo, rec.OldPR, "pr", baseURL),
		FromAuthorKind: authorKindHive, FromState: lineageFromStateClosed, At: &at, Reason: lineageReasonFixerExhaust}
}

// stageTrackerReauthorSuccessor reports whether PR it is the hive's fresh
// PR for rec: hive-authored, a different number than the superseded one,
// opened after the re-author and closing rec's issue.
func stageTrackerReauthorSuccessor(it StageTrackerItem, rec stageTrackerReauthorRecord) bool {
	if it.Kind != "pr" || it.Number == rec.OldPR || !strings.EqualFold(it.Repo, rec.Repo) {
		return false
	}
	if it.AuthorKind != authorKindHive {
		return false
	}
	if it.CreatedAt != nil && !rec.At.IsZero() && it.CreatedAt.Before(rec.At) {
		return false
	}
	for _, li := range it.LinkedIssues {
		if li.Number == rec.Issue {
			return true
		}
	}
	return false
}

// stageTrackerApplyReauthor stamps re-author lineage on fresh hive PRs
// and the "Re-authoring" caption on issues whose fresh PR is not listed
// yet. Runs before linked PRs are nested so sub-rows inherit the lineage.
func stageTrackerApplyReauthor(items []StageTrackerItem, store *stagelabels.Store, baseURL string, now time.Time) {
	idx := stageTrackerReauthorIndex(store)
	if len(idx) == 0 {
		return
	}
	successor := map[string]bool{}
	for i := range items {
		it := &items[i]
		if it.Kind != "pr" {
			continue
		}
		if issue, ok := stagelabels.ParseReauthoring(it.Reason); ok {
			rec := idx[stagelabels.Key(it.Repo, issue)]
			if rec.OldPR == 0 {
				rec = stageTrackerReauthorRecord{Repo: it.Repo, Issue: issue, OldPR: it.Number, At: now}
			}
			stageTrackerReauthorMotion(it, rec, now)
			continue
		}
		if it.Lineage != nil {
			continue
		}
		for ik, rec := range idx {
			if stageTrackerReauthorSuccessor(*it, rec) {
				it.Lineage = stageTrackerReauthorLineage(rec, baseURL)
				successor[ik] = true
				break
			}
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		ik := stagelabels.Key(it.Repo, it.Number)
		rec, ok := idx[ik]
		if !ok || successor[ik] || it.Stage >= trackerNumberNeedsDecision {
			continue
		}
		if !rec.At.IsZero() && now.Sub(rec.At) > stageTrackerReauthorWindow {
			continue
		}
		stageTrackerReauthorMotion(it, rec, now)
	}
}

// stageTrackerReauthorMotion words a row the hive is re-authoring: lane
// PR-in-progress, caption naming the superseded PR, remediation evidence
// of kind kick (the engine's own record is the pickup).
func stageTrackerReauthorMotion(it *StageTrackerItem, rec stageTrackerReauthorRecord, now time.Time) {
	since := rec.At
	if since.IsZero() {
		since = now
	}
	it.Caption, it.Actor = stageTrackerReauthorCaption(rec.OldPR), authorKindHive
	it.DisplayColumn = trackerNumberPRInProgress
	it.Remediation = &StageTrackerRemediation{Actor: authorKindHive, Kind: remediationKindKick, Since: timePtr(since), Source: stageTrackerReauthorSource}
	detail := "The hive's fixer gave up on #" + strconv.Itoa(rec.OldPR)
	if rec.Attempts > 0 {
		detail += " after " + strconv.Itoa(rec.Attempts) + " attempts"
	}
	detail += ", so the hive closed it as superseded and is opening a fresh PR for #" + strconv.Itoa(rec.Issue) +
		" from the current base (evidence: kick, " + stageTrackerReauthorSource + ", since " + since.UTC().Format(time.RFC3339) + "). One automatic re-author per issue; a second exhaustion parks for a decision."
	it.CodeActivity = &StageTrackerActivity{Head: it.Head, State: codeStateRemediating, Caption: it.Caption, Detail: detail,
		Actor: authorKindHive, Source: stageTrackerReauthorSource, Since: timePtr(since), ObservedAt: timePtr(now), Active: true}
}

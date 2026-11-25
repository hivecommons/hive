package dashboard

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerTriagePR names the open contributor PR on an issue still
// waiting on a maintainer's triage (StageTrackerItem.TriagePR).
type StageTrackerTriagePR struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
	Author string `json:"author,omitempty"`
}

// stageTrackerTriagePRCaption is the caption of a triage-waiting issue with
// an open contributor PR: the maintainer's real decision is "accept the
// issue and review PR #N" — accepting it is also what releases the PR's
// merge-side reporter-trust hold (pkg/github/pr_reporter_trust.go).
func stageTrackerTriagePRCaption(number int) string {
	return "Triage needed · contributor PR #" + strconv.Itoa(number) + " open — accept the issue to review the PR, or close"
}

// stageTrackerWaitsOnTriage reports whether an issue row waits on a
// maintainer's triage: stage 015, the needs-triage pill (only kept on a
// pre-claim stage, stage_tracker_build.go), or a 000-new reporter-trust
// hold. It is the server twin of the SPA's stageTrackerTriageNeeded.
func stageTrackerWaitsOnTriage(it *StageTrackerItem) bool {
	if it == nil || it.Kind != "issue" {
		return false
	}
	if it.triageWaiting {
		return true
	}
	if it.Stage == stagelabels.TriageNeeded.Number() || slices.Contains(it.Modifiers, modNeedsTriage) {
		return true
	}
	return it.New.TriagePending()
}

// stageTrackerApplyTriagePR marks every triage-waiting issue whose linked
// PRs (resolved by stageTrackerResolveLinkedPRs) include an open PR the
// hive did not author (hivecommons/hive#19843). The enumerator's reporter
// gate skips such an issue before the PR-claims pass can call it "covered
// by external PR", so without this the row reads a bare "Triage needed"
// with no hint a contributor's PR is ready. Display only.
func stageTrackerApplyTriagePR(items []StageTrackerItem) {
	for i := range items {
		it := &items[i]
		it.TriagePR = nil
		if !stageTrackerWaitsOnTriage(it) {
			continue
		}
		for _, lp := range it.LinkedPRs {
			if lp.Number <= 0 || strings.EqualFold(lp.State, "closed") || lp.SupersededBy > 0 || lp.AuthorKind == authorKindHive {
				continue
			}
			it.TriagePR = &StageTrackerTriagePR{Number: lp.Number, URL: lp.URL, Title: lp.Title, Author: lp.Author}
			it.Caption = stageTrackerTriagePRCaption(lp.Number)
			break
		}
	}
}

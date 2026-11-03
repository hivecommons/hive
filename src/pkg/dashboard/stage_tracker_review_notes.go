package dashboard

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// StageTrackerReviewNote is one blocked review-backlog issue folded under
// its source PR's tracker row.
type StageTrackerReviewNote struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
}

// stageTrackerReviewBacklogTitleRE matches the titles the review backlog
// files, with or without the `[reviewer]` role prefix.
var stageTrackerReviewBacklogTitleRE = regexp.MustCompile(`^\s*(?:\[[^\]]+\]\s*)?Review backlog\b`)

// stageTrackerIsBlockedReviewNote reports whether the item is a review-
// backlog issue still wearing `blocked` for a source PR in its own repo.
func stageTrackerIsBlockedReviewNote(it *StageTrackerItem) bool {
	if it == nil || it.Kind != "issue" || it.BlockedOnPR == nil || it.BlockedOnPR.Number == 0 {
		return false
	}
	if !slices.Contains(it.Modifiers, modBlocked) || !strings.EqualFold(it.BlockedOnPR.Repo, it.Repo) {
		return false
	}
	return stageTrackerReviewBacklogTitleRE.MatchString(it.Title)
}

// stageTrackerFoldReviewNotes names, on each blocked review-backlog issue,
// the source PR it is folded under (ReviewNoteOf) and, on each listed source
// PR, the notes folded under it (ReviewNotes). Notes whose PR the hive no
// longer counts open (merged or closed: the blocked sweep lifts them next)
// stay first-class rows.
func stageTrackerFoldReviewNotes(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		if items[i].Kind == "pr" {
			prs[strings.ToLower(items[i].Repo)+"#"+strconv.Itoa(items[i].Number)] = &items[i]
		}
	}
	for i := range items {
		it := &items[i]
		if !stageTrackerIsBlockedReviewNote(it) {
			continue
		}
		if st := it.BlockedOnPR.State; st == blockedOnPRStateMerged || st == blockedOnPRStateClosed {
			continue
		}
		it.ReviewNoteOf = it.BlockedOnPR.Number
		if pr := prs[strings.ToLower(it.Repo)+"#"+strconv.Itoa(it.ReviewNoteOf)]; pr != nil {
			pr.ReviewNotes = append(pr.ReviewNotes, StageTrackerReviewNote{Number: it.Number, Title: it.Title, URL: it.URL})
		}
	}
	for i := range items {
		if n := items[i].ReviewNotes; len(n) > 1 {
			slices.SortFunc(n, func(a, b StageTrackerReviewNote) int { return a.Number - b.Number })
		}
	}
}

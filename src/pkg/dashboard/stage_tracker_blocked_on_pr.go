package dashboard

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// StageTrackerBlockedOnPR is the pull request a `blocked` issue waits on
// (hivecommons/hive#14952). State is "open" or "merged" when the hive's own
// caches know it (the status snapshot's open PRs, which the forgestate
// overlay feeds, and the recent-merge ring); "" when they do not. No GitHub
// call is ever made to fill it.
type StageTrackerBlockedOnPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// stageTrackerBlockedOnPR names the PR a `blocked` issue waits on, from the
// review-backlog marker in its body when the row carries one, else from the
// enumeration's filtered reason or the stage reason ("blocked on
// owner/repo#N"). The caption then says what lifts the block instead of
// "No stage recorded yet".
func stageTrackerBlockedOnPR(it *StageTrackerItem, body, bucketReason, githubBaseURL string) {
	if it == nil || it.Kind != "issue" || !slices.ContainsFunc(it.Labels, func(l string) bool { return strings.EqualFold(strings.TrimSpace(l), github.BlockedLabel) }) {
		return
	}
	repo, number, ok := github.ParseBlockedOnMarker(body)
	if !ok {
		repo, number, ok = github.ParseBlockedFilterReason(bucketReason)
	}
	if !ok {
		repo, number, ok = github.ParseBlockedFilterReason(it.Reason)
	}
	if !ok {
		return
	}
	it.BlockedOnPR = &StageTrackerBlockedOnPR{Repo: repo, Number: number, URL: overviewItemURL("", repo, number, "pr", githubBaseURL)}
	it.Caption = "Blocked on PR " + stageTrackerBlockerList(it.Repo, []string{repo + "#" + strconv.Itoa(number)}) + " — lifts when it merges"
	it.Actor = "its blocking PR"
}

// stageTrackerResolveBlockedOnPRState fills each BlockedOnPR's state from
// what the response already holds: a PR listed as a row is open, one in the
// recent-merge ring merged.
func stageTrackerResolveBlockedOnPRState(items []StageTrackerItem, merged []StageTrackerMerged) {
	key := func(repo string, number int) string { return strings.ToLower(repo) + "#" + strconv.Itoa(number) }
	state := map[string]string{}
	for _, it := range items {
		if it.Kind == "pr" {
			state[key(it.Repo, it.Number)] = "open"
		}
	}
	for _, m := range merged {
		state[key(m.Repo, m.Number)] = "merged"
	}
	for i := range items {
		if b := items[i].BlockedOnPR; b != nil {
			b.State = state[key(b.Repo, b.Number)]
		}
	}
}

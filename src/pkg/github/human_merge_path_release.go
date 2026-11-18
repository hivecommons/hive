package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/writeback"
)

// CurrentHumanMergePathMatches reads PR number's current head and its
// complete changed-file list, and returns the head SHA and the changed files
// that match patterns, with the matcher every merge lane uses
// (intent.HumanMergePathMatches). An unreadable PR or an incomplete file list
// is an error: the caller cannot tell that a human-merge-path hold's reason is
// gone, and must keep treating it as present.
func CurrentHumanMergePathMatches(ctx context.Context, client *gh.Client, owner, repo string, number int, patterns []string) (string, []string, error) {
	if client == nil || len(patterns) == 0 {
		return "", nil, fmt.Errorf("human-merge-path check: no client or no paths configured")
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", nil, fmt.Errorf("fetching PR head: %w", err)
	}
	files, err := ListPRChangedFiles(ctx, client, owner, repo, pr)
	if err != nil {
		return "", nil, err
	}
	return pr.GetHead().GetSHA(), intent.HumanMergePathMatches(files, patterns), nil
}

// humanMergePathNoLongerApplies reports whether PR number's current head
// provably touches none of the repo's auto_merge.human_merge_paths: paths are
// configured, the complete changed-file list was read, and no file matches.
// Anything it cannot establish answers false, so the hold source stays.
func (c *Client) humanMergePathNoLongerApplies(ctx context.Context, owner, repo string, number int) bool {
	patterns := c.humanMergePathsFor(owner + "/" + repo)
	if len(patterns) == 0 || c.client == nil {
		return false
	}
	_, matches, err := CurrentHumanMergePathMatches(ctx, c.client, owner, repo, number, patterns)
	return err == nil && len(matches) == 0
}

// OtherHoldSourcesForPR lists what else, besides the human-merge-path
// refusal, explains the hold on PR number of repo ("owner/name"): this hive's
// pause label, the reporter-trust extra label, any other hold label, Hive's
// local hold record and every Hive App hold notice among comments. It makes no
// GitHub call. The stage-label pass lifts a hold whose human-merge-path reason
// is gone only when this is empty, so a hold with a second reason stays. A
// nil client answers unknown.
func (c *Client) OtherHoldSourcesForPR(repo string, number int, labels []string, comments []*gh.IssueComment) []string {
	if c == nil {
		return []string{HoldSourceUnknown}
	}
	var rest []string
	for _, label := range labels {
		if !strings.EqualFold(strings.TrimSpace(label), HumanMergePathHoldLabel) {
			rest = append(rest, label)
		}
	}
	set := map[string]bool{}
	class := c.classifyHoldLabels(rest)
	if class.paused {
		set[HoldSourceDashboardPause] = true
	}
	if class.reporterTrust {
		set[HoldSourceReporterTrust] = true
	}
	if class.otherHeld {
		set[HoldSourceUnknown] = true
	}
	holdSourcesMu.Lock()
	ledger := readHoldSourceLedgerLocked(HoldSourcesPath)
	holdSourcesMu.Unlock()
	for src := range ledger.Applied[holdSourceKey(HoldKindPR, repo, number)] {
		set[src] = true
	}
	for src := range c.holdNoticeSources(comments) {
		set[src] = true
	}
	delete(set, HoldSourceHumanMergePath)
	return orderedHoldSources(set)
}

func validHumanMergeHead(head string) bool {
	if len(head) != 40 {
		return false
	}
	for _, c := range head {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func humanMergeHeadMarker(head string) string {
	return "<!-- hive-human-merge-path-head: " + head + " -->"
}

// ReleaseHumanMergePathHold is shared by pin-only admission callers. qualified
// must mean the enabled policy qualified expectedHead using complete evidence,
// not merely that the diff contains SHA-shaped text. This only removes a label;
// it authorizes neither a merge nor bypassing any other merge gate.
//
// Ambiguous provenance (including legacy markers, edited comments, multiple hold
// applications, missing identity, or unavailable history) leaves the hold intact.
// Callers must supply the process writeback gate (nil fails closed), re-read
// labels after release, and retain exact-head merge binding.
func ReleaseHumanMergePathHold(ctx context.Context, client *gh.Client, gate *writeback.Gate, owner, repo string, number int, appLogin, expectedHead string, qualified bool) (bool, error) {
	if !qualified || gate == nil {
		return false, nil
	}
	allowed := func(item writeback.Item) bool {
		return gate.Allowed(owner+"/"+repo, item, writeback.KindHoldGateNotice).Allowed
	}
	return releaseHumanMergePathHold(ctx, client, allowed, owner, repo, number, appLogin, expectedHead, false)
}

// ReleaseApprovedHumanMergePathHold removes the `hold` the human-merge-path
// gate put on PR number once a trusted merger explicitly approved
// expectedHead on a repository listed in auto_merge.human_merge_owner_approval
// (the caller establishes both). Unlike the pin-only release it may lift a
// hold recorded for expectedHead itself, which is the point: the person
// approved exactly what the hold was put on. The provenance rules are the
// pin-only release's, widened only for the hold the gate re-adds on a later
// head (it posts no second notice): every `hold` the PR was ever labelled
// with must be the App's, the first of them added within a minute before
// the App's single, unedited, head-stamped notice, and the hold the PR
// wears now the latest of them. A hold a person added or re-added at any
// point is never removed. allowed is the caller's write-back decision for
// the item (nil fails closed). It only removes a label: CI, soak, review and
// every other merge gate still apply.
func ReleaseApprovedHumanMergePathHold(ctx context.Context, client *gh.Client, allowed func(writeback.Item) bool, owner, repo string, number int, appLogin, expectedHead string) (bool, error) {
	return releaseHumanMergePathHold(ctx, client, allowed, owner, repo, number, appLogin, expectedHead, true)
}

// releaseHumanMergePathHold is the shared body. approved selects the
// owner-approval rules described on ReleaseApprovedHumanMergePathHold;
// otherwise the pin-only rules apply (a stale head only, exactly one hold
// application).
func releaseHumanMergePathHold(ctx context.Context, client *gh.Client, allowed func(writeback.Item) bool, owner, repo string, number int, appLogin, expectedHead string, approved bool) (bool, error) {
	if client == nil || allowed == nil || strings.TrimSpace(appLogin) == "" || !validHumanMergeHead(expectedHead) {
		return false, nil
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return false, fmt.Errorf("fetching human-merge-path head: %w", err)
	}
	if pr.GetHead().GetSHA() != expectedHead {
		return false, nil
	}
	held := false
	for _, label := range pr.Labels {
		held = held || strings.EqualFold(label.GetName(), HumanMergePathHoldLabel)
	}
	if !held {
		return false, nil
	}

	var notice *gh.IssueComment
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing path hold comments: %w", err)
		}
		for _, comment := range comments {
			if !strings.Contains(comment.GetBody(), HumanMergePathMarker) {
				continue
			}
			// A second marker is ambiguous, even if authored by the App.
			if notice != nil || !strings.EqualFold(comment.GetUser().GetLogin(), appLogin) || comment.GetUser().GetType() != "Bot" {
				return false, nil
			}
			body := comment.GetBody()
			const prefix = "<!-- hive-human-merge-path-head: "
			start := strings.Index(body, prefix)
			if start < 0 {
				return false, nil
			}
			tail := strings.TrimPrefix(body[start:], prefix)
			if len(tail) < 44 || !validHumanMergeHead(tail[:40]) || !strings.HasPrefix(tail[40:], " -->") {
				return false, nil
			}
			if comment.GetCreatedAt().IsZero() || !comment.GetUpdatedAt().Time.Equal(comment.GetCreatedAt().Time) {
				return false, nil
			}
			// The pin-only release lifts only a stale path hold, never the
			// currently refused head; an owner's approval of that head may.
			if !approved && tail[:40] == expectedHead {
				return false, nil
			}
			notice = comment
		}
		if resp == nil {
			return false, nil
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if notice == nil {
		return false, nil
	}

	// Pin-only: exactly one hold transition in the complete history, owned
	// by the configured App and immediately preceding its notice. Older
	// remove/re-add cycles are deliberately not guessed at, even when the
	// latest actor is App. Approved: the first application is that one,
	// every later application is also the App's (the gate re-holding a new
	// head), and the latest transition is an application.
	var first, last *gh.IssueEvent
	eventsOpts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := client.Issues.ListIssueEvents(ctx, owner, repo, number, eventsOpts)
		if err != nil {
			return false, fmt.Errorf("listing path hold timeline: %w", err)
		}
		for _, event := range events {
			if !strings.EqualFold(event.GetLabel().GetName(), HumanMergePathHoldLabel) {
				continue
			}
			if event.GetEvent() != "labeled" && event.GetEvent() != "unlabeled" {
				continue
			}
			appApplied := event.GetEvent() == "labeled" && strings.EqualFold(event.GetActor().GetLogin(), appLogin) && event.GetActor().GetType() == "Bot"
			if first == nil {
				if !appApplied {
					return false, nil
				}
				delta := notice.GetCreatedAt().Sub(event.GetCreatedAt().Time)
				if event.GetCreatedAt().IsZero() || delta < 0 || delta > time.Minute {
					return false, nil
				}
				first, last = event, event
				continue
			}
			if !approved {
				return false, nil
			}
			if event.GetEvent() == "labeled" && !appApplied {
				return false, nil
			}
			last = event
		}
		if resp == nil {
			return false, nil
		}
		if resp.NextPage == 0 {
			break
		}
		eventsOpts.Page = resp.NextPage
	}
	if first == nil || last == nil || last.GetEvent() != "labeled" {
		return false, nil
	}
	// Refetch the head immediately before the write; GitHub label deletion has no
	// conditional-write API, so this is not a substitute for the merge SHA guard.
	current, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if current.GetHead().GetSHA() != expectedHead {
		return false, nil
	}
	item := writeback.Item{
		Number:       number,
		IsPR:         true,
		Author:       current.GetUser().GetLogin(),
		HiveAuthored: strings.EqualFold(current.GetUser().GetLogin(), appLogin) && current.GetUser().GetType() == "Bot",
		CreatedAt:    current.GetCreatedAt().Time,
	}
	for _, label := range current.Labels {
		item.Labels = append(item.Labels, label.GetName())
	}
	if !allowed(item) {
		return false, nil
	}
	if _, err := client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, HumanMergePathHoldLabel); err != nil { // writeback:gated
		return false, fmt.Errorf("releasing path hold: %w", err)
	}
	return true, nil
}

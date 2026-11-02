package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

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
// Callers must re-read labels after release and retain exact-head merge binding.
func ReleaseHumanMergePathHold(ctx context.Context, client *gh.Client, owner, repo string, number int, appLogin, expectedHead string, qualified bool) (bool, error) {
	if !qualified || client == nil || strings.TrimSpace(appLogin) == "" || !validHumanMergeHead(expectedHead) {
		return false, nil
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return false, fmt.Errorf("fetching pin-only head: %w", err)
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
			// Only stale path holds may be released, never the currently refused head.
			if tail[:40] == expectedHead || comment.GetCreatedAt().IsZero() || !comment.GetUpdatedAt().Time.Equal(comment.GetCreatedAt().Time) {
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

	// Require exactly one hold transition in the complete history, owned by the
	// configured App and immediately preceding its notice. Older remove/re-add
	// cycles are deliberately not guessed at, even when the latest actor is App.
	var applied *gh.IssueEvent
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
			if applied != nil || event.GetEvent() != "labeled" || !strings.EqualFold(event.GetActor().GetLogin(), appLogin) || event.GetActor().GetType() != "Bot" {
				return false, nil
			}
			delta := notice.GetCreatedAt().Sub(event.GetCreatedAt().Time)
			if event.GetCreatedAt().IsZero() || delta < 0 || delta > time.Minute {
				return false, nil
			}
			applied = event
		}
		if resp == nil {
			return false, nil
		}
		if resp.NextPage == 0 {
			break
		}
		eventsOpts.Page = resp.NextPage
	}
	if applied == nil {
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
	if _, err := client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, HumanMergePathHoldLabel); err != nil {
		return false, fmt.Errorf("releasing path hold: %w", err)
	}
	return true, nil
}

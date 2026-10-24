package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
)

// delegatedCompetingNumbers reuses the tracker's issue/PR pairing, including
// claim (dibs) links and sidelined PRs. Never infer competition from captions.
func delegatedCompetingNumbers(items []StageTrackerItem, repo string, pr *gh.PullRequest) []int {
	items = append([]StageTrackerItem(nil), items...)
	// The requested head may be newer than the enumeration's link evidence.
	live := StageTrackerItem{Repo: repo, Kind: "pr", Number: pr.GetNumber(), LinkedIssues: stageTrackerPRLinkedIssues(github.PullRequest{Repo: repo, Number: pr.GetNumber(), ReferencedIssues: github.PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber())}, "")}
	found := false
	for i := range items {
		if items[i].Repo == repo && items[i].Kind == "pr" && items[i].Number == live.Number {
			items[i].LinkedIssues = append(append([]StageTrackerLinkedIssue(nil), items[i].LinkedIssues...), live.LinkedIssues...)
			found = true
		} else {
			items[i].LinkedIssues = append([]StageTrackerLinkedIssue(nil), items[i].LinkedIssues...)
		}
		items[i].LinkedPRs = append([]StageTrackerLinkedPR(nil), items[i].LinkedPRs...)
	}
	if !found {
		items = append(items, live)
	}
	stageTrackerResolveLinkedPRs(items)
	seen := map[int]bool{}
	for _, issue := range items {
		if issue.Repo != repo || issue.Kind != "issue" {
			continue
		}
		covers := false
		for _, p := range issue.LinkedPRs {
			if p.Number == live.Number {
				covers = true
			}
		}
		if covers {
			for _, p := range issue.LinkedPRs {
				if p.Number != live.Number {
					seen[p.Number] = true
				}
			}
		}
	}
	numbers := make([]int, 0, len(seen))
	for n := range seen {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	return numbers
}

func (s *Server) gatherDelegatedPRContext(ctx context.Context, repo string, pr *gh.PullRequest, in *delegatedecision.Input) error {
	owner, name, _ := strings.Cut(repo, "/")
	now := time.Now().UTC()
	if !in.HiveAuthored {
		window := config.DefaultPresenceWindow
		if s.deps.Config != nil {
			window = s.deps.Config.Governor.Labels.Stage.EffectivePresenceWindow()
		}
		online, err := s.delegatedContributorOnline(ctx, owner, name, pr, now.Add(-window))
		if err != nil {
			return fmt.Errorf("reading contributor activity: %w", err)
		}
		in.ContributorOnline = online
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return nil
	}
	items := s.buildStageTracker(status, nil, now).Items
	api := s.deps.GHClient.GoGitHub()
	for _, number := range delegatedCompetingNumbers(items, repo, pr) {
		other, _, err := api.PullRequests.Get(ctx, owner, name, number)
		if err != nil {
			return fmt.Errorf("reading competing PR %s#%d: %w", repo, number, err)
		}
		// Enumeration can lag a close/merge; release-line siblings are not duplicates.
		if other.GetState() != "open" || other.GetMerged() || other.GetBase().GetRef() != pr.GetBase().GetRef() {
			continue
		}
		state := strings.ToLower(other.GetMergeableState())
		in.Competing = append(in.Competing, delegatedecision.CompetingPR{
			Number: number, HiveAuthored: s.deps.GHClient.IsHiveAuthorLogin(other.GetUser().GetLogin()),
			Mergeable: other.GetMergeable() && (state == "clean" || state == "has_hooks") && !other.GetDraft(),
			Current:   state != "behind" && state != "unknown" && state != "" && !other.GetDraft(),
		})
	}
	return nil
}

// Only the author's own activity proves presence: bot nudges and other people's
// reviews must not keep an absent contributor online. Read every page before
// declaring absence; API failures abort gathering rather than authorizing adoption.
func (s *Server) delegatedContributorOnline(ctx context.Context, owner, repo string, pr *gh.PullRequest, cutoff time.Time) (bool, error) {
	author := pr.GetUser().GetLogin()
	if author == "" {
		return true, nil
	} // Unknown identity is not evidence of absence.
	recent := func(login string, at time.Time) bool {
		return strings.EqualFold(login, author) && !at.IsZero() && !at.Before(cutoff)
	}
	if !pr.GetCreatedAt().Time.IsZero() && !pr.GetCreatedAt().Time.Before(cutoff) {
		return true, nil
	}
	api := s.deps.GHClient.GoGitHub()
	for page := 1; ; page++ {
		commits, resp, err := api.PullRequests.ListCommits(ctx, owner, repo, pr.GetNumber(), &gh.ListOptions{PerPage: 100, Page: page})
		if err != nil {
			return false, err
		}
		for _, c := range commits {
			if recent(c.GetAuthor().GetLogin(), c.GetCommit().GetAuthor().GetDate().Time) || recent(c.GetCommitter().GetLogin(), c.GetCommit().GetCommitter().GetDate().Time) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
	}
	for page := 1; ; page++ {
		comments, resp, err := api.Issues.ListComments(ctx, owner, repo, pr.GetNumber(), &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
		if err != nil {
			return false, err
		}
		for _, c := range comments {
			if recent(c.GetUser().GetLogin(), c.GetUpdatedAt().Time) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
	}
	for page := 1; ; page++ {
		reviews, resp, err := api.PullRequests.ListReviews(ctx, owner, repo, pr.GetNumber(), &gh.ListOptions{PerPage: 100, Page: page})
		if err != nil {
			return false, err
		}
		for _, r := range reviews {
			if recent(r.GetUser().GetLogin(), r.GetSubmittedAt().Time) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
	}
	// Inline review comments can be posted after the review was submitted.
	for page := 1; ; page++ {
		comments, resp, err := api.PullRequests.ListComments(ctx, owner, repo, pr.GetNumber(), &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
		if err != nil {
			return false, err
		}
		for _, c := range comments {
			if recent(c.GetUser().GetLogin(), c.GetUpdatedAt().Time) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
	}
	return false, nil
}

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// errDelegatedNoSnapshot fails a delegated PR decision closed when the
// dashboard has not enumerated the repo yet: without link evidence a hive
// duplicate would be released against competitors nobody has seen.
var errDelegatedNoSnapshot = errors.New("no dashboard snapshot yet; competing PRs unknown")

// delegatedCompetingNumbers reuses the tracker's issue/PR pairing, including
// claim (dibs) links and sidelined PRs. Never infer competition from captions.
func delegatedCompetingNumbers(items []StageTrackerItem, repo string, pr *gh.PullRequest) []int {
	items = append([]StageTrackerItem(nil), items...)
	// The requested head may be newer than the enumeration's link evidence.
	refs := github.PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber())
	livePR := github.PullRequest{Repo: repo, Number: pr.GetNumber(), ReferencedIssues: refs}
	live := StageTrackerItem{Repo: repo, Kind: "pr", Number: pr.GetNumber(), LinkedIssues: stageTrackerPRLinkedIssues(livePR, repo, "")}
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
	// The items are a tracker response already filtered to open PRs; a
	// link the response kept is kept here too.
	stageTrackerResolveLinkedPRs(items, func(string, int) bool { return true })
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
	in.HumanAuthor = s.delegatedHumanAuthor(in.Labels)
	in.OwnerAuthor = s.isAuthorizedUserLogin(in.Author)
	// A person's PR is never a contributor with an expected connected hive;
	// "offline" has no meaning for it (hivecommons/hive#15202).
	if !in.HiveAuthored && !in.PersonAuthored() {
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
		return errDelegatedNoSnapshot
	}
	// The stage store carries the "covered by PR #N" claim records the
	// snapshot alone does not; the live tracker reads it the same way.
	items := s.buildStageTracker(status, s.stageStoreOrShared(), now).Items
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
		mergeable := other.GetMergeable() && !other.GetDraft() && (state == "clean" || state == "has_hooks")
		if other.GetMergeable() && !other.GetDraft() && state == "blocked" {
			// GitHub says "blocked" for a missing review and for red required
			// checks alike; only the former is a mergeable competitor. A head
			// whose checks never ran (an untrusted fork awaiting ok-to-test)
			// or are still pending has not proven anything yet.
			summary, err := github.SummarizeCommitChecks(ctx, api, owner, name, other.GetHead().GetSHA())
			if err != nil {
				return fmt.Errorf("reading checks for competing PR %s#%d: %w", repo, number, err)
			}
			mergeable = len(summary.Red) == 0 && summary.Pending == 0 && summary.Green > 0
		}
		winner, _ := delegatedTrainDuplicate(s.stageStoreOrShared(), repo, number)
		in.Competing = append(in.Competing, delegatedecision.CompetingPR{
			Number: number, HiveAuthored: s.deps.GHClient.IsHiveAuthorLogin(other.GetUser().GetLogin()),
			Mergeable:     mergeable,
			Current:       state != "behind" && state != "unknown" && state != "" && !other.GetDraft(),
			HeldDuplicate: winner == pr.GetNumber(),
		})
	}
	in.TrainDuplicateOf, in.TrainDuplicateIssue = delegatedTrainDuplicate(s.stageStoreOrShared(), repo, pr.GetNumber())
	return nil
}

// delegatedTrainDuplicate reads the stage store for the one-PR-per-issue
// hold the merge queue recorded on a PR (hivecommons/hive#15395): the PR
// it yields to and the shared issue, 0 when it is not held.
func delegatedTrainDuplicate(store *stagelabels.Store, repo string, number int) (winner, issue int) {
	if store == nil {
		return 0, 0
	}
	rec, ok := store.Get(stagelabels.Key(repo, number))
	if !ok {
		return 0, 0
	}
	mt, ok := prstage.ParseMergeTrain(rec.Reason)
	if !ok || mt.State != prstage.MergeTrainStateDuplicateOf {
		return 0, 0
	}
	return mt.Front, mt.Position
}

// delegatedActivity is one author-attributed event on a PR.
type delegatedActivity struct {
	Login string
	At    time.Time
}

// Only the author's own activity proves presence: bot nudges and other people's
// reviews must not keep an absent contributor online. Read every page before
// declaring absence; API failures abort gathering rather than authorizing adoption.
// delegatedHumanAuthor reads the author-kind label (author/human by default,
// renamed via governor.labels.author) from the PR's labels. hive beats
// clanker beats human when labels disagree, as in pkg/authorkind.
func (s *Server) delegatedHumanAuthor(labels []string) bool {
	human, clanker, hive := authorKindLabelPfx+authorKindHuman, authorKindLabelPfx+authorKindClanker, authorKindLabelPfx+authorKindHive
	if s != nil && s.deps != nil && s.deps.Config != nil {
		a := s.deps.Config.Governor.Labels.Author
		if strings.TrimSpace(a.Human) != "" {
			human = a.Human
		}
		if strings.TrimSpace(a.Clanker) != "" {
			clanker = a.Clanker
		}
		if strings.TrimSpace(a.Hive) != "" {
			hive = a.Hive
		}
	}
	isHuman := false
	for _, l := range labels {
		switch l = strings.TrimSpace(l); {
		case strings.EqualFold(l, hive), strings.EqualFold(l, clanker):
			return false
		case strings.EqualFold(l, human):
			isHuman = true
		}
	}
	return isHuman
}

// isAuthorizedUserLogin: login is one of this hive's dashboard.authorized_users.
func (s *Server) isAuthorizedUserLogin(login string) bool {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if login == "" || s == nil || s.deps == nil || s.deps.Config == nil {
		return false
	}
	for _, entry := range s.deps.Config.Dashboard.AuthorizedUsers {
		name, _ := dashboardAuthorizedUserEntry(entry)
		if name != "" && strings.EqualFold(name, login) {
			return true
		}
	}
	return false
}

func (s *Server) delegatedContributorOnline(ctx context.Context, owner, repo string, pr *gh.PullRequest, cutoff time.Time) (bool, error) {
	author := pr.GetUser().GetLogin()
	// Unknown identity is not evidence of absence.
	if author == "" {
		return true, nil
	}
	latest := time.Now().UTC().Add(5 * time.Minute)
	recent := func(login string, at time.Time) bool {
		return strings.EqualFold(login, author) && !at.IsZero() && !at.Before(cutoff) && !at.After(latest)
	}
	if recent(author, pr.GetCreatedAt().Time) {
		return true, nil
	}
	// A commit whose email is not linked to any GitHub account has no login;
	// on the author's own branch it is still the author pushing.
	commitLogin := func(u *gh.User) string {
		if login := u.GetLogin(); login != "" {
			return login
		}
		return author
	}
	api := s.deps.GHClient.GoGitHub()
	number := pr.GetNumber()
	sources := []func(page int) ([]delegatedActivity, *gh.Response, error){
		func(page int) ([]delegatedActivity, *gh.Response, error) {
			commits, resp, err := api.PullRequests.ListCommits(ctx, owner, repo, number, &gh.ListOptions{PerPage: 100, Page: page})
			var out []delegatedActivity
			for _, c := range commits {
				out = append(out,
					delegatedActivity{commitLogin(c.GetAuthor()), c.GetCommit().GetAuthor().GetDate().Time},
					delegatedActivity{commitLogin(c.GetCommitter()), c.GetCommit().GetCommitter().GetDate().Time})
			}
			return out, resp, err
		},
		func(page int) ([]delegatedActivity, *gh.Response, error) {
			comments, resp, err := api.Issues.ListComments(ctx, owner, repo, number, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
			var out []delegatedActivity
			for _, c := range comments {
				out = append(out, delegatedActivity{c.GetUser().GetLogin(), c.GetUpdatedAt().Time})
			}
			return out, resp, err
		},
		func(page int) ([]delegatedActivity, *gh.Response, error) {
			reviews, resp, err := api.PullRequests.ListReviews(ctx, owner, repo, number, &gh.ListOptions{PerPage: 100, Page: page})
			var out []delegatedActivity
			for _, r := range reviews {
				out = append(out, delegatedActivity{r.GetUser().GetLogin(), r.GetSubmittedAt().Time})
			}
			return out, resp, err
		},
		// Inline review comments can be posted after the review was submitted.
		func(page int) ([]delegatedActivity, *gh.Response, error) {
			comments, resp, err := api.PullRequests.ListComments(ctx, owner, repo, number, &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
			var out []delegatedActivity
			for _, c := range comments {
				out = append(out, delegatedActivity{c.GetUser().GetLogin(), c.GetUpdatedAt().Time})
			}
			return out, resp, err
		},
	}
	for _, fetch := range sources {
		for page := 1; ; page++ {
			events, resp, err := fetch(page)
			if err != nil {
				return false, err
			}
			for _, e := range events {
				if recent(e.Login, e.At) {
					return true, nil
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
		}
	}
	return false, nil
}

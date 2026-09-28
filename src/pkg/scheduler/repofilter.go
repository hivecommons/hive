package scheduler

import (
	"sort"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

func actionableForRepo(actionable *github.ActionableResult, repo string) *github.ActionableResult {
	if actionable == nil || repo == "" {
		return actionable
	}
	out := *actionable
	out.Issues = github.IssueResultFromItems(filterIssuesByRepo(actionable.Issues.Items, repo))
	out.PRs = github.PRResult{
		Items:       filterPRsByRepo(actionable.PRs.Items, repo),
		StaleDrafts: filterPRsByRepo(actionable.PRs.StaleDrafts, repo),
	}
	out.PRs.Count = len(out.PRs.Items)
	out.Hold = filterHoldByRepo(actionable.Hold, repo)
	out.TotalByRepo = map[string]github.RepoCounts{repo: actionable.TotalByRepo[repo]}
	return &out
}

func filterIssuesByRepo(issues []github.Issue, repo string) []github.Issue {
	filtered := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Repo == repo {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func filterPRsByRepo(prs []github.PullRequest, repo string) []github.PullRequest {
	filtered := make([]github.PullRequest, 0, len(prs))
	for _, pr := range prs {
		if pr.Repo == repo {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}

func filterHoldByRepo(hold github.HoldResult, repo string) github.HoldResult {
	filtered := make([]github.HoldItem, 0, len(hold.Items))
	for _, item := range hold.Items {
		if item.Repo == repo {
			filtered = append(filtered, item)
		}
	}
	out := github.HoldResult{Items: filtered}
	for _, item := range filtered {
		switch item.Type {
		case "pr":
			out.PRs++
		default:
			out.Issues++
		}
	}
	out.Total = len(filtered)
	return out
}

func issueRefsForAgent(agentName string, issues []github.Issue, limit int) []string {
	agentIssues := issues
	if agentName != "scanner" {
		agentIssues = filterByLane(issues, agentName)
	}
	if limit <= 0 {
		limit = maxIssuesPerKick
	}
	if len(agentIssues) > limit {
		agentIssues = agentIssues[:limit]
	}
	refs := make([]string, 0, len(agentIssues))
	seen := make(map[string]bool, len(agentIssues))
	for _, issue := range agentIssues {
		// One canonical key implementation (kubestellar/hive#4245). The old
		// `Number <= 0` skip dropped every Linear and Jira item on the floor:
		// they reach here with Number == 0, so no non-GitHub work was ever
		// referenced in an internal-agent kick at all. issueKey keeps
		// GitHub-backed refs byte-identical "repo#number" and gives external
		// work its own "repo!EXT-1" identity instead of a shared "repo#0".
		ref := issueKey(issue)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs
}

// issueKey is the scheduler's single entry point to the canonical work
// identity. It delegates to pkg/worksource so the scheduler cannot drift into a
// second key format — the parity test in scheduler_worksource_identity_test.go
// pins that it produces exactly what worksource.Ref.Key() does.
func issueKey(issue github.Issue) string {
	return worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}.Key()
}

// issueDisplayRef is the human-facing form written into kick message bodies:
// "owner/repo#42" for GitHub-backed work, "owner/repo!ENG-123" for a
// string-keyed source. It exists so no rendering site formats "%s#%d" directly
// and prints "owner/repo#0" for an item that simply has no issue number.
//
// It falls back to the bare repo when an item carries no usable identity at
// all, which keeps a malformed enumeration readable in the message rather than
// rendering a key nothing can match.
func issueDisplayRef(issue github.Issue) string {
	if key := issueKey(issue); key != "" {
		return key
	}
	return issue.Repo
}

// fairShareByRepo picks which items survive a kick list cap, spreading the
// budget evenly across the repos present instead of filling it from the head
// of the list (hivecommons/hive#7455).
//
// A flat prefix cut spends the whole budget on whichever repos sort first. On
// a 16-repo spoke with 305 open PRs the cap of 50 was exhausted inside the
// third repo, so 13 repos contributed nothing to any kick — and an agent handed
// an issue in one of those repos could not see the open PR already doing that
// work, which is how two PRs get opened for the same change.
//
// Allocation is round-robin over repos in first-appearance order: every repo
// takes one slot per pass until the cap is reached or the items run out. That
// yields an even share without computing one, and a repo holding fewer items
// than its share simply drops out of later passes, redistributing the
// remainder to repos that still have work — "10 from each unless there aren't
// 10 to retrieve".
//
// A limit of config.KickListUnlimited (0) or less returns every item. The
// result preserves the caller's original ordering so the rendered list still
// groups by repo; only membership is decided here.
func fairShareByRepo[T any](items []T, limit int, repoOf func(T) string) []T {
	if limit <= config.KickListUnlimited || len(items) <= limit {
		return items
	}

	order := make([]string, 0, 16)
	pending := make(map[string][]int, 16)
	for i, item := range items {
		repo := repoOf(item)
		if _, seen := pending[repo]; !seen {
			order = append(order, repo)
		}
		pending[repo] = append(pending[repo], i)
	}

	picked := make([]int, 0, limit)
	for len(picked) < limit {
		progressed := false
		for _, repo := range order {
			if len(picked) >= limit {
				break
			}
			queue := pending[repo]
			if len(queue) == 0 {
				continue
			}
			picked = append(picked, queue[0])
			pending[repo] = queue[1:]
			progressed = true
		}
		// Every repo is drained; nothing left to hand out even though the cap
		// has room. Without this the loop spins forever.
		if !progressed {
			break
		}
	}

	sort.Ints(picked)
	out := make([]T, 0, len(picked))
	for _, i := range picked {
		out = append(out, items[i])
	}
	return out
}

func filterByLane(issues []github.Issue, lane string) []github.Issue {
	var result []github.Issue
	for _, issue := range issues {
		if issue.Lane == lane || issue.Lane == "" {
			result = append(result, issue)
		}
	}
	return result
}

// filterIssuesForRepos keeps only the issues in repos the predicate accepts.
func filterIssuesForRepos(issues []github.Issue, keep func(repo string) bool) []github.Issue {
	if keep == nil {
		return issues
	}
	out := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if keep(issue.Repo) {
			out = append(out, issue)
		}
	}
	return out
}

// buildReposSectionFor renders the AUTHORIZED REPOS block for one agent.
//
// For an unscoped agent — every agent on a hive that does not use per-repo
// custom agents (#6204) — this is the hive's repo list, unchanged. For a scoped
// agent it is that agent's repos, and the section says out loud that the hive
// watches more: an agent that has filed issues in a repo for weeks and suddenly
// does not see it would otherwise read the shorter list as scope loss and file
// a finding about it.
// activeReposForAgent splits the repos the named agent serves (#6204) into the
// ones it may act on this session and the ones the operator has paused (#6203).
// The two narrowings compose rather than override: a scope says which repos an
// agent is FOR, a pause says which repos are open for work at all, and what
// reaches a kick is the intersection.
func (s *Scheduler) activeReposForAgent(agentName string) (active, paused []string) {
	repos := s.cfg.ReposForAgent(agentName)
	active = make([]string, 0, len(repos))
	for _, repo := range repos {
		if s.cfg.IsRepoPaused(repo) {
			paused = append(paused, repo)
			continue
		}
		active = append(active, repo)
	}
	return active, paused
}

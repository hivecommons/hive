package reach

import (
	"sort"
	"time"
)

// PRInfo is the merged-PR input to the join: its number, merge commit, merge
// time and changed files (from the hub's GitHub client).
type PRInfo struct {
	Number      int       `json:"number"`
	Title       string    `json:"title,omitempty"`
	MergeCommit string    `json:"merge_commit"`
	MergedAt    time.Time `json:"merged_at"`
	Files       []string  `json:"-"`
}

// WindowAssignment is the D4 (#3973/#3994) co-attribution result for one PR.
// PRs whose merge commits land between the same two DEPLOYED commits ride
// the same deploy and therefore share reach/latency/error results BY
// CONSTRUCTION — the shared set is labeled, never hidden behind fake per-PR
// precision.
type WindowAssignment struct {
	// DeployedCommit is the first deployed commit whose history contains the
	// PR's merge commit — the commit that actually shipped the PR. Empty
	// when no deployed commit contains the PR yet (merged, not deployed).
	DeployedCommit string `json:"deployed_commit,omitempty"`
	// SharedWith lists the OTHER PR numbers in the same deploy window,
	// sorted ascending. Empty when the deploy was 1:1.
	SharedWith []int `json:"shared_with,omitempty"`
}

// AssignWindows groups PRs into deploy windows. deployedOldestFirst is the
// sequence of commits observed actually running in the fleet (the
// digest-anchored deploy history — see DeployedCommits), oldest first. Each
// PR is assigned to the EARLIEST deployed commit that contains its merge
// commit; PRs assigned to the same deployed commit share a window.
//
// Ancestry errors abort the assignment: a partial answer would silently
// mis-attribute reach across windows.
func AssignWindows(deployedOldestFirst []string, prs []PRInfo, anc Ancestry) (map[int]WindowAssignment, error) {
	// windowMembers collects PR numbers per closing deployed commit.
	windowMembers := map[string][]int{}
	assignments := make(map[int]WindowAssignment, len(prs))

	for _, pr := range prs {
		assigned := ""
		for _, deployed := range deployedOldestFirst {
			contained, err := anc.IsAncestor(pr.MergeCommit, deployed)
			if err != nil {
				return nil, err
			}
			if contained {
				assigned = deployed
				break
			}
		}
		assignments[pr.Number] = WindowAssignment{DeployedCommit: assigned}
		if assigned != "" {
			windowMembers[assigned] = append(windowMembers[assigned], pr.Number)
		}
	}

	// Second pass: label each deployed PR with its co-windowed peers.
	for prNumber, assignment := range assignments {
		if assignment.DeployedCommit == "" {
			continue
		}
		var shared []int
		for _, peer := range windowMembers[assignment.DeployedCommit] {
			if peer != prNumber {
				shared = append(shared, peer)
			}
		}
		sort.Ints(shared)
		assignment.SharedWith = shared
		assignments[prNumber] = assignment
	}
	return assignments, nil
}

// DeployedCommits derives the digest-anchored deploy history from the
// fleet's reach reports: the distinct commits hives have REPORTED RUNNING
// (never tags or Deployment specs — the #3816 anchoring rule), ordered by
// the earliest time each commit was first seen executing. This is the
// deployedOldestFirst input to AssignWindows.
func DeployedCommits(reports map[string][]ComponentReach) []string {
	firstSeen := map[string]time.Time{}
	for _, entries := range reports {
		for _, e := range entries {
			if e.Commit == "" {
				continue
			}
			if t, ok := firstSeen[e.Commit]; !ok || e.FirstSeen.Before(t) {
				firstSeen[e.Commit] = e.FirstSeen
			}
		}
	}
	commits := make([]string, 0, len(firstSeen))
	for c := range firstSeen {
		commits = append(commits, c)
	}
	sort.Slice(commits, func(i, j int) bool {
		ti, tj := firstSeen[commits[i]], firstSeen[commits[j]]
		if ti.Equal(tj) {
			return commits[i] < commits[j]
		}
		return ti.Before(tj)
	})
	return commits
}

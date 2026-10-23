package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// The your-turn critical path (hivecommons/hive#13646, RFC #13617 P1).
//
// After the ownership decision has marked which items are your turn, every
// your-turn issue and pull request gets what it waits on, how many other
// your-turn items it unblocks, which open PRs it may conflict with and the
// one action that moves it. The graph's hard edges are:
//
//   - an explicit `Depends-on: #N` line in a PR body (authoritative; the
//     author knows the order);
//   - an issue waits on the open PR whose closing link (`Fixes/Closes/
//     Resolves #N`, or GitHub's own closing reference) names it;
//   - an issue waits on its open GitHub "blocked by" issues;
//   - a PR whose failing checks are red on its base branch waits on that
//     check, and on the open PR that fixes the red default branch.
//
// File overlap between open PRs is never a hard edge: it is only the soft
// may_conflict_with hint. Nothing here changes which items are your turn.

// Kinds of a WaitsOn edge.
const (
	waitsOnKindPR        = "pr"
	waitsOnKindIssue     = "issue"
	waitsOnKindExternal  = "external"
	waitsOnKindBaseCheck = "base-check"
	waitsOnKindApproval  = "approval"
	waitsOnKindMerge     = "merge"
)

// The action named on each your-turn row.
const (
	criticalActionMerge       = "merge"
	criticalActionReleaseHold = "release-hold"
	criticalActionRebase      = "rebase"
	criticalActionRerun       = "rerun"
	criticalActionApprove     = "approve"
)

// WaitsOn is one thing a your-turn item waits on. Ref is "#N" for an item of
// the same repository, "owner/repo#N" for another one, and the check name
// for a base-check edge; approval and merge edges carry no ref.
type WaitsOn struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
}

// CriticalPath is the critical-path view of one your-turn issue or PR.
// Unblocks counts the other your-turn items that wait on this one, directly
// or transitively. RootCause is true when the item waits on no other
// your-turn item; otherwise Blocker names the root cause ("#N" or
// "owner/repo#N") it collapses under on the dashboard. Action is one of
// merge, release-hold, rebase, rerun or approve; ActionRef names the item
// the action is relative to ("rebase after #90").
type CriticalPath struct {
	WaitsOn         []WaitsOn `json:"waits_on"`
	Unblocks        int       `json:"unblocks"`
	MayConflictWith []int     `json:"may_conflict_with"`
	Action          string    `json:"action"`
	ActionRef       string    `json:"action_ref,omitempty"`
	RootCause       bool      `json:"root_cause"`
	Blocker         string    `json:"blocker,omitempty"`
}

// YourTurnRef names one item of a repository's your-turn list.
type YourTurnRef struct {
	Kind   string `json:"kind"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// YourTurnGroup is one collapsed row of a repository card: the your-turn
// items of the repository that wait on the same root cause.
type YourTurnGroup struct {
	Blocker      YourTurnRef   `json:"blocker"`
	BlockerTitle string        `json:"blocker_title,omitempty"`
	Dependents   []YourTurnRef `json:"dependents"`
}

// MainRedSummary is the repository card's red-main banner input: the red
// required check, since when, and the fix PR with whether it is your turn.
type MainRedSummary struct {
	Check       string    `json:"check"`
	Since       time.Time `json:"since"`
	Fix         int       `json:"fix,omitempty"`
	FixYourTurn bool      `json:"fix_your_turn,omitempty"`
}

// RepoYourTurn is a repository's your-turn summary. Issues and PRs count
// the your-turn items that are not CLOSE (the inbox's "N your turn");
// RootIssues, RootPRs and RootCauseCount count those that are root causes,
// the collapsed count. Order lists every your-turn item, unblocks first,
// then oldest first.
type RepoYourTurn struct {
	Issues         int             `json:"issues"`
	PRs            int             `json:"prs"`
	RootIssues     int             `json:"root_issues"`
	RootPRs        int             `json:"root_prs"`
	RootCauseCount int             `json:"root_cause_count"`
	Order          []YourTurnRef   `json:"order"`
	Groups         []YourTurnGroup `json:"groups,omitempty"`
	MainRed        *MainRedSummary `json:"main_red,omitempty"`
}

// FrontendYourTurn is the hive-wide sum of the repositories' RepoYourTurn
// counts. The phone widget's YOUR TURN · ISSUES / PRS cells read RootIssues
// and RootPRs: the collapsed root causes, not every waiting item.
type FrontendYourTurn struct {
	Issues         int `json:"issues"`
	PRs            int `json:"prs"`
	RootIssues     int `json:"root_issues"`
	RootPRs        int `json:"root_prs"`
	RootCauseCount int `json:"root_cause_count"`
}

type cpNode struct {
	key      string
	repoIdx  int
	repo     string
	kind     string
	number   int
	title    string
	created  time.Time
	yourTurn bool
	verb     string
	held     bool
	pr       *github.PullRequest
	issue    *github.Issue
	closes   []string
	waits    []WaitsOn
	deps     []string
	// dependsOnPR are the open PRs named by the body's Depends-on lines.
	dependsOnPR []string
}

type cpGraph struct {
	nodes []*cpNode
	byKey map[string]*cpNode
	alias map[string]string
	repos []FrontendRepo
}

func cpKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// canonicalRepo maps a repository spelling (short name or owner/repo) to the
// snapshot's name for it, or returns it unchanged for an untracked one.
func (g *cpGraph) canonicalRepo(name string) (string, bool) {
	if canonical, ok := g.alias[strings.ToLower(strings.TrimSpace(name))]; ok {
		return canonical, true
	}
	return name, false
}

// refText renders a reference relative to the repository of the item that
// carries it.
func refText(fromRepo, toRepo string, number int) string {
	if strings.EqualFold(fromRepo, toRepo) {
		return "#" + strconv.Itoa(number)
	}
	return toRepo + "#" + strconv.Itoa(number)
}

func newCPGraph(repos []FrontendRepo) *cpGraph {
	g := &cpGraph{byKey: map[string]*cpNode{}, alias: map[string]string{}, repos: repos}
	for i := range repos {
		canonical := overviewRepoName(repos[i])
		for _, name := range []string{repos[i].Name, repos[i].Full} {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				g.alias[name] = canonical
			}
		}
	}
	for i := range repos {
		repo := &repos[i]
		repoName := overviewRepoName(*repo)
		add := func(n *cpNode) {
			if n.number <= 0 {
				return
			}
			n.repoIdx, n.repo = i, repoName
			n.key = cpKey(repoName, n.number)
			if o := ownershipFor(repo.Ownership, n.kind, n.number); o != nil {
				n.yourTurn, n.verb = o.YourTurn, o.Verb
			}
			if prev, ok := g.byKey[n.key]; ok {
				// A PR row wins over an issues-feed echo of the same number.
				if prev.kind == ownershipKindPR || n.kind != ownershipKindPR {
					return
				}
				for j := range g.nodes {
					if g.nodes[j] == prev {
						g.nodes = append(g.nodes[:j], g.nodes[j+1:]...)
						break
					}
				}
			}
			g.byKey[n.key] = n
			g.nodes = append(g.nodes, n)
		}
		issueRows := func(items []any, held bool) {
			for _, raw := range items {
				issue, ok := frontendIssue(raw)
				if !ok {
					continue
				}
				add(&cpNode{kind: ownershipKindIssue, number: issue.Number, title: issue.Title, created: issue.CreatedAt, held: held, issue: &issue})
			}
		}
		issueRows(repo.ActionableIssues, false)
		issueRows(repo.HeldIssues, true)
		for _, raw := range repo.NonActionableIssues {
			if item, ok := repoWorkIssueRow(raw); ok {
				add(&cpNode{kind: ownershipKindIssue, number: item.Number, title: item.Title, created: item.CreatedAt})
			}
		}
		prRows := func(items []any, held bool) {
			for _, raw := range items {
				entry, ok := frontendPullRequest(raw)
				if !ok {
					continue
				}
				pr := entry.pr
				add(&cpNode{kind: ownershipKindPR, number: pr.Number, title: pr.Title, created: pr.CreatedAt, held: held, pr: &pr})
			}
		}
		prRows(repo.OpenPrs, false)
		prRows(repo.HeldPrs, true)
		// Sidelined PRs (drafts, exempt labels) are open: an item that
		// depends on one still waits. They are never your turn here.
		for _, raw := range repo.NonActionablePrs {
			if item, ok := repoWorkPRRow(raw); ok {
				add(&cpNode{kind: ownershipKindPR, number: item.Number, title: item.Title, created: item.CreatedAt})
			}
		}
	}
	return g
}

func repoWorkPRRow(raw any) (github.RepoWorkPR, bool) {
	switch v := raw.(type) {
	case github.RepoWorkPR:
		return v, true
	case *github.RepoWorkPR:
		if v == nil {
			return github.RepoWorkPR{}, false
		}
		return *v, true
	}
	return github.RepoWorkPR{}, false
}

// prClosingKeys are the issues a PR closes: GitHub's closing references and
// the closing keywords the enumeration read from its body.
func (g *cpGraph) prClosingKeys(n *cpNode) []string {
	seen := map[string]bool{}
	var out []string
	add := func(repo string, number int) {
		if number <= 0 {
			return
		}
		canonical, _ := g.canonicalRepo(nonEmpty(repo, n.repo))
		key := cpKey(canonical, number)
		if !seen[key] && key != n.key {
			seen[key] = true
			out = append(out, key)
		}
	}
	for _, linked := range n.pr.LinkedIssues {
		add(linked.Repo, linked.Number)
	}
	for _, ref := range n.pr.ReferencedIssues {
		if ref.Via == github.PRLinkViaFixes {
			add(ref.Repo, ref.Number)
		}
	}
	sort.Strings(out)
	return out
}

// edgeTo adds a hard edge from n to the item repo#number. An item of a
// tracked repository that is not open in the snapshot is done, so the edge
// is dropped; one in an untracked repository is kept as external.
func (g *cpGraph) edgeTo(n *cpNode, repo string, number int, fallbackKind string) {
	if number <= 0 {
		return
	}
	canonical, tracked := g.canonicalRepo(nonEmpty(repo, n.repo))
	key := cpKey(canonical, number)
	if key == n.key {
		return
	}
	for _, dep := range n.deps {
		if dep == key {
			return
		}
	}
	target, open := g.byKey[key]
	switch {
	case open:
		n.waits = append(n.waits, WaitsOn{Kind: target.kind, Ref: refText(n.repo, target.repo, number)})
		n.deps = append(n.deps, key)
	case !tracked:
		n.waits = append(n.waits, WaitsOn{Kind: waitsOnKindExternal, Ref: refText(n.repo, canonical, number)})
	case fallbackKind != "":
		// A GitHub blocked-by link GitHub still reports open.
		n.waits = append(n.waits, WaitsOn{Kind: fallbackKind, Ref: refText(n.repo, canonical, number)})
	}
}

func splitItemKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, "#")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return key[:i], n, true
}

func (g *cpGraph) buildEdges() {
	closedBy := map[string][]*cpNode{}
	for _, n := range g.nodes {
		if n.pr == nil {
			continue
		}
		n.closes = g.prClosingKeys(n)
		for _, key := range n.closes {
			closedBy[key] = append(closedBy[key], n)
		}
	}
	for _, n := range g.nodes {
		if n.kind == ownershipKindPR {
			if n.pr != nil {
				g.buildPREdges(n)
			}
			continue
		}
		prs := closedBy[n.key]
		sort.Slice(prs, func(a, b int) bool { return prs[a].number < prs[b].number })
		for _, pr := range prs {
			g.edgeTo(n, pr.repo, pr.number, "")
		}
		if n.issue != nil {
			for _, key := range n.issue.OpenBlockers() {
				if repo, number, ok := splitItemKey(key); ok {
					g.edgeTo(n, repo, number, waitsOnKindIssue)
				}
			}
		}
		if n.waits == nil {
			n.waits = []WaitsOn{}
		}
	}
}

func (g *cpGraph) buildPREdges(n *cpNode) {
	pr := n.pr
	for _, dep := range pr.DependsOn {
		g.edgeTo(n, dep.Repo, dep.Number, "")
	}
	// The PRs this one declares it goes after: it is rebased once they land.
	for _, w := range n.waits {
		if w.Kind == waitsOnKindPR {
			n.dependsOnPR = append(n.dependsOnPR, w.Ref)
		}
	}
	if len(pr.BaseRedChecks) > 0 {
		for _, check := range pr.BaseRedChecks {
			n.waits = append(n.waits, WaitsOn{Kind: waitsOnKindBaseCheck, Ref: check})
		}
		if fix := g.repos[n.repoIdx].RedMain.PrimaryFix(); fix != nil {
			g.edgeTo(n, n.repo, fix.Number, "")
		}
	}
	if prNeedsApproval(*pr) {
		n.waits = append(n.waits, WaitsOn{Kind: waitsOnKindApproval})
	}
	if len(n.waits) == 0 {
		n.waits = []WaitsOn{{Kind: waitsOnKindMerge}}
	}
}

// prNeedsApproval reports a PR GitHub says still needs an approving review.
func prNeedsApproval(pr github.PullRequest) bool {
	return pr.Protection != nil && pr.Protection.ReviewDecision == github.ReviewDecisionReviewRequired
}

// reach walks the edges from start (forward along deps, or backward along
// the reverse map) and returns every node reached, start excluded. The
// visited set guards cycles.
func reach(start string, next func(string) []string) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range next(cur) {
			if !seen[k] {
				seen[k] = true
				queue = append(queue, k)
			}
		}
	}
	delete(seen, start)
	return seen
}

// criticalAction names the one action that moves a your-turn item.
func criticalAction(n *cpNode) (string, string) {
	if n.held {
		return criticalActionReleaseHold, ""
	}
	if n.pr == nil {
		for _, w := range n.waits {
			if w.Kind == waitsOnKindPR {
				return criticalActionMerge, w.Ref
			}
		}
		return criticalActionApprove, ""
	}
	pr := n.pr
	if len(n.dependsOnPR) > 0 {
		return criticalActionRebase, n.dependsOnPR[0]
	}
	if pr.Mergeable == github.MergeableNo || pr.MergeableState == "dirty" || pr.MergeableState == "behind" {
		return criticalActionRebase, ""
	}
	if len(pr.BaseRedChecks) > 0 || pr.CIStatus == "failure" {
		// Re-run once the red base is fixed, when a fix PR is open.
		for _, w := range n.waits {
			if w.Kind == waitsOnKindPR {
				return criticalActionRerun, w.Ref
			}
		}
		return criticalActionRerun, ""
	}
	if prNeedsApproval(*pr) {
		return criticalActionApprove, ""
	}
	return criticalActionMerge, ""
}

// mayConflictWith lists the other open PRs of n's repository that touch a
// file n touches (when both file lists are known) or close an issue n
// closes. It is a soft hint: never a waits_on edge.
func (g *cpGraph) mayConflictWith(n *cpNode) []int {
	out := []int{}
	if n.pr == nil {
		return out
	}
	hard := map[string]bool{}
	for _, d := range n.deps {
		hard[d] = true
	}
	paths := map[string]bool{}
	for _, p := range n.pr.ChangedPaths {
		paths[p] = true
	}
	closes := map[string]bool{}
	for _, c := range n.closes {
		closes[c] = true
	}
	for _, other := range g.nodes {
		if other == n || other.pr == nil || other.repoIdx != n.repoIdx || hard[other.key] {
			continue
		}
		overlap := false
		for _, p := range other.pr.ChangedPaths {
			if paths[p] {
				overlap = true
				break
			}
		}
		for _, c := range other.closes {
			if overlap {
				break
			}
			overlap = closes[c]
		}
		if overlap {
			out = append(out, other.number)
		}
	}
	sort.Ints(out)
	return out
}

// computeCriticalPath stamps CriticalPath on every your-turn row of repos
// (repo.CriticalPath, keyed by ownershipKey) and the your-turn summary on
// each repository, and returns the hive-wide counts. It reads the stored
// ownership decisions and changes none of them.
func computeCriticalPath(repos []FrontendRepo) *FrontendYourTurn {
	g := newCPGraph(repos)
	g.buildEdges()
	reverse := map[string][]string{}
	for _, n := range g.nodes {
		for _, d := range n.deps {
			reverse[d] = append(reverse[d], n.key)
		}
	}
	forward := func(k string) []string {
		if n, ok := g.byKey[k]; ok {
			return n.deps
		}
		return nil
	}
	backward := func(k string) []string { return reverse[k] }

	var yt []*cpNode
	for _, n := range g.nodes {
		if n.yourTurn {
			yt = append(yt, n)
		}
	}
	unblocks := map[string]int{}
	reaches := map[string]map[string]bool{}
	for _, n := range yt {
		count := 0
		for k := range reach(n.key, backward) {
			if g.byKey[k] != nil && g.byKey[k].yourTurn {
				count++
			}
		}
		unblocks[n.key] = count
		ytReach := map[string]bool{}
		for k := range reach(n.key, forward) {
			if g.byKey[k] != nil && g.byKey[k].yourTurn {
				ytReach[k] = true
			}
		}
		reaches[n.key] = ytReach
	}
	// A root cause waits on no other your-turn item. Items that wait on each
	// other in a cycle with no way out would leave none, so the smallest key
	// of such a cycle stands in as its root.
	root := map[string]bool{}
	for _, n := range yt {
		terminal, smallest := true, true
		for k := range reaches[n.key] {
			if !reaches[k][n.key] {
				terminal = false
				break
			}
			if k < n.key {
				smallest = false
			}
		}
		root[n.key] = terminal && smallest
	}
	blocker := map[string]string{}
	for _, n := range yt {
		if root[n.key] {
			continue
		}
		best := ""
		for k := range reaches[n.key] {
			if !root[k] {
				continue
			}
			if best == "" || unblocks[k] > unblocks[best] || (unblocks[k] == unblocks[best] && k < best) {
				best = k
			}
		}
		blocker[n.key] = best
	}

	for i := range repos {
		repos[i].CriticalPath = map[string]CriticalPath{}
		repos[i].YourTurn = &RepoYourTurn{Order: []YourTurnRef{}}
		if fact := repos[i].RedMain; fact != nil {
			summary := &MainRedSummary{Check: fact.Check, Since: fact.Since}
			if fix := fact.PrimaryFix(); fix != nil {
				summary.Fix = fix.Number
				if n := g.byKey[cpKey(overviewRepoName(repos[i]), fix.Number)]; n != nil {
					summary.FixYourTurn = n.yourTurn
				}
			}
			repos[i].YourTurn.MainRed = summary
		}
	}
	total := &FrontendYourTurn{}
	groups := map[int]map[string]*YourTurnGroup{}
	perRepo := map[int][]*cpNode{}
	for _, n := range yt {
		action, actionRef := criticalAction(n)
		cp := CriticalPath{
			WaitsOn:         n.waits,
			Unblocks:        unblocks[n.key],
			MayConflictWith: g.mayConflictWith(n),
			Action:          action,
			ActionRef:       actionRef,
			RootCause:       root[n.key],
		}
		if b := g.byKey[blocker[n.key]]; b != nil {
			cp.Blocker = refText(n.repo, b.repo, b.number)
			byBlocker := groups[n.repoIdx]
			if byBlocker == nil {
				byBlocker = map[string]*YourTurnGroup{}
				groups[n.repoIdx] = byBlocker
			}
			group := byBlocker[b.key]
			if group == nil {
				group = &YourTurnGroup{Blocker: YourTurnRef{Kind: b.kind, Repo: b.repo, Number: b.number}, BlockerTitle: b.title}
				byBlocker[b.key] = group
			}
			group.Dependents = append(group.Dependents, YourTurnRef{Kind: n.kind, Repo: n.repo, Number: n.number})
		}
		repos[n.repoIdx].CriticalPath[ownershipKey(n.kind, n.number)] = cp
		perRepo[n.repoIdx] = append(perRepo[n.repoIdx], n)
		if n.verb == OwnershipVerbClose {
			continue
		}
		summary := repos[n.repoIdx].YourTurn
		if n.kind == ownershipKindPR {
			summary.PRs++
			if cp.RootCause {
				summary.RootPRs++
			}
		} else {
			summary.Issues++
			if cp.RootCause {
				summary.RootIssues++
			}
		}
	}
	for i := range repos {
		summary := repos[i].YourTurn
		items := perRepo[i]
		sortByUnblocks(items, unblocks)
		for _, n := range items {
			summary.Order = append(summary.Order, YourTurnRef{Kind: n.kind, Repo: n.repo, Number: n.number})
		}
		for _, group := range groups[i] {
			summary.Groups = append(summary.Groups, *group)
		}
		sort.Slice(summary.Groups, func(a, b int) bool {
			x, y := summary.Groups[a], summary.Groups[b]
			if len(x.Dependents) != len(y.Dependents) {
				return len(x.Dependents) > len(y.Dependents)
			}
			if x.Blocker.Repo != y.Blocker.Repo {
				return x.Blocker.Repo < y.Blocker.Repo
			}
			return x.Blocker.Number < y.Blocker.Number
		})
		for gi := range summary.Groups {
			deps := summary.Groups[gi].Dependents
			sort.Slice(deps, func(a, b int) bool { return deps[a].Number < deps[b].Number })
		}
		summary.RootCauseCount = summary.RootIssues + summary.RootPRs
		total.Issues += summary.Issues
		total.PRs += summary.PRs
		total.RootIssues += summary.RootIssues
		total.RootPRs += summary.RootPRs
	}
	total.RootCauseCount = total.RootIssues + total.RootPRs
	return total
}

// sortByUnblocks orders your-turn items by how many items they unblock, then
// oldest first, then by number.
func sortByUnblocks(items []*cpNode, unblocks map[string]int) {
	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		if unblocks[x.key] != unblocks[y.key] {
			return unblocks[x.key] > unblocks[y.key]
		}
		if !x.created.Equal(y.created) {
			if x.created.IsZero() || y.created.IsZero() {
				return !x.created.IsZero()
			}
			return x.created.Before(y.created)
		}
		if x.number != y.number {
			return x.number < y.number
		}
		return x.kind < y.kind
	})
}

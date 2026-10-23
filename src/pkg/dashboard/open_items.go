package dashboard

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
)

// The Overview tile, its sidebar pill and the Hive activity monitor used
// to count open work from three different places: the enumeration's
// TotalByRepo (or the forgestate cache's open totals), the per-repo
// WorkBreakdown, and the tracker's own walk of the item lists. The three
// drift apart whenever one source moves without the others — a webhook
// re-read drops a closed issue from the item lists and TotalByRepo but
// never touches WorkBreakdown, the forgestate overlay bumps Other without
// any item record behind it, and the tracker nests PRs under their issue
// before it counts rows (epic hivecommons/hive#13373).
//
// This file makes the item lists the single source: every open issue or PR
// a repo's status row carries (actionable, held, and the enumeration's
// sidelined work-detail rows) is the canonical open-items set, counted once
// per (kind, number). The repo's issues/prs totals, its WorkBreakdown
// total, StatusPayload.OverviewTotals and the activity monitor's items all
// derive from that set, so the tile, the pill and the tracker agree by
// construction. A total the enumeration or cache reported beyond the items
// it could list is kept apart as "unlisted", never folded into the count.

// openItemRef is one member of a repo's canonical open-items set.
type openItemRef struct {
	Kind   string // "issue" or "pr"
	Number int
	Held   bool
	// Bucket and Reason are the enumeration's work-detail classification
	// for sidelined items; empty for actionable and held ones.
	Bucket string
	Reason string
	// Issue is set for Kind == "issue" items from the actionable/held lists.
	Issue github.Issue
	// PR is set for Kind == "pr" items from the actionable/held lists.
	PR frontendPR
	// WorkIssue / WorkPR carry the sidelined item's work-detail row; Sidelined
	// says which of the two shapes the item came in.
	Sidelined bool
	WorkIssue github.RepoWorkIssue
	WorkPR    github.RepoWorkPR
}

// repoOpenItems returns the repo's canonical open-items set in a stable
// order: actionable issues, actionable PRs, held issues, held PRs, then the
// enumeration's sidelined issues and PRs. GitHub's issues feed also returns
// pull requests, so a number present in the repo's PR set (or whose URL is a
// /pull/ URL) is never an issue here — the PR record wins. Each (kind,
// number) appears at most once; the first occurrence is kept.
func repoOpenItems(repo FrontendRepo) []openItemRef {
	prNumbers := map[int]bool{}
	for _, held := range []bool{false, true} {
		for _, fp := range frontendRepoPRs(repo, held) {
			prNumbers[fp.pr.Number] = true
		}
	}
	for _, raw := range repo.NonActionablePrs {
		if w, ok := repoWorkPR(raw); ok {
			prNumbers[w.Number] = true
		}
	}
	seen := map[string]bool{}
	var out []openItemRef
	add := func(it openItemRef) {
		key := it.Kind + "#" + strconv.Itoa(it.Number)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, it)
	}
	isPR := func(number int, url string) bool {
		return prNumbers[number] || strings.Contains(url, "/pull/")
	}
	for _, held := range []bool{false, true} {
		for _, issue := range frontendRepoIssues(repo, held) {
			if isPR(issue.Number, issue.URL) {
				continue
			}
			add(openItemRef{Kind: "issue", Number: issue.Number, Held: held, Issue: issue})
		}
		for _, fp := range frontendRepoPRs(repo, held) {
			add(openItemRef{Kind: "pr", Number: fp.pr.Number, Held: held, PR: fp})
		}
	}
	for _, raw := range repo.NonActionableIssues {
		w, ok := repoWorkIssue(raw)
		if !ok || isPR(w.Number, w.URL) {
			continue
		}
		add(openItemRef{Kind: "issue", Number: w.Number, Bucket: w.Bucket, Reason: w.Reason, Sidelined: true, WorkIssue: w})
	}
	for _, raw := range repo.NonActionablePrs {
		if w, ok := repoWorkPR(raw); ok {
			add(openItemRef{Kind: "pr", Number: w.Number, Bucket: w.Bucket, Reason: w.Reason, Sidelined: true, WorkPR: w})
		}
	}
	return out
}

// forgeUnclassifiedReason is the work-detail reason on an item the forge
// cache holds open that the enumeration has no row for.
const forgeUnclassifiedReason = "Open in the forge cache; not yet classified by the scanner"

// seedOpenItemsFromForge adds the forgestate cache's open items the repo's
// lists do not already carry as sidelined work-detail rows, so the count the
// cache reports is backed by items the activity monitor can show rather
// than an Other bucket with nothing behind it. The cache's items are the
// same snapshot its totals come from (forgestate.Counts.OpenItems). Only a
// Ready snapshot seeds; items the enumeration lists but the cache lacks are
// kept — the next enumeration or reconcile settles them.
func seedOpenItemsFromForge(r *FrontendRepo, full string, snap forgestate.Snapshot) int {
	if !snap.Ready {
		return 0
	}
	have := map[string]bool{}
	for _, it := range repoOpenItems(*r) {
		have[it.Kind+"#"+strconv.Itoa(it.Number)] = true
	}
	added := 0
	for _, n := range sortedItemNumbers(snap.PRs) {
		it := snap.PRs[n]
		if it.Closed || have["pr#"+strconv.Itoa(n)] {
			continue
		}
		bucket := "other"
		if it.Draft {
			bucket = "draft"
		}
		r.NonActionablePrs = append(r.NonActionablePrs, github.RepoWorkPR{Repo: full, Number: n, Title: it.Title, Author: it.Author, Labels: append([]string(nil), it.Labels...),
			Draft: it.Draft, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, URL: it.HTMLURL, Bucket: bucket, Reason: forgeUnclassifiedReason})
		have["pr#"+strconv.Itoa(n)] = true
		added++
	}
	for _, n := range sortedItemNumbers(snap.Issues) {
		it := snap.Issues[n]
		if it.Closed || have["issue#"+strconv.Itoa(n)] || have["pr#"+strconv.Itoa(n)] {
			continue
		}
		r.NonActionableIssues = append(r.NonActionableIssues, github.RepoWorkIssue{Repo: full, Number: n, Title: it.Title, Author: it.Author, Labels: append([]string(nil), it.Labels...),
			Assignees: append([]string(nil), it.Assignees...), CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, URL: it.HTMLURL, Bucket: "other", Reason: forgeUnclassifiedReason})
		added++
	}
	return added
}

func sortedItemNumbers(items map[int]forgestate.Item) []int {
	out := make([]int, 0, len(items))
	for n := range items {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// openItemCounts is the canonical set's size by kind.
type openItemCounts struct {
	Issues, PRs int
}

func countOpenItems(items []openItemRef) openItemCounts {
	var c openItemCounts
	for _, it := range items {
		if it.Kind == "pr" {
			c.PRs++
		} else {
			c.Issues++
		}
	}
	return c
}

// reconcileRepoOpenItems makes the repo row's totals and WorkBreakdown
// agree with its canonical open-items set. Issues/PRs become the item
// counts; anything the enumeration or forgestate cache reported above them
// is recorded as UnlistedIssues/UnlistedPRs instead of being folded into the
// total. The WorkBreakdown is left alone when its total already equals the
// item count (the enumeration's bucket attribution is finer than what the
// items carry); otherwise its coarse buckets are rebuilt from the items so
// the Overview partition sums to the same number the tile prints.
func reconcileRepoOpenItems(r *FrontendRepo) openItemCounts {
	items := repoOpenItems(*r)
	counts := countOpenItems(items)
	r.UnlistedIssues = max(0, r.Issues-counts.Issues)
	r.UnlistedPRs = max(0, r.PRs-counts.PRs)
	r.Issues = counts.Issues
	r.PRs = counts.PRs
	if r.WorkBreakdown != nil {
		wb := *r.WorkBreakdown
		if wb.Issues.Total() != counts.Issues {
			wb.Issues = rebuildIssueBreakdown(wb.Issues, items)
		}
		if wb.PRs.Total() != counts.PRs {
			wb.PRs = rebuildPRBreakdown(items)
		}
		r.WorkBreakdown = &wb
	}
	return counts
}

// rebuildIssueBreakdown recounts the coarse issue buckets from the items.
// The named sub-buckets inside Filtered (needs-human, needs-decision,
// needs-spec, reporter-confirmation, exempt, blocked) come from labels the
// item rows do not carry, so they are kept from the enumeration and clamped
// to the recounted Filtered total.
func rebuildIssueBreakdown(prev github.RepoIssueBreakdown, items []openItemRef) github.RepoIssueBreakdown {
	var b github.RepoIssueBreakdown
	for _, it := range items {
		if it.Kind != "issue" {
			continue
		}
		switch {
		case !it.Sidelined && it.Held:
			b.Hold++
		case !it.Sidelined:
			b.Actionable++
		case it.Bucket == "hive_advisory":
			b.HiveAdvisory++
		case it.Bucket == "dependency_dashboard":
			b.DependencyDashboard++
		case it.Bucket == "filtered":
			b.Filtered++
		case it.Bucket == "reporter_triage":
			b.ReporterTriage++
		default:
			b.Other++
		}
	}
	named := []*int{&prev.NeedsHuman, &prev.NeedsDecision, &prev.NeedsSpec, &prev.PossiblyExists, &prev.ReporterConfirmation, &prev.Exempt}
	budget := b.Filtered
	for _, n := range named {
		*n = min(*n, budget)
		budget -= *n
	}
	b.NeedsHuman, b.NeedsDecision, b.NeedsSpec, b.PossiblyExists, b.ReporterConfirmation, b.Exempt = prev.NeedsHuman, prev.NeedsDecision, prev.NeedsSpec, prev.PossiblyExists, prev.ReporterConfirmation, prev.Exempt
	b.Blocked = min(prev.Blocked, b.Filtered)
	return b
}

func rebuildPRBreakdown(items []openItemRef) github.RepoPRBreakdown {
	var b github.RepoPRBreakdown
	for _, it := range items {
		if it.Kind != "pr" {
			continue
		}
		switch {
		case !it.Sidelined && it.Held:
			b.Hold++
		case !it.Sidelined && it.PR.pr.Draft:
			// Stale drafts ride in the actionable list for the draft-lift
			// path but are drafts in the partition.
			b.Draft++
		case !it.Sidelined:
			b.Actionable++
		case it.Bucket == "draft":
			b.Draft++
		case it.Bucket == "filtered":
			b.Filtered++
		default:
			b.Other++
		}
	}
	return b
}

// FrontendOpenItems is the status payload's summary of the canonical
// open-items set: the one count the Overview tile, its sidebar pill and the
// Hive activity monitor all print. Issues and PRs are the item counts
// summed over repos; Buckets splits them the way the Overview equation
// does; Unlisted is work a source reported but could not list, shown beside
// the total rather than inside it. StaleSince is set when any repo's items
// are last-good figures from an earlier read.
type FrontendOpenItems struct {
	Issues     int                     `json:"issues"`
	PRs        int                     `json:"prs"`
	Total      int                     `json:"total"`
	Buckets    FrontendOpenItemBuckets `json:"buckets"`
	Unlisted   FrontendOpenItemKinds   `json:"unlisted"`
	StaleSince string                  `json:"staleSince,omitempty"`
	// IssuesSyncing is true when any repo's issue side is still pending
	// (FrontendRepoOpenItems.IssuesSyncing): the issue total is a lower
	// bound, not a count.
	IssuesSyncing bool                    `json:"issuesSyncing,omitempty"`
	Repos         []FrontendRepoOpenItems `json:"repos"`
}

// FrontendOpenItemKinds is an issues/PRs pair.
type FrontendOpenItemKinds struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
}

// FrontendOpenItemBuckets partitions the set the way the Overview tile's
// equation does: actionable + held + blocked + outside = total.
type FrontendOpenItemBuckets struct {
	Actionable FrontendOpenItemKinds `json:"actionable"`
	Held       FrontendOpenItemKinds `json:"held"`
	Blocked    FrontendOpenItemKinds `json:"blocked"`
	Outside    FrontendOpenItemKinds `json:"outside"`
}

// FrontendRepoOpenItems is one repo's share of the canonical set.
type FrontendRepoOpenItems struct {
	Repo       string                `json:"repo"`
	Issues     int                   `json:"issues"`
	PRs        int                   `json:"prs"`
	Unlisted   FrontendOpenItemKinds `json:"unlisted"`
	Stale      bool                  `json:"stale,omitempty"`
	StaleSince string                `json:"staleSince,omitempty"`
	// IssuesSyncing mirrors FrontendRepo.IssuesSyncing: the issue side of
	// this repo has not been listed for real yet.
	IssuesSyncing bool `json:"issuesSyncing,omitempty"`
}

// markIssuesSyncing enforces the Overview invariant that a repo's issue
// count never silently renders 0 beside open pull requests. A repo that
// lists PRs but no issues while its issue figures are stale or unlisted is
// a pending issue fetch, not an empty backlog: it is flagged IssuesSyncing
// and its counts are marked stale so the tile, the sidebar pill and the
// activity monitor show "syncing" rather than a confident zero. A repo
// whose empty issue list is confirmed (no stale, nothing unlisted) is
// left alone. Call after reconcileRepoOpenItems.
func markIssuesSyncing(r *FrontendRepo, staleSince string) {
	if r.Issues != 0 || r.PRs == 0 {
		return
	}
	if r.UnlistedIssues == 0 && !r.CountsStale {
		return
	}
	r.IssuesSyncing = true
	r.CountsStale = true
	if r.CountsStaleSince == "" {
		r.CountsStaleSince = staleSince
	}
}

// openItemsSummary builds FrontendOpenItems from a status whose repos have
// been through reconcileRepoOpenItems and whose OverviewTotals/ActionableNow
// are already set, so the buckets quote the same equation the tile shows.
func openItemsSummary(status *StatusPayload) FrontendOpenItems {
	var out FrontendOpenItems
	if status == nil {
		return out
	}
	out.Repos = make([]FrontendRepoOpenItems, 0, len(status.Repos))
	for _, repo := range status.Repos {
		row := FrontendRepoOpenItems{
			Repo:     overviewRepoName(repo),
			Issues:   repo.Issues,
			PRs:      repo.PRs,
			Unlisted: FrontendOpenItemKinds{Issues: repo.UnlistedIssues, PRs: repo.UnlistedPRs},
		}
		if repo.CountsStale {
			row.Stale = true
			row.StaleSince = repo.CountsStaleSince
		}
		if repo.IssuesSyncing {
			row.IssuesSyncing = true
			out.IssuesSyncing = true
		}
		out.Issues += row.Issues
		out.PRs += row.PRs
		out.Unlisted.Issues += row.Unlisted.Issues
		out.Unlisted.PRs += row.Unlisted.PRs
		out.Repos = append(out.Repos, row)
	}
	out.Total = out.Issues + out.PRs
	out.StaleSince = overviewStaleSince(status.Repos)
	an := status.ActionableNow
	term := overviewTermCount
	out.Buckets.Actionable = FrontendOpenItemKinds{Issues: an.Issues, PRs: an.PRs}
	out.Buckets.Held = FrontendOpenItemKinds{Issues: term(an.IssueEquation, "held"), PRs: term(an.PREquation, "held")}
	out.Buckets.Blocked = FrontendOpenItemKinds{Issues: term(an.IssueEquation, "blocked_needs_human"), PRs: term(an.PREquation, "blocked_needs_human")}
	out.Buckets.Outside = FrontendOpenItemKinds{Issues: term(an.IssueEquation, "outside"), PRs: term(an.PREquation, "outside")}
	return out
}

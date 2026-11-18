package dashboard

import (
	"fmt"
	"strings"
	"time"
)

// Scope and freshness of the counts in the meeting attendee's hive status
// brief. Two rules from the scored live test of 2026-11-17:
//
//   - Every count carries its scope, worded the same way every time:
//     "across 13 repos" (the configured repo count), or the one repo's name
//     when the hive has one repo or the question is about one repo.
//   - A count that does not cover every configured repo yet is never
//     stated as a total. Just after a restart, or while a repo's issue list
//     is still pending, the brief says "at least N, counted in 5 of 13
//     repos so far" and names it a partial count.
//
// The numbers are the ones GET /api/status serves as openItems.issues and
// openItems.prs (the Overview tiles and the sidebar "BP" pill): the brief
// reads the same derived payload, so there is no second cache to lag. What
// does lag GitHub is the snapshot itself, which the governor republishes
// once per eval cycle; hiveStatusFreshness carries its age.

const (
	// hiveStatusFreshFor is how old the published status snapshot may be
	// before the brief says how old the counts are.
	hiveStatusFreshFor = 3 * time.Minute
	// hiveStatusMaxPendingNamed bounds the repos named as still counting.
	hiveStatusMaxPendingNamed = 3
)

// hiveCountScope is what the open counts cover.
type hiveCountScope struct {
	// Repos is the configured repo count (the rows of openItems.repos); 0
	// when the status carries no per-repo rows.
	Repos int
	// Single is the repo's name when the hive has exactly one.
	Single string
	// IssuesCounted and PRsCounted are the repos whose issue and PR lists
	// are in for real.
	IssuesCounted, PRsCounted int
	// Pending names the repos whose issue list is not in yet.
	Pending []string
	// Syncing is set when the status flags the issue count as a lower
	// bound without per-repo rows to say which repo.
	Syncing bool
}

// hiveStatusCountScope classifies each configured repo. A repo is not
// counted yet when it has no counts at all (CountsIncomplete), or when the
// snapshot is the one restored from disk after a restart and this process
// has not re-read the repo (RestartRefresh with CountsStale). Its issue
// side alone is pending while the issue list has not come back
// (IssuesSyncing, open_items.go markIssuesSyncing).
func hiveStatusCountScope(status *StatusPayload) hiveCountScope {
	oi := status.OpenItems
	sc := hiveCountScope{Repos: len(oi.Repos)}
	if sc.Repos == 0 {
		sc.Syncing = oi.IssuesSyncing
		return sc
	}
	if sc.Repos == 1 {
		sc.Single = oi.Repos[0].Repo
	}
	flags := map[string]FrontendRepo{}
	for _, r := range status.Repos {
		flags[overviewRepoName(r)] = r
	}
	restarted := status.RestartRefresh != nil
	for _, row := range oi.Repos {
		r := flags[row.Repo]
		whole := r.CountsIncomplete || (restarted && r.CountsStale && !r.Paused)
		if !whole {
			sc.PRsCounted++
		}
		if whole || r.IssuesSyncing || row.IssuesSyncing {
			sc.Pending = append(sc.Pending, row.Repo)
			continue
		}
		sc.IssuesCounted++
	}
	return sc
}

// IssuesPartial reports whether the issue count is a lower bound.
func (sc hiveCountScope) IssuesPartial() bool {
	return sc.Syncing || sc.IssuesCounted < sc.Repos
}

// PRsPartial reports whether the PR count is a lower bound.
func (sc hiveCountScope) PRsPartial() bool { return sc.PRsCounted < sc.Repos }

// Partial reports whether any count in the brief is a lower bound.
func (sc hiveCountScope) Partial() bool { return sc.IssuesPartial() || sc.PRsPartial() }

// phrase is the scope of a count over counted of the configured repos.
// Complete: " across 13 repos" (or " in the <name> repo"). Partial:
// ", counted in 5 of 13 repos so far".
func (sc hiveCountScope) phrase(counted int) string {
	switch {
	case sc.Repos == 0:
		return ""
	case counted == 0:
		// Figures exist (restored after a restart, or last-good) but no
		// repo has been confirmed since: "counted in 0 of 14 repos"
		// contradicted the numbers beside it (live test).
		return fmt.Sprintf(", last known figures, not yet confirmed in any of the %d repos", sc.Repos)
	case counted < sc.Repos:
		return fmt.Sprintf(", counted in %d of %d repos so far", counted, sc.Repos)
	case sc.Single != "":
		return " in the " + sc.Single + " repo"
	}
	return fmt.Sprintf(" across %d repos", sc.Repos)
}

// Scope is the scope phrase of a sentence that counts issues and PRs
// together: the narrower of the two.
func (sc hiveCountScope) Scope() string {
	return sc.phrase(min(sc.IssuesCounted, sc.PRsCounted))
}

// atLeast prefixes a lower bound.
func atLeast(partial bool) string {
	if partial {
		return "at least "
	}
	return ""
}

// OpenSentence is "Open now: ..." with the scope, and with every partial
// count worded so it cannot be read as a total.
func (sc hiveCountScope) OpenSentence(issues, prs int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Open now: %s%d issues", atLeast(sc.IssuesPartial()), issues)
	switch {
	case sc.Syncing:
		b.WriteString(" (the issue count is still syncing: a partial count, not a total)")
		fmt.Fprintf(&b, " and %d PRs", prs)
	case sc.PRsPartial():
		fmt.Fprintf(&b, " and at least %d PRs%s (a partial count, not a total%s)", prs, sc.Scope(), sc.pendingNote("still counting"))
	case sc.IssuesPartial():
		fmt.Fprintf(&b, "%s (a partial count, not a total%s), and %d PRs%s", sc.phrase(sc.IssuesCounted),
			sc.pendingNote("issue list still syncing for"), prs, sc.phrase(sc.PRsCounted))
	default:
		fmt.Fprintf(&b, " and %d PRs%s", prs, sc.Scope())
	}
	return b.String()
}

// pendingNote names the repos still being counted, capped.
func (sc hiveCountScope) pendingNote(label string) string {
	if len(sc.Pending) == 0 {
		return ""
	}
	names := sc.Pending
	more := len(names) - hiveStatusMaxPendingNamed
	if more > 0 {
		names = names[:hiveStatusMaxPendingNamed]
	}
	out := "; " + label + " " + strings.Join(names, ", ")
	if more > 0 {
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}

// Guide is the one line that tells the reader how to word the scope, so
// every count answer carries it identically.
func (sc hiveCountScope) Guide() string {
	switch {
	case sc.Repos == 0:
		return ""
	case sc.Partial() && min(sc.IssuesCounted, sc.PRsCounted) == 0:
		return fmt.Sprintf(" Scope of every count here: the hive has %d repos and none is confirmed yet, so these are last known figures: say \"at least\" and \"last known figures, not yet confirmed in any of the %d repos\", never a total.",
			sc.Repos, sc.Repos)
	case sc.Partial():
		return fmt.Sprintf(" Scope of every count here: the hive has %d repos and not all are counted yet, so say \"at least\" and \"counted in %d of %d repos so far\", never a total.",
			sc.Repos, min(sc.IssuesCounted, sc.PRsCounted), sc.Repos)
	case sc.Single != "":
		return " Scope of every count here: the " + sc.Single + " repo; say it with each count."
	}
	return fmt.Sprintf(" Scope of every count here: across %d repos; say \"across %d repos\" with each count, or for a question about one repo give that repo's own count and name the repo.",
		sc.Repos, sc.Repos)
}

// hiveStatusFreshness says how old the counts are when they are known to
// be stale: the snapshot is older than hiveStatusFreshFor, some repos serve
// last-good figures (StaleSince), or the count is partial. Empty for a
// fresh, complete snapshot, so a normal answer carries no caveat.
func hiveStatusFreshness(status *StatusPayload, sc hiveCountScope, now time.Time) string {
	var out string
	staleSince := firstNonEmpty(status.StaleSince, status.OpenItems.StaleSince)
	read, err := time.Parse(time.RFC3339, status.Timestamp)
	known := err == nil && !read.IsZero()
	age := now.Sub(read)
	if known && (age > hiveStatusFreshFor || sc.Partial() || staleSince != "") {
		out += " These counts were read " + hiveStatusAge(age) + " ago."
	}
	if staleSince != "" {
		out += " Some counts are last-good figures from " + staleSince
		if since, err := time.Parse(time.RFC3339, staleSince); err == nil && now.After(since) {
			out += " (" + hiveStatusAge(now.Sub(since)) + " old)"
		}
		out += "."
	}
	if rr := status.RestartRefresh; rr != nil {
		out += fmt.Sprintf(" The hive restarted and has re-read %d of %d repos so far.", rr.ReposScanned, rr.ReposTotal)
	}
	if out != "" {
		out += " Say how old the counts are in one short clause."
	}
	return out
}

// hiveStatusAge words a duration for speech: "under a minute", "4
// minutes", "2 hours", "3 days".
func hiveStatusAge(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < day:
		return plural(int(d.Hours()), "hour")
	}
	return plural(int(d/day), "day")
}

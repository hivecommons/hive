package hiveadvisor

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Band keys as the dashboard Overview classifier spells them. The advisor
// never re-derives a band: it trusts the key the caller classified each item
// into, so the digest and the Overview donuts cannot disagree.
const (
	PRBandBlocked  = "blocked"
	PRBandWaiting  = "waiting"
	IssueBandWait  = "waiting"
	IssueBandDone  = "done"
	IssueBandAgent = "agent-filed"

	// Export paths served by the dashboard (#9102). Links are relative to the
	// dashboard origin; the renderer prefixes them when it knows the origin.
	prExportPath    = "/api/overview/prs.csv"
	issueExportPath = "/api/overview/issues.csv"

	// maxItems caps the per-recommendation item list.
	maxItems = 5
)

// PRItem is one open (or held) pull request as the Overview classifier saw
// it. Band and the cause flags come straight from the classifier; the advisor
// only aggregates them.
type PRItem struct {
	Repo   string
	Number int
	URL    string
	// Band is the Overview band key: waiting, eligible, blocked, in-review,
	// open, or draft.
	Band string
	// Held is true for a PR parked behind a hold (dashboard hold list or a
	// hold label). Held PRs sit in the waiting band.
	Held bool
	// NeedsHuman and NeedsDecision mirror the needs-human and
	// needs-decision/2-discussing labels — the two human-gate causes the
	// waiting band groups together.
	NeedsHumanReason string
	NeedsHuman       bool
	NeedsDecision    bool
	// CIFailing, Conflict and VerdictBlocked are the three blocked-band
	// causes. They are not exclusive: a PR can fail CI and have conflicts.
	CIFailing      bool
	FailingChecks  []string
	Conflict       bool
	VerdictBlocked bool
	VerdictReason  string
	// Lane is the agent lane that produced the PR (agent/<role> label or hive
	// attribution), empty for a human author.
	Lane   string
	Author string
	Stale  bool
	// AgeDays counts days since the PR was opened; IdleDays days since its
	// last activity. The caller computes both against its own clock.
	AgeDays  int
	IdleDays int
}

// IssueItem is one open (or held) issue as the Overview classifier saw it.
type IssueItem struct {
	Repo   string
	Number int
	URL    string
	// Band is the Overview band key: ready, in-progress, agent-filed,
	// waiting, or done.
	Band string
	// Blocked, NeedsDecision and Discussing split the waiting band by label.
	Blocked       bool
	NeedsDecision bool
	Discussing    bool
	AgeDays       int
	IdleDays      int
}

// BandRule is the one-line rule text the Overview tooltips and pill legend
// print for a band. Kind is "pr" or "issue".
type BandRule struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Label string `json:"label"`
	Rule  string `json:"rule"`
}

// Queue is the Overview chart's per-item breakdown: every open and held item
// across every repo card, already classified into bands. It is deliberately
// NOT the governor's actionable queue (Signals.QueueIssues/QueuePRs), which
// excludes held, draft and human-gated items — see Counts.
type Queue struct {
	PRs        []PRItem
	Issues     []IssueItem
	PRBands    []BandRule
	IssueBands []BandRule
	// StaleDays is the classifier's inactivity threshold, quoted in the
	// stale-PR advice so "stale" has a number beside it.
	StaleDays int
}

// Thresholds are the queue-health trigger points. Every field is a whole
// number; the *Pct fields are percentages of the relevant total. Zero fields
// resolve to DefaultThresholds. The defaults are starting points to calibrate
// against real hives, not settled policy — that is why they are configurable.
type Thresholds struct {
	// BlockedPRPct: blocked PRs ≥ this % of open PRs → reduce-blocked-prs.
	BlockedPRPct int
	// NeedsHumanPRPct: waiting PRs ≥ this % of open PRs → clear-human-gate-prs.
	NeedsHumanPRPct int
	// NeedsHumanIssuePct: waiting issues ≥ this % of open issues → unblock-human-issues.
	NeedsHumanIssuePct int
	// StaleBlockedPRs: blocked PRs past the stale threshold ≥ this count → review-stale-prs.
	StaleBlockedPRs int
	// LaneSharePct: one agent lane produced ≥ this % of blocked PRs → throttle-lane.
	LaneSharePct int
	// CheckSharePct: one check fails on ≥ this % of CI-blocked PRs → fix-failing-check.
	CheckSharePct int
}

// DefaultThresholds returns the shipped queue-health trigger points.
func DefaultThresholds() Thresholds {
	return Thresholds{BlockedPRPct: 50, NeedsHumanPRPct: 25, NeedsHumanIssuePct: 30, StaleBlockedPRs: 10, LaneSharePct: 50, CheckSharePct: 50}
}

func (t Thresholds) normalized() Thresholds {
	d := DefaultThresholds()
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return Thresholds{
		BlockedPRPct:       pick(t.BlockedPRPct, d.BlockedPRPct),
		NeedsHumanPRPct:    pick(t.NeedsHumanPRPct, d.NeedsHumanPRPct),
		NeedsHumanIssuePct: pick(t.NeedsHumanIssuePct, d.NeedsHumanIssuePct),
		StaleBlockedPRs:    pick(t.StaleBlockedPRs, d.StaleBlockedPRs),
		LaneSharePct:       pick(t.LaneSharePct, d.LaneSharePct),
		CheckSharePct:      pick(t.CheckSharePct, d.CheckSharePct),
	}
}

// Link is a list the advice points at, relative to the dashboard origin.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Item names one issue or PR the advice is about. Note says why it is listed
// (the failing check, "conflict", the hold reason...).
type Item struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Counts states which totals the advice quotes. The governor pair is the
// actionable queue that drives mode and cadence; the Overview pair is every
// open or held item across repo cards, which is what the queue-health rules
// and the Overview donuts count. A hive can legitimately report a governor
// queue of 0 alongside 93 open PRs: the difference is the held, draft,
// in-review and human-gated items the governor does not act on.
type Counts struct {
	GovernorIssues int `json:"governorIssues"`
	GovernorPRs    int `json:"governorPRs"`
	OverviewIssues int `json:"overviewIssues"`
	OverviewPRs    int `json:"overviewPRs"`
}

type bucket struct {
	key   string
	count int
	items []Item
}

// topBuckets returns buckets sorted by count desc, key asc.
func topBuckets(m map[string]*bucket) []bucket {
	out := make([]bucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].key < out[j].key
	})
	return out
}

func addBucket(m map[string]*bucket, key string, item Item) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	b := m[key]
	if b == nil {
		b = &bucket{key: key}
		m[key] = b
	}
	b.count++
	b.items = append(b.items, item)
}

// prBreakdown is the aggregated PR side of a Queue.
type prBreakdown struct {
	open                                         int
	blocked, blockedCI, blockedConflict, verdict int
	waiting, held, needsHuman, needsDecision     int
	staleBlocked                                 int
	oldestBlockedDays                            int
	checks, verdicts, lanes, authors, repos      []bucket
	blockedItems, waitingItems, staleItems       []Item
}

type issueBreakdown struct {
	open                                     int
	waiting, blocked, needsDecision, discuss int
	done, agentFiled                         int
	waitingItems, doneItems                  []Item
}

func summarizePRs(prs []PRItem) prBreakdown {
	var b prBreakdown
	checks, verdicts, lanes, authors, repos := map[string]*bucket{}, map[string]*bucket{}, map[string]*bucket{}, map[string]*bucket{}, map[string]*bucket{}
	sorted := append([]PRItem(nil), prs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].IdleDays != sorted[j].IdleDays {
			return sorted[i].IdleDays > sorted[j].IdleDays
		}
		if sorted[i].Repo != sorted[j].Repo {
			return sorted[i].Repo < sorted[j].Repo
		}
		return sorted[i].Number < sorted[j].Number
	})
	for _, pr := range sorted {
		b.open++
		switch pr.Band {
		case PRBandBlocked:
			b.blocked++
			item := Item{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Note: prBlockedNote(pr)}
			b.blockedItems = append(b.blockedItems, item)
			if pr.CIFailing {
				b.blockedCI++
				for _, check := range pr.FailingChecks {
					addBucket(checks, check, Item{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Note: "fails " + strings.TrimSpace(check)})
				}
			}
			if pr.Conflict {
				b.blockedConflict++
			}
			if pr.VerdictBlocked {
				b.verdict++
				addBucket(verdicts, pr.VerdictReason, item)
			}
			addBucket(lanes, pr.Lane, item)
			addBucket(authors, pr.Author, item)
			addBucket(repos, pr.Repo, item)
			if pr.Stale {
				b.staleBlocked++
				b.staleItems = append(b.staleItems, Item{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Note: fmt.Sprintf("idle %dd", pr.IdleDays)})
			}
			if pr.AgeDays > b.oldestBlockedDays {
				b.oldestBlockedDays = pr.AgeDays
			}
		case PRBandWaiting:
			b.waiting++
			b.waitingItems = append(b.waitingItems, Item{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Note: prWaitingNote(pr)})
			if pr.Held {
				b.held++
			}
			if pr.NeedsHuman {
				b.needsHuman++
			}
			if pr.NeedsDecision {
				b.needsDecision++
			}
		}
	}
	b.checks, b.verdicts, b.lanes, b.authors, b.repos = topBuckets(checks), topBuckets(verdicts), topBuckets(lanes), topBuckets(authors), topBuckets(repos)
	return b
}

func summarizeIssues(issues []IssueItem) issueBreakdown {
	var b issueBreakdown
	sorted := append([]IssueItem(nil), issues...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].IdleDays != sorted[j].IdleDays {
			return sorted[i].IdleDays > sorted[j].IdleDays
		}
		if sorted[i].Repo != sorted[j].Repo {
			return sorted[i].Repo < sorted[j].Repo
		}
		return sorted[i].Number < sorted[j].Number
	})
	for _, issue := range sorted {
		b.open++
		switch issue.Band {
		case IssueBandWait:
			b.waiting++
			note := ""
			switch {
			case issue.NeedsDecision:
				b.needsDecision++
				note = "needs-decision"
			case issue.Blocked:
				b.blocked++
				note = "blocked"
			case issue.Discussing:
				b.discuss++
				note = "2-discussing"
			}
			b.waitingItems = append(b.waitingItems, Item{Repo: issue.Repo, Number: issue.Number, URL: issue.URL, Note: note})
		case IssueBandDone:
			b.done++
			b.doneItems = append(b.doneItems, Item{Repo: issue.Repo, Number: issue.Number, URL: issue.URL, Note: "agent-marked done"})
		case IssueBandAgent:
			b.agentFiled++
		}
	}
	return b
}

func prBlockedNote(pr PRItem) string {
	parts := make([]string, 0, 3)
	if pr.CIFailing {
		if len(pr.FailingChecks) > 0 {
			parts = append(parts, "fails "+strings.Join(pr.FailingChecks, ", "))
		} else {
			parts = append(parts, "failing CI")
		}
	}
	if pr.Conflict {
		parts = append(parts, "merge conflict")
	}
	if pr.VerdictBlocked {
		if pr.VerdictReason != "" {
			parts = append(parts, "verdict blocked: "+pr.VerdictReason)
		} else {
			parts = append(parts, "verdict blocked")
		}
	}
	return strings.Join(parts, "; ")
}

func prWaitingNote(pr PRItem) string {
	parts := make([]string, 0, 3)
	if pr.Held {
		parts = append(parts, "held")
	}
	if pr.NeedsHuman {
		note := "needs-human"
		if pr.NeedsHumanReason != "" {
			note += ": " + pr.NeedsHumanReason
		}
		parts = append(parts, note)
	}
	if pr.NeedsDecision {
		parts = append(parts, "needs-decision")
	}
	return strings.Join(parts, ", ")
}

func pct(part, total int) int {
	if total <= 0 {
		return 0
	}
	return part * 100 / total
}

// meetsPct reports part/total ≥ threshold% without floating point.
func meetsPct(part, total, threshold int) bool {
	return total > 0 && part > 0 && part*100 >= threshold*total
}

func capItems(items []Item) []Item {
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	return append([]Item(nil), items...)
}

func exportLink(label, path string, params ...string) Link {
	q := url.Values{}
	for i := 0; i+1 < len(params); i += 2 {
		q.Set(params[i], params[i+1])
	}
	return Link{Label: label, URL: path + "?" + q.Encode()}
}

// queueCandidates is the mode-independent queue-health rule table. Every rule
// runs in every governor mode: a pile of blocked PRs is a problem whether the
// governor calls the hive IDLE or SURGE.
func queueCandidates(q Queue, th Thresholds) []Recommendation {
	th = th.normalized()
	prs := summarizePRs(q.PRs)
	issues := summarizeIssues(q.Issues)
	var out []Recommendation

	if meetsPct(prs.blocked, prs.open, th.BlockedPRPct) {
		r := rec("reduce-blocked-prs", 95, "Reduce the blocked PR queue",
			fmt.Sprintf("%d of %d open PRs (%d%%) are Blocked. %s", prs.blocked, prs.open, pct(prs.blocked, prs.open), blockedFirstAction(prs)),
			sig("overview_open_prs", prs.open), sig("blocked", prs.blocked), sig("blocked_pct", pct(prs.blocked, prs.open)),
			sig("failing_ci", prs.blockedCI), sig("merge_conflict", prs.blockedConflict), sig("verdict_blocked", prs.verdict),
			sig("stale_blocked", prs.staleBlocked), sig("oldest_blocked_days", prs.oldestBlockedDays))
		r.Signals = append(r.Signals, concentrationSignals(prs)...)
		r.Bands = []string{"pr/" + PRBandBlocked}
		r.Links = []Link{exportLink("Blocked PRs (CSV)", prExportPath, "band", PRBandBlocked)}
		if len(prs.repos) > 0 && meetsPct(prs.repos[0].count, prs.blocked, 50) {
			r.Links = append(r.Links, exportLink("Blocked PRs in "+prs.repos[0].key+" (CSV)", prExportPath, "band", PRBandBlocked, "repo", prs.repos[0].key))
		}
		r.Items = capItems(prs.blockedItems)
		out = append(out, r)
	}

	if prs.blockedCI >= 2 && len(prs.checks) > 0 && meetsPct(prs.checks[0].count, prs.blockedCI, th.CheckSharePct) {
		top := prs.checks[0]
		r := rec("fix-failing-check", 88, fmt.Sprintf("Fix check `%s` first", top.key),
			fmt.Sprintf("Check `%s` fails on %d of %d CI-blocked PRs (%d%%); one fix unblocks them all.", top.key, top.count, prs.blockedCI, pct(top.count, prs.blockedCI)),
			sig("check", top.key), sig("failing_prs", top.count), sig("ci_blocked", prs.blockedCI))
		r.Bands = []string{"pr/" + PRBandBlocked}
		r.Links = []Link{exportLink("Blocked PRs (CSV)", prExportPath, "band", PRBandBlocked)}
		r.Items = capItems(top.items)
		out = append(out, r)
	}

	if prs.blocked >= 2 && len(prs.lanes) > 0 && meetsPct(prs.lanes[0].count, prs.blocked, th.LaneSharePct) {
		top := prs.lanes[0]
		r := rec("throttle-lane", 86, fmt.Sprintf("Throttle lane `%s`", top.key),
			fmt.Sprintf("Agent lane `%s` produced %d of %d blocked PRs (%d%%); slow its cadence until its open PRs are fixed or closed.", top.key, top.count, prs.blocked, pct(top.count, prs.blocked)),
			sig("lane", top.key), sig("blocked_from_lane", top.count), sig("blocked", prs.blocked))
		r.Bands = []string{"pr/" + PRBandBlocked}
		r.Links = []Link{exportLink("Blocked PRs (CSV)", prExportPath, "band", PRBandBlocked)}
		r.Items = capItems(top.items)
		out = append(out, r)
	}

	if meetsPct(prs.waiting, prs.open, th.NeedsHumanPRPct) {
		r := rec("clear-human-gate-prs", 84, "Clear the human gate on PRs",
			fmt.Sprintf("%d of %d open PRs (%d%%) wait on a human: %d held, %d labelled needs-human, %d needs-decision — release, review, or close them.",
				prs.waiting, prs.open, pct(prs.waiting, prs.open), prs.held, prs.needsHuman, prs.needsDecision),
			sig("overview_open_prs", prs.open), sig("needs_human", prs.waiting), sig("held", prs.held), sig("needs_human_label", prs.needsHuman), sig("needs_decision", prs.needsDecision))
		r.Bands = []string{"pr/" + PRBandWaiting}
		r.Links = []Link{exportLink("Needs-human PRs (CSV)", prExportPath, "band", PRBandWaiting)}
		r.Items = capItems(prs.waitingItems)
		out = append(out, r)
	}

	if meetsPct(issues.waiting, issues.open, th.NeedsHumanIssuePct) {
		r := rec("unblock-human-issues", 82, "Unblock the human queue",
			fmt.Sprintf("%d of %d open issues (%d%%) need a human: %d needs-decision, %d blocked, %d discussing — decide the oldest first.",
				issues.waiting, issues.open, pct(issues.waiting, issues.open), issues.needsDecision, issues.blocked, issues.discuss),
			sig("overview_open_issues", issues.open), sig("needs_human", issues.waiting), sig("needs_decision", issues.needsDecision), sig("blocked", issues.blocked), sig("discussing", issues.discuss), sig("needs_triage", issues.agentFiled))
		r.Bands = []string{"issue/" + IssueBandWait}
		r.Links = []Link{exportLink("Needs-human issues (CSV)", issueExportPath, "band", IssueBandWait)}
		r.Items = capItems(issues.waitingItems)
		out = append(out, r)
	}

	if prs.staleBlocked >= th.StaleBlockedPRs {
		r := rec("review-stale-prs", 78, "Review stale PRs",
			fmt.Sprintf("%d Blocked PRs untouched > %dd — rebase/fix, or close if obsolete. Stale means no activity past the threshold, not proven dead.", prs.staleBlocked, staleDays(q)),
			sig("stale_blocked", prs.staleBlocked), sig("stale_days", staleDays(q)))
		r.Bands = []string{"pr/" + PRBandBlocked}
		r.Links = []Link{exportLink("Stale blocked PRs (CSV)", prExportPath, "band", PRBandBlocked, "stale", "true")}
		r.Items = capItems(prs.staleItems)
		out = append(out, r)
	}

	if issues.done > 0 {
		r := rec("confirm-and-close", 72, "Close what agents already finished",
			fmt.Sprintf("%d issues are agent-marked done; confirming them shrinks the backlog by %d%%.", issues.done, pct(issues.done, issues.open)),
			sig("overview_open_issues", issues.open), sig("agent_done", issues.done))
		r.Bands = []string{"issue/" + IssueBandDone}
		r.Links = []Link{exportLink("Confirm & close issues (CSV)", issueExportPath, "band", IssueBandDone)}
		r.Items = capItems(issues.doneItems)
		out = append(out, r)
	}
	return out
}

func staleDays(q Queue) int {
	if q.StaleDays > 0 {
		return q.StaleDays
	}
	return 14
}

// blockedFirstAction names the dominant blocked cause and the first thing to
// do about it. Ties resolve CI → conflict → verdict, the order an owner can
// act in fastest.
func blockedFirstAction(b prBreakdown) string {
	switch {
	case b.blockedCI > 0 && b.blockedCI >= b.blockedConflict && b.blockedCI >= b.verdict:
		if len(b.checks) > 0 {
			return fmt.Sprintf("%d of %d fail check `%s` — fix the check before touching cadence.", b.checks[0].count, b.blocked, b.checks[0].key)
		}
		return fmt.Sprintf("%d of %d fail CI — fix the checks before touching cadence.", b.blockedCI, b.blocked)
	case b.blockedConflict > 0 && b.blockedConflict >= b.verdict:
		return fmt.Sprintf("%d of %d have merge conflicts — rebase or close the oldest.", b.blockedConflict, b.blocked)
	case b.verdict > 0:
		if len(b.verdicts) > 0 {
			return fmt.Sprintf("%d of %d carry a blocked sweep verdict (top reason: %s) — read the verdict before touching cadence.", b.verdict, b.blocked, b.verdicts[0].key)
		}
		return fmt.Sprintf("%d of %d carry a blocked sweep verdict — read the verdict before touching cadence.", b.verdict, b.blocked)
	default:
		return "The classifier put them in Blocked without a recorded cause — open the list."
	}
}

func concentrationSignals(b prBreakdown) []Signal {
	var out []Signal
	add := func(name string, buckets []bucket) {
		if len(buckets) == 0 {
			return
		}
		out = append(out, sig(name, fmt.Sprintf("%s (%d, %d%%)", buckets[0].key, buckets[0].count, pct(buckets[0].count, b.blocked))))
	}
	add("top_repo", b.repos)
	add("top_lane", b.lanes)
	add("top_author", b.authors)
	if len(b.checks) > 0 {
		out = append(out, sig("top_check", fmt.Sprintf("%s (%d, %d%%)", b.checks[0].key, b.checks[0].count, pct(b.checks[0].count, b.blockedCI))))
	}
	return out
}

// bandRules resolves the band keys named by recs ("pr/blocked",
// "issue/waiting") to their rule text, deduplicated, in first-mention order.
func bandRules(recs []Recommendation, q Queue) []BandRule {
	var out []BandRule
	seen := map[string]bool{}
	for _, r := range recs {
		for _, key := range r.Bands {
			if seen[key] {
				continue
			}
			seen[key] = true
			kind, band, ok := strings.Cut(key, "/")
			if !ok {
				continue
			}
			rules := q.PRBands
			if kind == "issue" {
				rules = q.IssueBands
			}
			for _, rule := range rules {
				if rule.Key == band {
					rule.Kind = kind
					out = append(out, rule)
					break
				}
			}
		}
	}
	return out
}

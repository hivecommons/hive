package dashboard

import (
	"context"
	"net/http"
	"sort"
	"strconv"
)

// Warp-style triage-level view (#2612 part b). A read-only, LIVE-DERIVED grouping
// of the hive's contribute issues into a lifecycle ladder — Triaging → Ready to
// implement → Implementing → Reviewing → Closed — computed on each request from
// the SAME live signals the Operations tab already shows (the ready-work queue,
// the fleet snapshot's in-flight work) plus the part-(c) PR→issue link. There is
// NO persistent per-issue lifecycle store: a persisted lifecycle is a possible
// future enhancement but is deliberately out of scope here (issues are transient,
// sourced live from GitHub). The whole projection is recomputed per request and
// stays cheap because the PR-link lookups it depends on are themselves cached
// (prLinkResolver, short TTL) and degrade to "no linked PR" on any GitHub error.

// Triage ladder levels. Ordered constants (the slice below fixes render order).
const (
	triageTriaging     = "triaging"     // open, unclaimed, no linked PR — awaiting triage
	triageReady        = "ready"        // admitted to the ready queue, not yet picked up
	triageImplementing = "implementing" // a clanker holds it, or an open linked PR exists
	triageReviewing    = "reviewing"    // linked PR is open + not merged (awaiting review)
	triageClosed       = "closed"       // issue closed, or its linked PR merged
)

// triageLevelOrder pins the ladder's render/count order (early → late lifecycle).
var triageLevelOrder = []string{
	triageTriaging, triageReady, triageImplementing, triageReviewing, triageClosed,
}

// triageLevelLabel is the human-facing name for each level, shown as the group
// heading in the view. Kept beside the constants so the mapping is legible.
var triageLevelLabel = map[string]string{
	triageTriaging:     "Triaging",
	triageReady:        "Ready to implement",
	triageImplementing: "Implementing",
	triageReviewing:    "Reviewing",
	triageClosed:       "Closed",
}

// triageSignals is the live per-issue evidence deriveTriageLevel maps onto a
// ladder rung. Every field is a projection over data the hive already has — the
// ready queue, the fleet snapshot, and the (c) PR-link — so the derivation stays
// a pure function of observable state with no hidden lookups.
type triageSignals struct {
	// closed is true when the issue itself is no longer open (GitHub state). The
	// ready-queue/fleet sources only carry OPEN, admissible issues, so this is set
	// only when a closed issue is surfaced explicitly (e.g. via a merged PR link).
	closed bool
	// inReadyQueue: the issue is admitted to the ready-work queue (ReadyQueue),
	// i.e. it passed every admission/cooldown filter and is not in flight.
	inReadyQueue bool
	// inFleet: a connected clanker currently holds this issue as its task
	// (FleetSnapshot().Work) — it is actively being implemented.
	inFleet bool
	// prLink is the resolved fix-PR for the issue (part c), or nil when none is
	// linked. A merged link implies the work landed; an open link implies review.
	prLink *prLinkStatus
}

// deriveTriageLevel maps live signals onto exactly one Warp-style ladder rung.
// This is the ONE documented place the mapping lives, so it is legible and unit
// testable. The precedence is late-lifecycle-first so the most-advanced evidence
// wins: a merged PR (or a closed issue) is Closed even if the issue also lingers
// in some stale queue; an open PR is Reviewing; active fleet work (or an open PR
// with no review signal yet) is Implementing; an admitted-but-unclaimed queue
// item is Ready; anything else open is Triaging.
//
//	Closed:       issue closed OR linked PR merged
//	Reviewing:    linked PR open + not merged (awaiting review)
//	Implementing: in-flight in the fleet, OR has an open linked PR
//	Ready:        admitted to the ready queue, not yet picked up
//	Triaging:     open, unclaimed, not queued, no linked PR
func deriveTriageLevel(sig triageSignals) string {
	// Closed wins outright: the fix landed (merged PR) or the issue is done.
	if sig.closed || (sig.prLink != nil && sig.prLink.State == prLinkStateMerged) {
		return triageClosed
	}
	// An open linked PR means a fix is up and awaiting review.
	if sig.prLink != nil && sig.prLink.State == prLinkStateOpen {
		// A PR that is up is being implemented/reviewed; if the issue is ALSO held
		// by a clanker it is squarely Implementing, otherwise it is in Reviewing.
		if sig.inFleet {
			return triageImplementing
		}
		return triageReviewing
	}
	// A clanker actively holds it → Implementing.
	if sig.inFleet {
		return triageImplementing
	}
	// Admitted to the ready queue but not yet picked up → Ready to implement.
	if sig.inReadyQueue {
		return triageReady
	}
	// Open, unclaimed, unqueued, unlinked → still Triaging.
	return triageTriaging
}

// triageIssue is one issue placed on the ladder, carrying just enough metadata to
// render a row (and its optional PR badge). It is the wire shape the triage
// endpoint returns and the render helper consumes.
type triageIssue struct {
	Repo   string        `json:"repo"`
	Number int           `json:"number"`
	Title  string        `json:"title"`
	URL    string        `json:"url,omitempty"`
	Level  string        `json:"level"`
	PR     *prLinkStatus `json:"pr,omitempty"`
}

// triageGroup is one ladder rung: its level id/label, the issues on it, and the
// count (== len(Issues), echoed so a client can show a count without the list).
type triageGroup struct {
	Level  string        `json:"level"`
	Label  string        `json:"label"`
	Count  int           `json:"count"`
	Issues []triageIssue `json:"issues"`
}

// triageSnapshot is the full projection: the ordered ladder groups plus the total
// count of placed issues. Recomputed per request; never persisted.
type triageSnapshot struct {
	Groups []triageGroup `json:"groups"`
	Total  int           `json:"total"`
}

// maxTriageIssuesPerLevel bounds how many issues each rung lists in the response,
// so a pathological backlog cannot blow out the JSON/DOM. The count still reflects
// the true total on the rung; only the enumerated list is capped (matching how the
// ready-queue caps its own visible list). Generous enough to show a real backlog.
const maxTriageIssuesPerLevel = 60

// buildTriageSnapshot derives the ladder live from the ready queue + fleet snapshot
// + the PR-link resolver. It issues a bounded PR-link lookup per candidate issue
// (cached, short-TTL, degrade-to-nil) — never blocking on GitHub — and groups the
// results by deriveTriageLevel. Read-only; assigns nothing, persists nothing.
func (s *Server) buildTriageSnapshot(ctx context.Context) triageSnapshot {
	snap := triageSnapshot{}
	if s == nil || s.contributeHub == nil {
		// Still return the empty ladder (all rungs, zero counts) so the UI renders a
		// stable, non-error shell.
		return emptyTriageSnapshot()
	}

	resolver := s.contributePRLinkResolver()

	// In-flight work (Implementing candidates) from the fleet snapshot, keyed the
	// same way activeIssueKeys/selectTask key issues so a fleet item and a queue
	// item line up on the same identity string.
	fleetKeys := map[string]bool{}
	fleetItems := map[string]triageIssue{}
	for _, w := range s.contributeHub.FleetSnapshot().Work {
		if w.Repo == "" || w.Number <= 0 {
			continue
		}
		key := prLinkKey(w.Repo, w.Number)
		fleetKeys[key] = true
		fleetItems[key] = triageIssue{Repo: w.Repo, Number: w.Number, Title: w.Title, URL: issueURLFor(w.Repo, w.Number)}
	}

	// Ready-work queue (Ready / Reviewing / Implementing-via-PR candidates).
	byLevel := map[string][]triageIssue{}
	seen := map[string]bool{}

	place := func(it triageIssue, sig triageSignals) {
		key := prLinkKey(it.Repo, it.Number)
		if seen[key] {
			return
		}
		seen[key] = true
		it.PR = sig.prLink
		it.Level = deriveTriageLevel(sig)
		byLevel[it.Level] = append(byLevel[it.Level], it)
	}

	for _, q := range s.contributeHub.ReadyQueue(readyQueueDefaultLimit) {
		key := prLinkKey(q.Repo, q.Number)
		pr := resolver.resolve(ctx, q.Repo, q.Number)
		place(triageIssue{Repo: q.Repo, Number: q.Number, Title: q.Title, URL: q.URL}, triageSignals{
			inReadyQueue: true,
			inFleet:      fleetKeys[key],
			prLink:       pr,
		})
	}

	// Fleet items not already placed via the queue (an in-flight issue is excluded
	// from the ready queue by design, so it is surfaced here).
	for key, it := range fleetItems {
		if seen[key] {
			continue
		}
		pr := resolver.resolve(ctx, it.Repo, it.Number)
		place(it, triageSignals{inFleet: true, prLink: pr})
	}

	// Assemble the ordered ladder. Every rung is always present (even at zero) so
	// the view is a stable shape; per-rung lists are sorted (repo, number) for a
	// deterministic render and capped.
	for _, level := range triageLevelOrder {
		items := byLevel[level]
		sort.Slice(items, func(i, j int) bool {
			if items[i].Repo != items[j].Repo {
				return items[i].Repo < items[j].Repo
			}
			return items[i].Number < items[j].Number
		})
		count := len(items)
		if len(items) > maxTriageIssuesPerLevel {
			items = items[:maxTriageIssuesPerLevel]
		}
		snap.Groups = append(snap.Groups, triageGroup{
			Level:  level,
			Label:  triageLevelLabel[level],
			Count:  count,
			Issues: items,
		})
		snap.Total += count
	}
	return snap
}

// emptyTriageSnapshot returns the ladder with every rung present and empty, used
// when there is no contribute hub to derive from so the UI still renders a stable
// (zero-count) shell rather than an error.
func emptyTriageSnapshot() triageSnapshot {
	snap := triageSnapshot{}
	for _, level := range triageLevelOrder {
		snap.Groups = append(snap.Groups, triageGroup{
			Level:  level,
			Label:  triageLevelLabel[level],
			Count:  0,
			Issues: []triageIssue{},
		})
	}
	return snap
}

// issueURLFor builds the canonical GitHub issue URL for a "owner/repo" + number,
// used when a source (the fleet snapshot) does not already carry a URL so the
// triage row can still link out.
func issueURLFor(repo string, number int) string {
	if repo == "" || number <= 0 {
		return ""
	}
	return "https://github.com/" + repo + "/issues/" + strconv.Itoa(number)
}

// handleContributeTriage serves the live triage-ladder projection as JSON. GET
// only, read-only, public within the /api/contribute* surface (same posture as
// /queue and /fleet — it exposes only public issue metadata + public PR numbers,
// no tokens, no PII). The Operations-tab triage card fetches this after page load
// so a slow GitHub PR-link lookup never delays the page render.
func (s *Server) handleContributeTriage(w http.ResponseWriter, r *http.Request) {
	snap := s.buildTriageSnapshot(r.Context())
	jsonResponse(w, snap)
}

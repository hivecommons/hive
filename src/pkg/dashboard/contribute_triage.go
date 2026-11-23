package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Triage ladder levels. Ordered constants (the slice below fixes render order).
const (
	triageTriaging     = "triaging"     // open, unclaimed, no linked PR: held back from agents (shown as "Held back")
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
//
// The first rung keeps its "triaging" key but reads "Held back": it holds every
// candidate the queue is not offering to agents right now, for whatever reason
// (tracker, cooldown, open PR, skip label, operator hold and so on), and only a
// few of those are actually waiting for triage.
var triageLevelLabel = map[string]string{
	triageTriaging:     "Held back",
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
//	Triaging:     open, unclaimed, not queued, no linked PR (shown as Held back)
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
	// WithheldReason / WithheldTag / WithheldDetail say why a Held back row is
	// not on offer: the reason code the queue's admission pass recorded (or one
	// of the triageHeld* codes below), a short plain-words tag for the row, and
	// the full sentence for its hover text. Set only on the Held back rung;
	// additive and omitempty, so every other row is unchanged.
	WithheldReason string `json:"withheld_reason,omitempty"`
	WithheldTag    string `json:"withheld_tag,omitempty"`
	WithheldDetail string `json:"withheld_detail,omitempty"`
}

// Held back reasons the admission pass does not record as withheld, named here
// so every Held back row carries a reason. They exist only on this card.
const (
	// triageHeldOperatorHold: an operator parked the issue in the queue. The
	// queue surfaces it as Held rather than withheld (an operator's own
	// decision is never reported as the hive's refusal).
	triageHeldOperatorHold = "operator_hold"
	// triageHeldIssueClosed: the scan's row says the issue is closed, so the
	// admission pass skips it before any gate.
	triageHeldIssueClosed = "issue_closed"
	// triageHeldUnrecorded: the pass recorded no reason for the issue, which
	// happens when its withheld list reached the row limit first.
	triageHeldUnrecorded = "unrecorded"
)

// triageHeldTags is the tag and hover text for each triageHeld* code.
var triageHeldTags = map[string][2]string{
	triageHeldOperatorHold: {"On hold", "Held by an operator in the Contributor Queue"},
	triageHeldIssueClosed:  {"Issue closed", "The last scan saw this issue closed, so the queue skips it"},
	triageHeldUnrecorded:   {withheldUnrecordedTag, "The queue is not offering this issue, and its last pass recorded no reason for it (its withheld list stops at the queue's row limit)"},
}

// triageHeldReason is one Held back row's reason, ready to attach.
type triageHeldReason struct {
	code, tag, detail string
}

// heldReasonFromWithheld turns the admission pass's withheld row into the
// card's reason: its code, a tag (withheldReasonTag, never empty) and its
// existing detail sentence, unchanged except that a needs-triage row says who
// that label holds back. A row with no reason code keeps its empty code and
// detail and reads withheldUnrecordedTag, so the card states nothing the
// withheld list does not.
func heldReasonFromWithheld(item AdmissionWithheldItem, hub config.HubConfig) triageHeldReason {
	code := item.Reason
	detail := item.Detail
	if note := withheldLabelNote(item.SkippedLabel); note != "" && item.Reason == contributorAdmissionReasonLabelSkipped {
		detail = strings.TrimSuffix(detail, ".") + ". " + note
	}
	return triageHeldReason{code: code, tag: withheldReasonTag(item, hub), detail: detail}
}

// heldReasonFor returns the reason for one of the triageHeld* codes; note, when
// set, is the operator's own hold note and is appended to the hover text.
func heldReasonFor(code, note string) triageHeldReason {
	t := triageHeldTags[code]
	r := triageHeldReason{code: code, tag: t[0], detail: t[1]}
	if note = strings.TrimSpace(note); note != "" {
		r.detail += ": " + note
	}
	return r
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

// buildTriageSnapshot derives the ladder live from the raw candidate population,
// ready queue + fleet snapshot + the PR-link resolver. Ready/in-flight issues get
// a bounded PR-link lookup (cached, short-TTL, degrade-to-nil); raw candidates that
// admission withheld are placed directly in Triaging (Held back), each with the
// reason the same admission pass recorded. The latter intentionally
// avoids turning a disabled or heavily filtered repository into one GitHub Search
// request per issue merely to explain why its queue is empty. Read-only; assigns
// nothing, persists nothing.
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

	// why attaches a Held back row's reason. Only that rung carries one: a row
	// that advanced to a later rung is explained by the rung itself.
	place := func(it triageIssue, sig triageSignals, why triageHeldReason) {
		key := prLinkKey(it.Repo, it.Number)
		if seen[key] {
			return
		}
		seen[key] = true
		it.PR = sig.prLink
		it.Level = deriveTriageLevel(sig)
		if it.Level == triageTriaging {
			it.WithheldReason, it.WithheldTag, it.WithheldDetail = why.code, why.tag, why.detail
		}
		byLevel[it.Level] = append(byLevel[it.Level], it)
	}

	// One admission pass gives the Ready queue AND the reasons it withheld the
	// rest. The queue is the same one ReadyQueue returns (collecting reasons
	// never changes admission; TestWithheld_CollectingDoesNotChangeAdmission),
	// and the reasons are the ones GET /api/contribute/queue?withheld=1 shows,
	// so this card adds no lookup of its own.
	var hubCfg config.HubConfig
	if s.deps != nil && s.deps.Config != nil {
		hubCfg = s.deps.Config.Hub
	}
	adm := s.contributeHub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	withheldByKey := make(map[string]AdmissionWithheldItem, len(adm.withheld))
	for _, w := range adm.withheld {
		if w.Number <= 0 {
			continue
		}
		key := prLinkKey(w.Repo, w.Number)
		if _, dup := withheldByKey[key]; !dup {
			withheldByKey[key] = w
		}
	}
	// An unexplained candidate is admitted but past the queue's row limit only
	// when the pass admitted more than the queue lists AND recorded every
	// refusal it made. selectTask offers from the whole admitted set, so such a
	// row is Ready, not Held back.
	pastLimit := adm.offerableTotal > readyQueueDefaultLimit && len(adm.withheld) < readyQueueDefaultLimit

	for _, q := range adm.queue {
		key := prLinkKey(q.Repo, q.Number)
		pr := resolver.resolve(ctx, q.Repo, q.Number)
		place(triageIssue{Repo: q.Repo, Number: q.Number, Title: q.Title, URL: q.URL}, triageSignals{
			// Held rows trail the offerable queue so operators can resume them, but
			// they are not Ready and must not inflate that rung.
			inReadyQueue: !q.Held,
			inFleet:      fleetKeys[key],
			prLink:       pr,
		}, heldReasonFor(triageHeldOperatorHold, q.HeldReason))
	}

	// Fleet items not already placed via the queue (an in-flight issue is excluded
	// from the ready queue by design, so it is surfaced here).
	for key, it := range fleetItems {
		if seen[key] {
			continue
		}
		pr := resolver.resolve(ctx, it.Repo, it.Number)
		place(it, triageSignals{inFleet: true, prLink: pr}, triageHeldReason{})
	}

	// Finally retain raw scanner candidates that did not survive admission. Before
	// this pass, the pure deriveTriageLevel mapping had a Triaging case but the
	// builder never supplied it: it iterated only ReadyQueue and FleetSnapshot, so
	// every disabled/filtered/cooling candidate disappeared and an affected hive
	// rendered a completely empty ladder. These items are the Held back
	// population, each with the reason the admission pass above recorded for
	// it, except admitted issues past the queue's row limit, which are Ready.
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status != nil {
		for _, repo := range status.Repos {
			for _, raw := range repo.ActionableIssues {
				b, err := json.Marshal(raw)
				if err != nil {
					continue
				}
				var issue map[string]any
				if err := json.Unmarshal(b, &issue); err != nil {
					continue
				}
				ref := refFromIssueMap(repo.Full, issue)
				if ref.Number <= 0 || seen[prLinkKey(repo.Full, ref.Number)] {
					continue
				}
				title, _ := issue["title"].(string)
				url, _ := issue["url"].(string)
				it := triageIssue{Repo: repo.Full, Number: ref.Number, Title: title, URL: url}
				state, _ := issue["state"].(string)
				w, withheld := withheldByKey[prLinkKey(repo.Full, ref.Number)]
				switch {
				case withheld:
					place(it, triageSignals{}, heldReasonFromWithheld(w, hubCfg))
				case strings.EqualFold(state, "closed"):
					place(it, triageSignals{}, heldReasonFor(triageHeldIssueClosed, ""))
				case pastLimit:
					place(it, triageSignals{inReadyQueue: true}, triageHeldReason{})
				default:
					place(it, triageSignals{}, heldReasonFor(triageHeldUnrecorded, ""))
				}
			}
		}
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

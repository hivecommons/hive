package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// ── Operations command center: live SSE broadcast + ready-work queue ──────────
//
// This file is PURELY ADDITIVE and READ-ONLY. It exposes the events the hub
// ALREADY records (ActivityEntry — join/leave/pick-up/complete/fail/promote) as a
// Server-Sent-Events stream, plus a read-only snapshot of the ready-work QUEUE
// derived from the SAME ActionableIssues set selectTask offers from. Nothing here
// changes the contributor WS protocol, assignment, credentials, or any control
// behaviour: it is a fan-out of information the dashboard could already poll, made
// live so the Operations tab can render an SRE "command center" (a live queue, a
// travel animation when work is picked up, a dev-log narration, and army framing).
//
// The stream lives under the /api/contribute* path prefix, so isPublicPath
// (server.go) already makes it PUBLIC — the whole clanker page is viewable with no
// auth, and this stream is read-only info, so anonymous viewers may subscribe. The
// gated MUTATION endpoints (trust/revoke/delete) are untouched and still enforced
// by requireContributorWrite.

// sseReplayCap is how many recent ActivityEntry events a freshly-connected SSE
// subscriber is replayed so its page is not blank on load. It is bounded by the
// hub's own maxActivityEntries ring buffer (50); we cap the replay a little lower
// so a reconnect does not re-narrate the entire history every time.
const sseReplayCap = 20

// sseSubscriberBuffer is the per-subscriber channel depth. A slow browser that
// falls this far behind is dropped rather than allowed to back-pressure the hub —
// the broadcast must NEVER block the WS event path (an appended activity entry
// fans out under a non-blocking send). The subscriber simply reconnects and
// replays the recent buffer, so a momentary drop is self-healing.
const sseSubscriberBuffer = 32

// readyQueueDefaultLimit is the default number of ready-work items surfaced in the
// queue snapshot / SSE initial payload. It is the "top of the stack" an operator
// watches get picked off. The full admissible set can be large (the hive routinely
// has ~150+ actionable items), and the operator wants to SEE and REORDER a long
// list, not just the top handful. So the cap is generous — high enough that the
// whole realistic backlog is visible and draggable — but still bounded so a
// pathological backlog can never blow out the JSON payload or the DOM. The queue
// panel is a fixed-height scroll container (.cc-queue), so a long list scrolls
// inside its card rather than stretching the page.
const readyQueueDefaultLimit = 150

// sseEvent is one framed message pushed to subscribers. Type distinguishes the
// initial hydration payload (queue + replay) from subsequent single activity
// events so the client can render each without guessing.
type sseEvent struct {
	Type     string           `json:"type"`               // "activity" | "hello"
	Activity *ActivityEntry   `json:"activity,omitempty"` // set when Type=="activity"
	Replay   []ActivityEntry  `json:"replay,omitempty"`   // set when Type=="hello"
	Queue    []ReadyQueueItem `json:"queue,omitempty"`    // set when Type=="hello"
}

// ReadyQueueItem is one admissible/ready issue in the "queue waiting to be picked
// off". It carries only public issue metadata (repo, number, title, labels) —
// exactly what selectTask reads as its candidate set. No credentials, no prompt.
type ReadyQueueItem struct {
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	Title  string   `json:"title"`
	URL    string   `json:"url,omitempty"`
	Labels []string `json:"labels,omitempty"`
	// MatchesInterest is set true, per VIEWER, when at least one of the issue's
	// labels exactly matches (case-insensitive) a label the viewing contributor
	// declared interest in (issue #2637). It is a SOFT signal the Operations tab
	// uses to highlight and float this row for that viewer; it is NEVER set on the
	// shared/anonymous queue snapshot (no viewer identity there) and NEVER used to
	// exclude an item. omitempty so the anonymous payload is byte-for-byte unchanged.
	MatchesInterest bool `json:"matches_interest,omitempty"`
	// Held is set true when the operator has manually parked this issue via the
	// queue HOLD control (Config.Hub.ContributeQueueHold). A held item is NEVER
	// offered — selectTask excludes it outright and ReadyQueue never sorts it into
	// the offer-eligible set — but it is still SURFACED here (appended after the
	// offerable items) so the Operations tab can render it greyed with an "on hold"
	// badge, letting the operator see and Resume it. Distinct from cooldown: a hold
	// is persistent and only clears when the operator Resumes it. omitempty so a
	// snapshot with no holds is byte-for-byte unchanged.
	Held bool `json:"held,omitempty"`
	// HeldReason is the OPTIONAL short operator note attached when the issue was
	// parked (Config.Hub.ContributeQueueHoldReasons). Only ever set on a Held item;
	// the Operations tab shows it in the on-hold badge tooltip ("On hold — <reason>").
	// Empty when the operator held with no note, so the badge falls back to its
	// generic text. omitempty so a hold without a reason is byte-for-byte unchanged.
	HeldReason string `json:"held_reason,omitempty"`
}

// sseSubscriber is one connected browser. events is the fan-out channel; done is
// closed by the HTTP handler when the client disconnects so the registry can drop
// it. Guarded by the hub's sseMu.
type sseSubscriber struct {
	events chan sseEvent
}

// sseRegistry holds the live SSE subscribers. It is a small struct on the hub so
// the broadcast path (addActivity) can fan out without reaching into HTTP state.
type sseRegistry struct {
	mu   sync.Mutex
	subs map[*sseSubscriber]struct{}
}

func newSSERegistry() *sseRegistry {
	return &sseRegistry{subs: make(map[*sseSubscriber]struct{})}
}

// subscribe registers a new subscriber and returns it. Caller MUST call
// unsubscribe when the client disconnects to avoid a leak.
func (r *sseRegistry) subscribe() *sseSubscriber {
	sub := &sseSubscriber{events: make(chan sseEvent, sseSubscriberBuffer)}
	r.mu.Lock()
	r.subs[sub] = struct{}{}
	r.mu.Unlock()
	return sub
}

// unsubscribe removes a subscriber and closes its channel. Idempotent: a second
// call (e.g. handler cleanup after a broadcast already dropped a wedged sub) is a
// no-op because the map delete guards the close.
func (r *sseRegistry) unsubscribe(sub *sseSubscriber) {
	r.mu.Lock()
	if _, ok := r.subs[sub]; ok {
		delete(r.subs, sub)
		close(sub.events)
	}
	r.mu.Unlock()
}

// broadcast fans an event out to every subscriber with a NON-BLOCKING send. A
// subscriber whose buffer is full is skipped (its browser will reconnect and
// replay) — the broadcast must never block the caller, which is the hub's event
// path. This is the leak-safe, back-pressure-free contract the spec requires.
func (r *sseRegistry) broadcast(ev sseEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for sub := range r.subs {
		select {
		case sub.events <- ev:
		default:
			// Subscriber is behind; drop this event for it rather than block the
			// hub. The client's ring-buffer replay on reconnect recovers state.
		}
	}
}

// count returns the number of live subscribers (used by tests to assert cleanup).
func (r *sseRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

// broadcastActivity is the hook addActivity calls after appending an entry. It
// fans the single new event out to all SSE subscribers. Nil-safe so a hub built
// without a registry (older tests) never panics.
func (h *ContributeWSHub) broadcastActivity(entry ActivityEntry) {
	if h == nil || h.sse == nil {
		return
	}
	e := entry
	h.sse.broadcast(sseEvent{Type: "activity", Activity: &e})
}

// ReadyQueue returns the top-N ready-work items — the admissible issues selectTask
// would offer from, derived from the SAME ActionableIssues candidate set and the
// SAME cooldown / failure / disabled-repo / admission-filter exclusions used at
// assignment time (contribute_ws.go selectTask). Read-only; it assigns nothing.
//
// Ordering note (honest scope): selectTask's richest ordering signal is OWN-WORK
// first (#2390), which is per-CONNECTED-CONTRIBUTOR and therefore meaningless for
// an anonymous queue view. So this shared queue drops that per-viewer partition
// and offers the deterministic scan order (repo order, then issue scan order) with
// the failure-aware exclusions applied — i.e. exactly the candidate set selectTask
// draws from, minus the per-contributor own-work reshuffle. That is the documented
// "simple recent/top-N is acceptable" fallback the spec permits.
func (h *ContributeWSHub) ReadyQueue(limit int) []ReadyQueueItem {
	if limit <= 0 {
		limit = readyQueueDefaultLimit
	}
	out := []ReadyQueueItem{}
	if h == nil || h.server == nil {
		return out
	}

	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return out
	}

	// Which issues are already being worked right now — exclude them from "ready"
	// exactly as selectTask does (an in-flight issue is not waiting to be picked).
	active := h.activeIssueKeys()

	var disabledRepos []string
	var held map[string]struct{}
	var holdReasons map[string]string
	if h.server.deps != nil && h.server.deps.Config != nil {
		disabledRepos = h.server.deps.Config.Hub.DisabledRepos
		held = queueHoldSet(h.server.deps.Config.Hub.ContributeQueueHold)
		holdReasons = h.server.deps.Config.Hub.ContributeQueueHoldReasons
	}

	// heldItems collects issues the operator parked. They are NEVER offered — kept
	// out of `out` (the offer-eligible set that the queue-order sort ranks) — but
	// they are still SURFACED, appended after ordering with Held:true, so the
	// Operations tab renders them greyed with an "on hold" badge and the operator
	// can Resume them. A held key uses the SAME canonical "%s#%d" form selectTask's
	// exclusion uses, so the two stay in lock-step.
	var heldItems []ReadyQueueItem

	// One ledger snapshot for this whole pass (#3845). Built here — not per
	// candidate — so the dependency gate stays cheap, and discarded when the pass
	// ends so the next call re-observes current state.
	sweep := h.newAdmissionSweep()

	for _, repo := range status.Repos {
		if len(repo.ActionableIssues) == 0 {
			continue
		}
		if config.MatchesAny(repo.Full, disabledRepos) || config.MatchesAny(repo.Name, disabledRepos) {
			continue
		}
		for _, raw := range repo.ActionableIssues {
			b, err := json.Marshal(raw)
			if err != nil {
				continue
			}
			var issue map[string]any
			if err := json.Unmarshal(b, &issue); err != nil {
				continue
			}
			number := 0
			switch n := issue["number"].(type) {
			case float64:
				number = int(n)
			case int:
				number = n
			}
			if number == 0 {
				continue
			}
			// Operator HOLD (#queue-hold): a parked issue is never offered, so it is
			// kept OUT of the offer-eligible `out` set. It is still collected into
			// heldItems (tagged Held) so it stays visible-but-dimmed for the operator.
			// Checked before cooldown/active so a held-AND-cooled issue still shows as
			// held (the operator's manual decision is the stronger, persistent signal).
			holdKey := fmt.Sprintf("%s#%d", repo.Full, number)
			if _, isHeld := held[holdKey]; isHeld {
				title, _ := issue["title"].(string)
				url, _ := issue["url"].(string)
				heldItems = append(heldItems, ReadyQueueItem{
					Repo:       repo.Full,
					Number:     number,
					Title:      title,
					URL:        url,
					Labels:     stringSliceFromAny(issue["labels"]),
					Held:       true,
					HeldReason: holdReasons[holdKey], // "" when no note (map miss) — omitempty
				})
				continue
			}
			if h.isTaskInCooldown(repo.Full, number) {
				continue
			}
			if h.isTaskInFailureCooldown(repo.Full, number) {
				continue
			}
			if active[fmt.Sprintf("%s#%d", repo.Full, number)] {
				continue
			}
			decision := h.evaluateContributorNeutralAdmission(sweep, contributorAdmissionCandidate{
				repoFull: repo.Full,
				repoName: repo.Name,
				number:   number,
			})
			if !decision.admitted {
				continue
			}
			title, _ := issue["title"].(string)
			url, _ := issue["url"].(string)
			author, _ := issue["author"].(string)
			labels := stringSliceFromAny(issue["labels"])
			assignees := stringSliceFromAny(issue["assignees"])

			// Apply the SAME admission filters selectTask enforces so the queue
			// shows genuinely-admissible work, not items the filters would reject.
			if h.server.deps != nil && h.server.deps.Config != nil {
				hub := h.server.deps.Config.Hub
				if !config.FilterPasses(title, hub.ContributeDenyTitles, hub.ContributeTitlesMode) ||
					!config.FilterPasses(author, hub.ContributeDenyAuthors, hub.ContributeAuthorsMode) ||
					!config.LabelsFilterPasses(labels, hub.ContributeDenyLabels, hub.ContributeLabelsMode) {
					continue
				}
				// Own-work is meaningless for an anonymous queue view, so pass an
				// empty "self" — an issue assigned solely to OTHERS is skipped, one
				// unassigned stays. This matches selectTask's skip-assigned toggle.
				if hub.ContributeSkipAssignedToOthers && assignedToOthers(assignees, "") {
					continue
				}
			}

			out = append(out, ReadyQueueItem{
				Repo:   repo.Full,
				Number: number,
				Title:  title,
				URL:    url,
				Labels: labels,
			})
		}
	}

	// Operator priority override (#queue-reorder): if the operator dragged items to
	// the front on the Operations tab, offer those FIRST — in exactly the operator's
	// order — and keep everything else in the established scan order behind them. A
	// stable sort keyed on the priority index does this without disturbing the
	// relative order of unpinned items. This only reorders OFFER PRIORITY: every item
	// in `out` already passed the SAME admission / cooldown / disabled-repo /
	// in-flight exclusions above, so a pinned issue that is no longer actionable was
	// never collected and is simply absent (stale keys are skipped, not resurrected).
	if h.server.deps != nil && h.server.deps.Config != nil {
		applyQueueOrder(out, h.server.deps.Config.Hub.ContributeQueueOrder)
	}

	// Cap AFTER ordering so the operator's pinned items are guaranteed to survive the
	// truncation (they sort to the front), not be dropped by an arbitrary scan cut.
	if len(out) > limit {
		out = out[:limit]
	}

	// Held items (#queue-hold) trail ALL offerable work: they are never offered, so
	// they carry no offer priority and sit at the bottom of the display, greyed with
	// a badge. Appended after the cap so a large offerable queue never squeezes the
	// operator's parked rows off-screen — the operator must always be able to see and
	// Resume what they held.
	out = append(out, heldItems...)
	return out
}

// queueOrderIndex builds a "owner/repo#number" -> priority-rank map from the
// operator's ordered override list. Earlier entries rank lower (offered sooner).
// Keys not present rank as "unpinned" for callers (they use a sentinel above the
// map size). Returned map is nil-safe to range over when the override is empty.
func queueOrderIndex(order []string) map[string]int {
	if len(order) == 0 {
		return nil
	}
	idx := make(map[string]int, len(order))
	for i, key := range order {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		// First occurrence wins so a duplicated key keeps its earliest (highest)
		// priority rather than being demoted by a later copy.
		if _, seen := idx[key]; !seen {
			idx[key] = i
		}
	}
	return idx
}

// queueHoldSet builds a "owner/repo#number" -> struct{} membership set from the
// operator's HOLD list (Config.Hub.ContributeQueueHold). A key present here means
// the issue is manually parked and must never be OFFERED. The keys use the SAME
// canonical "%s#%d" (repo.Full # number) form every other admission check builds
// (cooldown / active / failure), so the exclusion cannot silently miss on a
// repo-name spelling mismatch (the #2648 class of bug). nil-safe to range over
// (returns nil for an empty list); membership on a nil map is a clean false.
func queueHoldSet(hold []string) map[string]struct{} {
	if len(hold) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(hold))
	for _, key := range hold {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		set[key] = struct{}{}
	}
	return set
}

// applyQueueOrder stably reorders items so any whose "repo#number" key appears in
// the operator override sort to the front in the operator's order, with unpinned
// items keeping their original relative (scan) order behind them. In place; no-op
// when the override is empty.
func applyQueueOrder(items []ReadyQueueItem, order []string) {
	idx := queueOrderIndex(order)
	if len(idx) == 0 {
		return
	}
	// Unpinned items rank at len(idx) (all pinned ranks are < len(idx)), so they all
	// sort behind every pinned item while SliceStable preserves their scan order.
	rank := func(it ReadyQueueItem) int {
		if r, ok := idx[fmt.Sprintf("%s#%d", it.Repo, it.Number)]; ok {
			return r
		}
		return len(idx)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return rank(items[i]) < rank(items[j])
	})
}

// activeIssueKeys returns the set of "repo#number" keys currently in flight across
// all connected clankers, so ReadyQueue can exclude work that is already picked up.
// Mirrors the activeIssues set selectTask builds, read-only.
func (h *ContributeWSHub) activeIssueKeys() map[string]bool {
	active := map[string]bool{}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		c.mu.Lock()
		if c.currentTask != nil {
			active[fmt.Sprintf("%s#%d", c.currentTask.Repo, c.currentTask.Number)] = true
		}
		c.mu.Unlock()
	}
	return active
}

// handleContributeEvents is the READ-ONLY Server-Sent-Events endpoint
// (GET /api/contribute/events). It streams contributor activity events to any
// connected dashboard browser. It lives under /api/contribute*, so isPublicPath
// makes it PUBLIC — anonymous viewers may subscribe (read-only info only). It never
// mutates anything and never carries credentials.
//
// On connect it sends a "hello" frame (recent-activity replay + the ready-work
// queue snapshot) so a fresh page is not blank, then forwards each subsequent
// appended ActivityEntry as an "activity" frame. Subscriber cleanup is guaranteed:
// the deferred unsubscribe runs on ANY exit (client disconnect via r.Context, or
// server flush error), closing the channel and removing the registry entry — no
// goroutine or subscriber leak.
func (s *Server) handleContributeEvents(w http.ResponseWriter, r *http.Request) {
	if s.contributeHub == nil || s.contributeHub.sse == nil {
		http.Error(w, "event stream unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The dashboard is same-origin; no CORS needed. Prevent proxies from buffering
	// the stream (nginx honours this) so events arrive live.
	w.Header().Set("X-Accel-Buffering", "no")

	sub := s.contributeHub.sse.subscribe()
	defer s.contributeHub.sse.unsubscribe(sub)

	// Initial hydration: recent activity (bounded) + the ready-work queue snapshot.
	replay := s.contributeHub.RecentActivity()
	if len(replay) > sseReplayCap {
		replay = replay[len(replay)-sseReplayCap:]
	}
	hello := sseEvent{
		Type:   "hello",
		Replay: replay,
		Queue:  s.contributeHub.ReadyQueue(readyQueueDefaultLimit),
	}
	if !writeSSE(w, hello) {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	// A heartbeat comment keeps intermediaries from timing out an idle stream and
	// lets a broken pipe surface promptly as a flush error.
	ticker := time.NewTicker(sseHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-sub.events:
			if !open {
				return // registry closed our channel (server shutdown / forced drop)
			}
			if !writeSSE(w, ev) {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// sseHeartbeatInterval is how often an idle stream emits an SSE comment ping. It is
// short enough to keep proxies/load-balancers from closing an idle connection and
// to detect a dead client promptly, without meaningful overhead.
const sseHeartbeatInterval = 25 * time.Second

// writeSSE marshals one event and writes it as a single SSE "data:" frame. Returns
// false on a write/marshal error so the caller can tear down the subscriber.
func writeSSE(w http.ResponseWriter, ev sseEvent) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	return true
}

// sortReadyQueue is a small helper kept for deterministic test assertions: it
// orders items by repo then number. The live ReadyQueue preserves scan order (to
// mirror selectTask); tests that only care about set membership can sort a copy.
func sortReadyQueue(items []ReadyQueueItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Repo != items[j].Repo {
			return items[i].Repo < items[j].Repo
		}
		return items[i].Number < items[j].Number
	})
}

// ── Label-affinity: contributor-declared label interests (#2637) ───────────────
//
// A contributor opts in to a set of label names they can help with (e.g. an
// NVIDIA-machine owner subscribes to "nvidia"). The Operations ready-work queue
// then, FOR THAT VIEWER ONLY, tags matching issues and floats them to the front.
// This is a SOFT signal — a personalised VIEW over the same admissible set — never
// a filter: nothing is removed, so a contributor with no interests, or an issue
// with no labels, is never starved. The shared/anonymous queue is untouched.
//
// Matching rule (chosen for predictability): an issue matches when at least one of
// its GitHub labels equals — case-insensitively, after trimming surrounding
// whitespace — a label the viewer declared. Exact NAME match, not substring, so a
// "gpu" interest does not silently sweep in "gpu-docs" and the contributor gets
// exactly the labels they asked for.

// normalizeLabelInterest lower-cases and trims one label string so interest
// matching is case-insensitive and whitespace-insensitive. Empty after trimming
// means "not a real interest" and callers drop it.
func normalizeLabelInterest(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// labelInterestSet builds a lookup set of normalised interest labels from a
// contributor's declared list, dropping blanks and de-duplicating. A nil/empty
// input yields an empty (non-nil) set, so callers can range/lookup without a nil
// guard and "no interests" naturally matches nothing (→ no reordering).
func labelInterestSet(interests []string) map[string]struct{} {
	set := make(map[string]struct{}, len(interests))
	for _, in := range interests {
		if n := normalizeLabelInterest(in); n != "" {
			set[n] = struct{}{}
		}
	}
	return set
}

// issueMatchesInterests reports whether any of an issue's labels exactly matches
// (case-insensitively) a label in the interest set. An empty interest set or a
// label-less issue returns false — the anti-starvation contract: a non-match is
// never excluded, it simply is not promoted.
func issueMatchesInterests(labels []string, interests map[string]struct{}) bool {
	if len(interests) == 0 {
		return false
	}
	for _, l := range labels {
		if _, ok := interests[normalizeLabelInterest(l)]; ok {
			return true
		}
	}
	return false
}

// personalizeQueueByInterests annotates each item with MatchesInterest for the
// given viewer and STABLY floats matching items to the front, preserving the
// established (operator-pinned + scan) relative order WITHIN the matching group and
// WITHIN the non-matching group. It mutates items in place and returns the same
// slice for convenience.
//
// Soft-signal guarantees:
//   - Empty interests → no item matches → order is unchanged (stable sort with a
//     constant key is a no-op), so a contributor who set nothing sees the exact
//     shared queue: NO STARVATION.
//   - A label-less issue never matches, but is never removed — it keeps its place
//     behind any matches and remains fully eligible.
//   - Only OFFER PRIORITY in THIS VIEWER'S view changes; the persisted operator
//     order and every other viewer's queue are untouched (this runs per-request on
//     a copy the handler owns). A matching item may rise above an operator-pinned
//     non-match for this viewer — that is the intended personalisation and is
//     view-only, never written back.
func personalizeQueueByInterests(items []ReadyQueueItem, interests []string) []ReadyQueueItem {
	set := labelInterestSet(interests)
	for i := range items {
		items[i].MatchesInterest = issueMatchesInterests(items[i].Labels, set)
	}
	if len(set) == 0 {
		return items // fast path: nothing to promote, order untouched
	}
	// Stable partition: matches first, everything else after, each group keeping its
	// prior relative order. SliceStable with a boolean "!match" key does exactly this.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].MatchesInterest && !items[j].MatchesInterest
	})
	return items
}

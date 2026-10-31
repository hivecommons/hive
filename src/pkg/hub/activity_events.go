package hub

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hubactivity"
)

// Fleet-wide activity event ring (RFC hivecommons/hive#15861 §A/§B, child
// #15964). Spokes attach the stage hops, opens and merges they saw since
// the hub last acknowledged to their activity block; the hub keeps the most
// recent activityEventRingSize of them across the fleet IN MEMORY ONLY and
// serves them on GET /api/hub/activity, so the hub board can animate
// movement and show merge toasts. Events are copied out per hive only for
// hives the viewer is authorized for, exactly like digests, and they are
// part of the response body, so its ETag changes when new events arrive.

// activityEventRingSize bounds the fleet-wide ring. When it is full, the
// oldest event of the hive holding the most events is dropped first, so a
// single hive cannot evict the other hives' events.
const activityEventRingSize = 1000

// activityEvent is one stored event tagged with its hive.
type activityEvent struct {
	hiveID     string
	event      hubactivity.Event
	receivedAt time.Time
}

// activityEventHive is the per-hive acknowledgement state.
type activityEventHive struct {
	// lastSeq is the highest event sequence number accepted (the ack).
	lastSeq uint64
	// dropped sums the drops the spoke disclosed and the hub's own cap
	// applied to its batches.
	dropped int
	// disclosed is the drop count the latest counted batch claimed (the
	// spoke's disclosure plus the hub's cap). The spoke's disclosure is
	// cumulative until an ack covers its latest build, so a batch resent
	// after a lost ack restates the drops already counted; only the
	// increase over disclosed is added then.
	disclosed int
	lastSeen  time.Time
}

// activityEventRing is guarded by activityStore.mu. The zero value is
// ready to use.
type activityEventRing struct {
	entries []activityEvent
	byHive  map[string]*activityEventHive
}

// ingest takes a spoke's events. Only events newer than the hive's
// acknowledged sequence number are kept, so a batch resent because an
// acknowledgement was lost is not stored (or its drops counted) twice.
func (r *activityEventRing) ingest(hiveID string, events []hubactivity.Event, dropped int, now time.Time) {
	clean, capped := hubactivity.SanitizeEvents(events)
	h := r.byHive[hiveID]
	if h == nil {
		if len(clean) == 0 {
			return
		}
		if r.byHive == nil {
			r.byHive = map[string]*activityEventHive{}
		}
		h = &activityEventHive{}
		r.byHive[hiveID] = h
	}
	h.lastSeen = now
	fresh, restated := 0, false
	for _, e := range clean {
		if e.Seq <= h.lastSeq {
			// The spoke trims acknowledged events, so holding one proves
			// it never saw the ack and its drop disclosure is cumulative
			// over the one already counted. Known limit: if the spoke
			// buffered more than hubactivity.MaxEvents new events before
			// the resend, the earlier events were trimmed for room and
			// nothing here repeats a Seq, so the restatement goes
			// undetected and the previous batch's drops are counted once
			// more. The over-count is bounded by one batch's disclosure
			// and affects only the drop counter, never stored events.
			restated = true
			continue
		}
		h.lastSeq = e.Seq
		r.entries = append(r.entries, activityEvent{hiveID: hiveID, event: e, receivedAt: now})
		fresh++
	}
	if fresh == 0 {
		return
	}
	claimed := hubactivity.ClampCount(hubactivity.ClampCount(dropped) + capped)
	add := claimed
	if restated {
		add = max(claimed-h.disclosed, 0)
	}
	h.disclosed = claimed
	h.dropped = hubactivity.ClampCount(h.dropped + add)
	r.evictOver()
}

// evictOver trims the ring to activityEventRingSize. Each eviction removes
// the oldest event of the hive holding the most events (the hive holding
// the oldest event among ties), so one spoke flooding the ring can only
// evict its own events once it is the largest holder, never the events of
// hives with a smaller share. Evictions count as drops for the victim hive.
func (r *activityEventRing) evictOver() {
	over := len(r.entries) - activityEventRingSize
	if over <= 0 {
		return
	}
	held := make(map[string]int, len(r.byHive))
	for _, e := range r.entries {
		held[e.hiveID]++
	}
	evict := make(map[int]bool, over)
	for ; over > 0; over-- {
		most := 0
		for _, n := range held {
			most = max(most, n)
		}
		for i, e := range r.entries {
			if held[e.hiveID] == most && !evict[i] {
				evict[i] = true
				held[e.hiveID]--
				if h := r.byHive[e.hiveID]; h != nil {
					h.dropped = hubactivity.ClampCount(h.dropped + 1)
				}
				break
			}
		}
	}
	i := -1
	r.entries = slices.DeleteFunc(r.entries, func(activityEvent) bool {
		i++
		return evict[i]
	})
}

// ack is the highest sequence number held for the hive (0 = none).
func (r *activityEventRing) ack(hiveID string) uint64 {
	if h := r.byHive[hiveID]; h != nil {
		return h.lastSeq
	}
	return 0
}

// forget drops a hive's events and acknowledgement state.
func (r *activityEventRing) forget(hiveID string) {
	if _, ok := r.byHive[hiveID]; !ok {
		return
	}
	delete(r.byHive, hiveID)
	r.entries = slices.DeleteFunc(r.entries, func(e activityEvent) bool { return e.hiveID == hiveID })
}

// forgetOld forgets hives that sent nothing for maxAge.
func (r *activityEventRing) forgetOld(now time.Time, maxAge time.Duration) {
	for id, h := range r.byHive {
		if now.Sub(h.lastSeen) > maxAge {
			r.forget(id)
		}
	}
}

// activityEventsSnapshot is the events and drop counts of a set of hives.
type activityEventsSnapshot struct {
	events  []activityEvent
	dropped map[string]int
}

// eventsAck is the highest event sequence number the hub holds for hiveID,
// returned to the spoke so it can trim its buffer.
func (st *activityStore) eventsAck(hiveID string) uint64 {
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.events.ack(hiveID)
}

// eventsSnapshot copies the events of the given hives only, oldest first,
// so another hive's events are never copied out of the store.
func (st *activityStore) eventsSnapshot(hiveIDs []string) activityEventsSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	want := make(map[string]bool, len(hiveIDs))
	for _, id := range hiveIDs {
		want[id] = true
	}
	out := activityEventsSnapshot{dropped: map[string]int{}}
	for _, e := range st.events.entries {
		if want[e.hiveID] {
			out.events = append(out.events, e)
		}
	}
	for id := range want {
		if h := st.events.byHive[id]; h != nil && h.dropped > 0 {
			out.dropped[id] = h.dropped
		}
	}
	return out
}

// ActivityViewEvent is one event tagged with its hive.
type ActivityViewEvent struct {
	HiveID     string    `json:"hive_id"`
	ReceivedAt time.Time `json:"received_at"`
	hubactivity.Event
}

// matchesEvent applies the view filter to an event. Filters the event does
// not carry (modifier, author kind) are checked against the item's current
// row in the hive's digest; an event whose item is not there is left out.
func (f activityFilter) matchesEvent(hiveID string, e hubactivity.Event, current map[string]hubactivity.Item) bool {
	it := hubactivity.Item{Repo: e.Repo, Number: e.Number, Kind: e.Kind, Title: e.Title, Stage: e.Stage}
	if f.Mod != "" || f.AuthorKind != "" {
		cur, ok := current[activityItemKey(e.Repo, e.Number)]
		if !ok {
			return false
		}
		it.Mods, it.AuthorKind = cur.Mods, cur.AuthorKind
	}
	return f.matches(hiveID, it)
}

func activityItemKey(repo string, n int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(n)
}

// addActivityEvents adds the filtered events of the visible hives to view.
// Hives must already be authorized for the viewer, and the snapshot taken
// for them only.
func addActivityEvents(view *ActivityView, hives []activityHive, blocks map[string]storedActivity, snap activityEventsSnapshot, f activityFilter) {
	visible := make(map[string]bool, len(hives))
	for _, h := range hives {
		if !h.Missing {
			visible[h.ID] = true
		}
	}
	current := map[string]map[string]hubactivity.Item{}
	if f.Mod != "" || f.AuthorKind != "" {
		for id, b := range blocks {
			m := make(map[string]hubactivity.Item, len(b.digest.Items))
			for _, it := range b.digest.Items {
				m[activityItemKey(it.Repo, it.Number)] = it
			}
			current[id] = m
		}
	}
	for _, e := range snap.events {
		if !visible[e.hiveID] || !f.matchesEvent(e.hiveID, e.event, current[e.hiveID]) {
			continue
		}
		view.Events = append(view.Events, ActivityViewEvent{HiveID: e.hiveID, ReceivedAt: e.receivedAt.UTC(), Event: e.event})
	}
	for i := range view.Hives {
		view.Hives[i].EventsDropped = snap.dropped[view.Hives[i].HiveID]
	}
}

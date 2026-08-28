package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-hive activity timeline.
//
// Debugging "why is hive X stuck?" used to mean shelling into a cluster and
// reading `kubectl logs`. A hive restart-looped for hours because it was
// pinned to an immutable image tag; another sat "upgrading" forever because
// the hub could not reach its cluster; a third silently rejected a user.
// Every one of those had a signal the hub ALREADY receives on the heartbeat —
// it was just never recorded.
//
// This is an append-only, bounded, per-hive event store. It records state
// TRANSITIONS, not heartbeats: at ~2 min per beat and 42 hives, writing an
// event per beat would be 30k events/day of pure noise. An event is emitted
// only when a watched field actually changes.

// timelineMaxEvents caps the number of events retained per hive. Oldest are
// pruned first. This store lives on the hub PVC, so it must not grow without
// limit: 200 events is deep enough to cover several weeks of a normally quiet
// hive (these are transitions, not heartbeats), and a fully-saturated hive
// file measures ~37 KB — so the whole 42-hive fleet is bounded at ~1.5 MB
// even in the worst case where every hive is maxed out.
const timelineMaxEvents = 200

// timelineMaxDetailRunes bounds a single event's human-readable detail
// string. Details are built from spoke-reported (i.e. untrusted) values such
// as SHAs, branch names and health check names, so they are both length- and
// content-bounded before they are ever persisted.
const timelineMaxDetailRunes = 300

// timelineAgentCountDelta is the minimum change in running agent count that
// is considered material enough to record. Agent counts oscillate by one as
// tasks start and finish; recording every ±1 would drown the timeline. A
// jump of this size or more indicates a real capacity change (scale-up, mass
// crash, governor mode flip).
const timelineAgentCountDelta = 3

// timelineRestartSlack absorbs clock skew and RFC3339 second-truncation when
// comparing a spoke's reported StartedAt across heartbeats. A StartedAt that
// moves forward by more than this means the process actually restarted.
const timelineRestartSlack = 30 * time.Second

// timelineUpgradeStaleAfter is how long a hive may sit with Upgrading=true
// before the timeline records that the upgrade went stale. It matches the
// hub's own staleUpgradeTimeout so the timeline and the recovery logic agree
// on what "stuck" means.
const timelineUpgradeStaleAfter = staleUpgradeTimeout

// Event kinds. Stable strings — they are persisted and rendered in the UI.
const (
	TimelineVersionChanged   = "version_changed"
	TimelineBranchChanged    = "branch_changed"
	TimelineWentOffline      = "went_offline"
	TimelineCameOnline       = "came_online"
	TimelineHealthChanged    = "health_changed"
	TimelineUpgradeStarted   = "upgrade_started"
	TimelineUpgradeCompleted = "upgrade_completed"
	TimelineUpgradeStale     = "upgrade_stale"
	TimelineRestarted        = "restarted"
	TimelineACMMChanged      = "acmm_changed"
	TimelineAgentsChanged    = "agents_changed"
	TimelineGitHubAppChanged = "github_app_changed"
	TimelineAccess           = "access"
	TimelineOwnership        = "ownership"
	TimelineAdmin            = "admin"
	TimelineRenamed          = "renamed"
)

// timelineDir is the directory holding one JSON file per hive. It follows the
// existing /data/saas conventions (PVC-backed, atomic tmp+rename writes).
// var, not const, so tests can redirect it.
var timelineDir = "/data/saas/timeline"

// TimelineEvent is one recorded state transition.
type TimelineEvent struct {
	TS     string `json:"ts"`              // RFC3339 UTC
	Kind   string `json:"kind"`            // one of the Timeline* constants
	Detail string `json:"detail"`          // human-readable, pre-sanitized
	Actor  string `json:"actor,omitempty"` // GitHub username for human-initiated events
	// ActorName is the resolved display label for an opaque OIDC Actor
	// identity, stamped on the SERVED copies only (never persisted — the
	// store keeps raw identities so history survives display-name changes).
	// Empty when Actor is already the best label.
	ActorName string `json:"actorName,omitempty"`
}

// hiveTimeline is the on-disk shape: an append-only, capped event list.
type hiveTimeline struct {
	HiveID string          `json:"hiveId"`
	Events []TimelineEvent `json:"events"`
}

// timelineStore holds every hive's timeline in memory and mirrors it to disk.
//
// It owns its OWN mutex, and that mutex is a LEAF: nothing acquired under it
// ever reaches back for HubServer.mu. Callers likewise append only after
// releasing s.mu — re-locking a non-reentrant mutex from the heartbeat path
// has deadlocked hub startup before, and persist() does file I/O that would
// otherwise serialize every heartbeat in the fleet behind one disk write.
type timelineStore struct {
	mu   sync.RWMutex
	byID map[string][]TimelineEvent
}

func newTimelineStore() *timelineStore {
	return &timelineStore{byID: make(map[string][]TimelineEvent)}
}

// timelinePath returns the on-disk path for a hive, or "" if the hive ID is
// not a safe filename. Hive IDs arrive over the heartbeat, so a traversal
// attempt must never reach the filesystem.
func timelinePath(hiveID string) string {
	if hiveID == "" || !isValidName(hiveID) ||
		strings.Contains(hiveID, "..") || strings.ContainsAny(hiveID, `/\`) {
		return ""
	}
	return filepath.Join(timelineDir, hiveID+".json")
}

// truncateDetail bounds a detail string so a hostile spoke cannot inflate the
// store. Values are already sanitized upstream by sanitizeHeartbeatField;
// this is the belt to that suspenders.
func truncateDetail(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > timelineMaxDetailRunes {
		return string(r[:timelineMaxDetailRunes]) + "…"
	}
	return s
}

// append records one event for a hive, prunes to the retention cap
// (oldest-first) and persists. Safe to call from many goroutines.
func (t *timelineStore) append(hiveID, kind, detail, actor string) {
	if hiveID == "" || kind == "" {
		return
	}
	ev := TimelineEvent{
		TS:     time.Now().UTC().Format(time.RFC3339),
		Kind:   kind,
		Detail: truncateDetail(detail),
		Actor:  truncateDetail(actor),
	}

	t.mu.Lock()
	events := append(t.byID[hiveID], ev)
	if len(events) > timelineMaxEvents {
		// Prune oldest first. Copy into a fresh slice so the pruned prefix is
		// not retained by the backing array.
		trimmed := make([]TimelineEvent, timelineMaxEvents)
		copy(trimmed, events[len(events)-timelineMaxEvents:])
		events = trimmed
	}
	t.byID[hiveID] = events
	snapshot := make([]TimelineEvent, len(events))
	copy(snapshot, events)
	t.mu.Unlock()

	t.persist(hiveID, snapshot)
}

// appendAll records a batch of events under a single lock acquisition. Used
// by the heartbeat hook, which frequently produces several transitions at
// once (e.g. came online + version changed + upgrade completed).
func (t *timelineStore) appendAll(hiveID string, evs []TimelineEvent) {
	if hiveID == "" || len(evs) == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)

	t.mu.Lock()
	events := t.byID[hiveID]
	for _, ev := range evs {
		if ev.Kind == "" {
			continue
		}
		ev.TS = now
		ev.Detail = truncateDetail(ev.Detail)
		ev.Actor = truncateDetail(ev.Actor)
		events = append(events, ev)
	}
	if len(events) > timelineMaxEvents {
		trimmed := make([]TimelineEvent, timelineMaxEvents)
		copy(trimmed, events[len(events)-timelineMaxEvents:])
		events = trimmed
	}
	t.byID[hiveID] = events
	snapshot := make([]TimelineEvent, len(events))
	copy(snapshot, events)
	t.mu.Unlock()

	t.persist(hiveID, snapshot)
}

// recent returns up to n events for a hive, newest first. Never returns nil.
func (t *timelineStore) recent(hiveID string, n int) []TimelineEvent {
	t.mu.RLock()
	events := t.byID[hiveID]
	t.mu.RUnlock()

	if n <= 0 || n > len(events) {
		n = len(events)
	}
	out := make([]TimelineEvent, 0, n)
	for i := len(events) - 1; i >= len(events)-n; i-- {
		out = append(out, events[i])
	}
	return out
}

// persist writes a hive's timeline atomically (tmp+rename), matching the
// other /data/saas state files. Failures are logged by the caller's absence
// of a return value only — a timeline write must never fail a heartbeat.
func (t *timelineStore) persist(hiveID string, events []TimelineEvent) {
	path := timelinePath(hiveID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(timelineDir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(hiveTimeline{HiveID: hiveID, Events: events}, "", "  ")
	if err != nil {
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
	}
}

// load reads every persisted timeline back into memory at hub startup, so a
// hub restart does not blank the history users are trying to debug with.
func (t *timelineStore) load() {
	entries, err := os.ReadDir(timelineDir)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(timelineDir, name))
		if err != nil {
			continue
		}
		var tl hiveTimeline
		if json.Unmarshal(data, &tl) != nil || tl.HiveID == "" {
			continue
		}
		events := tl.Events
		if len(events) > timelineMaxEvents {
			events = events[len(events)-timelineMaxEvents:]
		}
		t.byID[tl.HiveID] = events
	}
}

// ---------------------------------------------------------------------------
// Transition detection
// ---------------------------------------------------------------------------

// healthStatus extracts the coarse status string from a heartbeat health map.
// The spoke reports health as a free-form map; "status" is the field the
// dashboard already keys on.
func healthStatus(h map[string]any) string {
	if h == nil {
		return ""
	}
	if v, ok := h["status"].(string); ok {
		return v
	}
	return ""
}

// flippedHealthChecks names the individual checks whose boolean/string value
// differs between two health maps. Returned sorted so the detail string is
// deterministic (map iteration order is not).
func flippedHealthChecks(oldH, newH map[string]any) []string {
	if oldH == nil || newH == nil {
		return nil
	}
	var flipped []string
	for k, nv := range newH {
		if k == "status" {
			continue
		}
		ov, present := oldH[k]
		if !present {
			continue
		}
		// Only compare scalars; nested structures are noisy and change shape.
		switch nv.(type) {
		case bool, string, float64:
			if fmt.Sprintf("%v", ov) != fmt.Sprintf("%v", nv) {
				flipped = append(flipped, k)
			}
		}
	}
	sort.Strings(flipped)
	return flipped
}

// absInt returns the absolute value of an int.
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// detectTransitions compares the previously-registered entry against the one
// just built from a heartbeat and returns the events worth recording.
//
// It is a PURE function: no locks, no I/O, no clock beyond `now`. That is
// what makes it callable from inside the heartbeat's existing s.mu critical
// section without any risk of lock re-entrancy, and what makes it directly
// table-testable.
func detectTransitions(old, new *RegistryEntry, now time.Time) []TimelineEvent {
	if old == nil || new == nil {
		return nil
	}
	var evs []TimelineEvent
	add := func(kind, detail string) {
		evs = append(evs, TimelineEvent{Kind: kind, Detail: detail})
	}

	// Offline → online. The registry marks Online=false from markStaleHives;
	// a heartbeat arriving at all means the hive is back.
	if !old.Online && new.Online {
		add(TimelineCameOnline, "hive resumed reporting to the hub")
	}

	// Restart: the spoke's own reported process start time moved forward.
	// This is the tell for a crash-looping hive that otherwise reads healthy.
	if old.StartedAt != "" && new.StartedAt != "" && old.StartedAt != new.StartedAt {
		oldT, errOld := time.Parse(time.RFC3339, old.StartedAt)
		newT, errNew := time.Parse(time.RFC3339, new.StartedAt)
		if errOld == nil && errNew == nil && newT.Sub(oldT) > timelineRestartSlack {
			add(TimelineRestarted, fmt.Sprintf("process restarted (up %s, previously started %s)",
				shortDur(now.Sub(newT)), old.StartedAt))
		}
	}

	// Version / SHA.
	if old.GitHash != "" && new.GitHash != "" && !sameCommit(old.GitHash, new.GitHash) {
		detail := fmt.Sprintf("version %s → %s", old.GitHash, new.GitHash)
		if old.Version != new.Version && new.Version != "" {
			detail += fmt.Sprintf(" (%s → %s)", old.Version, new.Version)
		}
		add(TimelineVersionChanged, detail)
	}

	// Branch.
	if old.GitBranch != "" && new.GitBranch != "" && old.GitBranch != new.GitBranch {
		add(TimelineBranchChanged, fmt.Sprintf("branch %s → %s", old.GitBranch, new.GitBranch))
	}

	// Upgrade lifecycle. new.Upgrading is already reconciled by the heartbeat
	// handler against the target SHA before this runs.
	switch {
	case !old.Upgrading && new.Upgrading:
		target := new.UpgradeTarget
		if target == "" {
			target = "latest"
		}
		add(TimelineUpgradeStarted, fmt.Sprintf("upgrade started → %s", target))
	case old.Upgrading && !new.Upgrading:
		if new.GitHash != "" && !sameCommit(old.GitHash, new.GitHash) {
			add(TimelineUpgradeCompleted, fmt.Sprintf("upgrade completed at %s", new.GitHash))
		} else {
			add(TimelineUpgradeCompleted, "upgrade cleared")
		}
	case old.Upgrading && new.Upgrading && !old.UpgradeStartedAt.IsZero():
		// Still upgrading. Record staleness EXACTLY once, at the crossing:
		// the previous heartbeat was inside the window and this one is not.
		elapsed := now.Sub(old.UpgradeStartedAt)
		if elapsed > timelineUpgradeStaleAfter {
			prev := now.Sub(old.UpgradeStartedAt) - heartbeatStalePollWindow
			if prev <= timelineUpgradeStaleAfter {
				target := new.UpgradeTarget
				if target == "" {
					target = "latest"
				}
				add(TimelineUpgradeStale, fmt.Sprintf(
					"upgrade to %s has been in flight for %s with no version change — the hive may be unreachable or pinned to an immutable tag",
					target, shortDur(elapsed)))
			}
		}
	}

	// Health status, plus which individual check flipped.
	oldStatus, newStatus := healthStatus(old.Health), healthStatus(new.Health)
	if oldStatus != newStatus && (oldStatus != "" || newStatus != "") {
		detail := fmt.Sprintf("health %s → %s", orDash(oldStatus), orDash(newStatus))
		if flipped := flippedHealthChecks(old.Health, new.Health); len(flipped) > 0 {
			detail += " (changed: " + strings.Join(flipped, ", ") + ")"
		}
		add(TimelineHealthChanged, detail)
	}

	// ACMM level.
	if old.ACMMLevel != new.ACMMLevel {
		add(TimelineACMMChanged, fmt.Sprintf("ACMM level %d → %d", old.ACMMLevel, new.ACMMLevel))
	}

	// Agent count — only material moves.
	if absInt(new.AgentCount-old.AgentCount) >= timelineAgentCountDelta {
		add(TimelineAgentsChanged, fmt.Sprintf("running agents %d → %d", old.AgentCount, new.AgentCount))
	}

	// GitHub App: permission issue appeared or cleared, install completed.
	if old.GitHubAppPermIssue != new.GitHubAppPermIssue {
		if new.GitHubAppPermIssue == "" {
			add(TimelineGitHubAppChanged, "GitHub App permission issue cleared")
		} else {
			add(TimelineGitHubAppChanged, "GitHub App permission issue: "+new.GitHubAppPermIssue)
		}
	}
	if old.PendingGitHubAppInstall && !new.PendingGitHubAppInstall {
		add(TimelineGitHubAppChanged, "GitHub App install completed")
	} else if !old.PendingGitHubAppInstall && new.PendingGitHubAppInstall {
		add(TimelineGitHubAppChanged, "GitHub App install pending — the App is not yet installed on the org")
	}
	if old.GitHubAppRequired != new.GitHubAppRequired {
		if new.GitHubAppRequired {
			add(TimelineGitHubAppChanged, "hive now requires a GitHub App install")
		} else {
			add(TimelineGitHubAppChanged, "GitHub App requirement satisfied")
		}
	}

	return evs
}

// heartbeatStalePollWindow is the assumed spacing between two consecutive
// heartbeats from the same hive. It is used only to fire the "upgrade went
// stale" event once, on the beat that first crosses the threshold, instead of
// on every beat thereafter. Spokes beat every ~2 min; maxHeartbeatAge is the
// hub's own tolerance for that spacing and is reused here so the two never
// disagree.
const heartbeatStalePollWindow = maxHeartbeatAge

// orDash renders an empty string as an em dash for readability.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// shortDur renders a duration as a compact human string ("3m", "2h", "4d").
func shortDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	const hoursPerDay = 24
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < hoursPerDay*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/hoursPerDay)
	}
}

// ---------------------------------------------------------------------------
// HubServer hooks
// ---------------------------------------------------------------------------

// recordTimeline appends a single human-initiated event. Call it alongside
// the existing `audit:` log lines — the log answers "what did the hub do",
// the timeline answers "what happened to this hive".
//
// Never call this while holding s.mu: it performs a file write. The timeline
// store has its own leaf lock, but the I/O would serialize whatever s.mu is
// protecting.
func (s *HubServer) recordTimeline(hiveID, kind, detail, actor string) {
	if s == nil || s.timeline == nil {
		return
	}
	s.timeline.append(hiveID, kind, detail, actor)
}

// recordHeartbeatTransitions is the heartbeat hook. `old` is the registry
// entry as it stood before this beat, `new` is the entry that replaced it.
//
// Call it AFTER s.mu.Unlock(): detectTransitions is pure, but the append that
// follows writes a file, and s.mu is a write lock held across the entire
// registry scan — doing I/O under it would serialize every heartbeat in the
// fleet behind one disk write.
func (s *HubServer) recordHeartbeatTransitions(old, new *RegistryEntry) {
	if s == nil || s.timeline == nil {
		return
	}
	evs := detectTransitions(old, new, time.Now())
	if len(evs) == 0 {
		return
	}
	s.timeline.appendAll(new.ID, evs)
}

// recordOfflineTransitions is the sweep hook. markStaleHives flips Online to
// false for hives that stopped beating; that transition can only be observed
// there, because a hive that has gone silent by definition sends no
// heartbeat to observe it on.
//
// markStaleHives runs with s.mu held, so this only COLLECTS the events; the
// caller drains and appends them after unlocking. See offlineSweepEvent.
type offlineSweepEvent struct {
	HiveID string
	Detail string
}

// offlineEventFor builds the went-offline event for a hive whose heartbeat
// just aged past maxHeartbeatAge. Pure — safe to call under s.mu.
func offlineEventFor(h *RegistryEntry, age time.Duration) offlineSweepEvent {
	return offlineSweepEvent{
		HiveID: h.ID,
		Detail: fmt.Sprintf("no heartbeat for %s — last seen %s", shortDur(age), orDash(h.LastHeartbeat)),
	}
}

// flushOfflineEvents appends collected went-offline events. Call AFTER
// releasing s.mu.
func (s *HubServer) flushOfflineEvents(evs []offlineSweepEvent) {
	if s == nil || s.timeline == nil {
		return
	}
	for _, e := range evs {
		s.timeline.append(e.HiveID, TimelineWentOffline, e.Detail, "")
	}
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

// handleHiveTimeline serves GET /api/saas/hives/{id}/timeline.
//
// Authorization is deliberately IDENTICAL to the existing per-hive endpoints
// (handleAccessList et al): the hive's owner, or the hub admin. No new
// authorization rule is introduced here.
func (s *HubServer) handleHiveTimeline(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !canonicalEqual(h.Owner, username) && !isHubAdmin(username) {
		http.Error(w, `{"error":"only the owner can view this hive's timeline"}`, http.StatusForbidden)
		return
	}
	events := s.timeline.recent(hiveID, timelineMaxEvents)
	s.decorateTimelineActors(events)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"events": events})
}

// handleAccessLog serves GET /api/saas/hives/{id}/access-log — the
// permission-change audit trail shown in the Manage Access dialog (#4148).
//
// It is a filtered VIEW over the same append-only timeline store that
// records every grant, role change, removal, request approval/denial and
// ownership transfer: no separate log is kept, so the two can never
// disagree. Entries carry actor + timestamp and are immutable once written
// (the store only ever appends; pruning is oldest-first at the retention
// cap).
//
// Authorization matches the Manage Access endpoints themselves
// (handleAccessList et al): the hive's owner — creator, granted owner role,
// or hub admin — may read it. This is deliberately WIDER than
// handleHiveTimeline's creator-only rule, because anyone who can change
// permissions must be able to audit permission changes.
func (s *HubServer) handleAccessLog(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can view the access audit log"}`, http.StatusForbidden)
		return
	}
	all := s.timeline.recent(hiveID, timelineMaxEvents)
	events := make([]TimelineEvent, 0, len(all))
	for _, ev := range all {
		// Ownership transfers change who holds the top permission, so they
		// belong in a permission-change audit alongside access events.
		if ev.Kind == TimelineAccess || ev.Kind == TimelineOwnership {
			events = append(events, ev)
		}
	}
	s.decorateTimelineActors(events)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"events": events})
}

// decorateTimelineActors stamps ActorName on served event copies whose Actor
// is an opaque OIDC identity with a stored display name. Serve-time only —
// the underlying store is never mutated.
func (s *HubServer) decorateTimelineActors(events []TimelineEvent) {
	label := s.identityLabeler()
	for i := range events {
		if l := label(events[i].Actor); l != events[i].Actor {
			events[i].ActorName = l
		}
	}
}

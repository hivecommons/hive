package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/hubactivity"
)

// Hub-wide Activity Monitor, first slice (RFC hivecommons/hive#15861,
// child #15951). Spokes attach a bounded hubactivity.Digest to their
// heartbeat; the hub keeps the latest one per hive IN MEMORY ONLY (never in
// the persisted registry) and serves a merged, per-user scoped view on
// GET /api/hub/activity with ETag polling. The hub is visibility-only: the
// endpoint is read-only and offers no actions.

const (
	activityPath = "/api/hub/activity"

	// activityHeartbeatInterval mirrors the spoke's heartbeatSendInterval
	// (pkg/dashboard). A hive whose activity block is older than two
	// intervals is stale.
	activityHeartbeatInterval = 2 * time.Minute
	activityStaleAfter        = 2 * activityHeartbeatInterval

	// activityForgetAfter drops a hive's block from memory once it has not
	// been refreshed for this long, so hives that stopped reporting (or left
	// the registry) cannot pin memory forever.
	activityForgetAfter = 24 * time.Hour

	// maxActivityResponseItems bounds the merged response no matter how many
	// hives a viewer can see; overflow is disclosed in Truncated.
	maxActivityResponseItems = 8 * hubactivity.MaxItemsCeiling

	// maxActivityFilterRunes bounds each query filter value.
	maxActivityFilterRunes = 200
)

// Hive status values in the activity view, most severe first.
const (
	activityStatusMissing      = "missing"       // granted but unknown to the registry
	activityStatusNotReporting = "not_reporting" // heartbeats, but sends no activity block
	activityStatusStale        = "stale"         // block older than two heartbeats
	activityStatusOK           = "ok"
)

// storedActivity is one hive's latest accepted digest.
type storedActivity struct {
	digest     hubactivity.Digest
	receivedAt time.Time
}

// activityStore holds the latest digest per hive. It is a leaf lock: never
// take s.mu while holding it. The zero value is ready to use.
type activityStore struct {
	mu     sync.Mutex
	byHive map[string]storedActivity
}

// record applies a heartbeat's activity block and returns the etag the hub
// now holds for the hive (the acknowledgement), or "" when it holds none.
//
//   - nil block: the spoke is too old or not participating; nothing changes.
//   - unchanged marker with the held etag: the block is refreshed in place.
//   - unchanged marker with any other etag: ignored; the returned (held or
//     empty) etag tells the spoke to send its full digest next beat.
//   - full digest: sanitized against the hub's ceilings and stored.
func (st *activityStore) record(hiveID string, block *hubactivity.Digest, now time.Time) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.forgetOldLocked(now)
	held, ok := st.byHive[hiveID]
	if block == nil {
		if ok {
			return held.digest.ETag
		}
		return ""
	}
	if block.IsUnchangedMarker() {
		// Compare in the stored (cleaned) form, or a marker whose etag
		// changes under cleaning would never match and wedge the handshake.
		if !ok || held.digest.ETag != hubactivity.CleanETag(block.ETag) {
			if ok {
				return held.digest.ETag
			}
			return ""
		}
		held.receivedAt = now
		if !block.GeneratedAt.IsZero() {
			held.digest.GeneratedAt = block.GeneratedAt.UTC()
		}
		st.byHive[hiveID] = held
		return held.digest.ETag
	}
	// The spoke's etag is kept as the handshake identity rather than
	// recomputed: a spoke that lies about it only misleads its own
	// freshness, and recomputing would make every marker miss.
	clean := hubactivity.Sanitize(*block)
	if clean.ETag == "" {
		clean.ETag = hubactivity.ContentETag(clean)
	}
	if st.byHive == nil {
		st.byHive = map[string]storedActivity{}
	}
	st.byHive[hiveID] = storedActivity{digest: clean, receivedAt: now}
	return clean.ETag
}

// forget drops a hive's held digest, e.g. when the hive leaves the registry.
// Unknown ids and a nil store are no-ops.
func (st *activityStore) forget(hiveID string) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.byHive, hiveID)
}

func (st *activityStore) forgetOldLocked(now time.Time) {
	for id, a := range st.byHive {
		if now.Sub(a.receivedAt) > activityForgetAfter {
			delete(st.byHive, id)
		}
	}
}

// snapshot copies the blocks of the given hives only, so data of hives the
// viewer cannot see is never even copied out of the store.
func (st *activityStore) snapshot(hiveIDs []string) map[string]storedActivity {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]storedActivity, len(hiveIDs))
	for _, id := range hiveIDs {
		if a, ok := st.byHive[id]; ok {
			out[id] = a
		}
	}
	return out
}

// activityHive is the registry slice the view needs.
type activityHive struct {
	ID            string
	Name          string
	Owner         string
	LastHeartbeat string
	Missing       bool
}

// visibleActivityHives returns the hives username may see, applying the
// same rule as the My Hives page (buildMyHivesResponse): hub admins see
// every registered hive; everyone else sees hives they own or hold an
// unexpired grant for. A granted hive the registry does not know is
// returned with Missing set so the view can disclose it.
func visibleActivityHives(username string, user *SaaSUser, registry []activityHive, now time.Time) []activityHive {
	if username == "" {
		return nil
	}
	isAdmin := isHubAdmin(username)
	granted := map[string]bool{}
	if user != nil {
		for id, role := range user.Hives {
			if role == "" || accessGrantExpired(user.HiveExpiry[id], now) {
				continue
			}
			granted[id] = true
		}
	}
	var out []activityHive
	seen := map[string]bool{}
	for _, h := range registry {
		if isAdmin || granted[h.ID] || canonicalEqual(h.Owner, username) {
			out = append(out, h)
			seen[h.ID] = true
		}
	}
	for id := range granted {
		if !seen[id] {
			out = append(out, activityHive{ID: id, Missing: true})
		}
	}
	slices.SortFunc(out, func(a, b activityHive) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// activityFilter is the parsed, bounded query string.
type activityFilter struct {
	Hive, Repo, Stage, Mod, AuthorKind, Q string
	// qLower is Q lowercased once; qNumber is Q read as an item number
	// ("123" or "#123"), or 0.
	qLower  string
	qNumber int
}

func parseActivityFilter(q url.Values) (activityFilter, error) {
	var f activityFilter
	fields := []struct {
		key string
		dst *string
	}{
		{"hive", &f.Hive}, {"repo", &f.Repo}, {"stage", &f.Stage},
		{"mod", &f.Mod}, {"author_kind", &f.AuthorKind}, {"q", &f.Q},
	}
	for _, fl := range fields {
		v := strings.TrimSpace(q.Get(fl.key))
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxActivityFilterRunes {
			return activityFilter{}, fmt.Errorf("invalid %s filter", fl.key)
		}
		*fl.dst = v
	}
	f.qLower = strings.ToLower(f.Q)
	if n, err := strconv.Atoi(strings.TrimPrefix(f.Q, "#")); err == nil && n > 0 {
		f.qNumber = n
	}
	return f, nil
}

func (f activityFilter) matches(hiveID string, it hubactivity.Item) bool {
	switch {
	case f.Hive != "" && f.Hive != hiveID:
		return false
	case f.Repo != "" && !strings.EqualFold(f.Repo, it.Repo):
		return false
	case f.Stage != "" && f.Stage != it.Stage:
		return false
	case f.AuthorKind != "" && f.AuthorKind != it.AuthorKind:
		return false
	case f.Mod != "" && !slices.Contains(it.Mods, f.Mod):
		return false
	}
	if f.Q == "" {
		return true
	}
	if f.qNumber > 0 && it.Number == f.qNumber {
		return true
	}
	return strings.Contains(strings.ToLower(it.Title), f.qLower)
}

// ActivityHiveView is one visible hive's freshness and totals.
type ActivityHiveView struct {
	HiveID string `json:"hive_id"`
	Name   string `json:"name,omitempty"`
	// Status is ok, stale, not_reporting ("upgrade to report activity") or
	// missing (granted, but the hub has no registry entry for it).
	Status        string              `json:"status"`
	Reporting     bool                `json:"reporting"`
	Stale         bool                `json:"stale"`
	LastHeartbeat string              `json:"last_heartbeat,omitempty"`
	ReceivedAt    *time.Time          `json:"received_at,omitempty"`
	GeneratedAt   *time.Time          `json:"generated_at,omitempty"`
	Totals        *hubactivity.Totals `json:"totals,omitempty"`
	// Truncated is how many of this hive's items its digest left out.
	Truncated int `json:"truncated,omitempty"`
}

// ActivityViewItem is one merged item tagged with its hive.
type ActivityViewItem struct {
	HiveID string `json:"hive_id"`
	hubactivity.Item
}

// ActivityView is the GET /api/hub/activity body.
type ActivityView struct {
	Hives []ActivityHiveView `json:"hives"`
	Items []ActivityViewItem `json:"items"`
	// Truncated is how many matching items the response cap left out, on
	// top of what each hive's own digest already left out.
	Truncated int `json:"truncated,omitempty"`
}

// buildActivityView merges the blocks of the visible hives only. Hives must
// already be authorized for the viewer.
func buildActivityView(hives []activityHive, blocks map[string]storedActivity, f activityFilter, now time.Time) ActivityView {
	view := ActivityView{Hives: []ActivityHiveView{}, Items: []ActivityViewItem{}}
	for _, h := range hives {
		hv := ActivityHiveView{HiveID: h.ID, Name: h.Name, LastHeartbeat: h.LastHeartbeat}
		block, ok := blocks[h.ID]
		switch {
		case h.Missing:
			hv.Status = activityStatusMissing
		case !ok:
			hv.Status = activityStatusNotReporting
		default:
			hv.Reporting = true
			hv.Stale = now.Sub(block.receivedAt) > activityStaleAfter
			hv.Status = activityStatusOK
			if hv.Stale {
				hv.Status = activityStatusStale
			}
			recv := block.receivedAt.UTC()
			hv.ReceivedAt = &recv
			if gen := block.digest.GeneratedAt; !gen.IsZero() {
				hv.GeneratedAt = &gen
			}
			hv.Totals = block.digest.Totals
			hv.Truncated = block.digest.Truncated
			for _, it := range block.digest.Items {
				if f.matches(h.ID, it) {
					view.Items = append(view.Items, ActivityViewItem{HiveID: h.ID, Item: it})
				}
			}
		}
		if f.Hive == "" || f.Hive == h.ID {
			view.Hives = append(view.Hives, hv)
		}
	}
	slices.SortStableFunc(view.Items, func(a, b ActivityViewItem) int {
		if c := hubactivity.CompareUrgency(a.Item, b.Item); c != 0 {
			return c
		}
		return strings.Compare(a.HiveID, b.HiveID)
	})
	if len(view.Items) > maxActivityResponseItems {
		view.Truncated = len(view.Items) - maxActivityResponseItems
		view.Items = view.Items[:maxActivityResponseItems]
	}
	return view
}

// handleHubActivity serves GET /api/hub/activity. It is mounted behind
// requireAuth; authorization to each hive is decided here BEFORE any block
// is read, so another hive's data never enters the response or its etag.
func (s *HubServer) handleHubActivity(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	f, err := parseActivityFilter(r.URL.Query())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	s.mu.RLock()
	registry := make([]activityHive, 0, len(s.registry.Hives))
	for _, h := range s.registry.Hives {
		name := h.ProjectName
		if name == "" {
			name = h.Name
		}
		registry = append(registry, activityHive{ID: h.ID, Name: name, Owner: h.Owner, LastHeartbeat: h.LastHeartbeat})
	}
	s.mu.RUnlock()

	hives := visibleActivityHives(username, loadSaaSUser(username), registry, now)
	ids := make([]string, len(hives))
	for i, h := range hives {
		ids[i] = h.ID
	}
	view := buildActivityView(hives, s.activity.snapshot(ids), f, now)
	body, err := json.Marshal(view)
	if err != nil {
		s.logger.Error("hub activity: encode view", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	etag := webstatic.ETagFor(body)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	// Per-user data: never stored by a shared cache, always revalidated.
	h.Set("Cache-Control", "private, no-cache")
	h.Set("Vary", "Cookie, Authorization")
	h.Set("ETag", etag)
	if activityETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// activityETagMatches is the RFC 9110 weak comparison of If-None-Match.
func activityETagMatches(header, etag string) bool {
	want := strings.TrimPrefix(etag, "W/")
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == want {
			return true
		}
	}
	return false
}

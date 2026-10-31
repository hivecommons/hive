package hub

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/hubactivity"
)

// Public-safe Fleet activity view (#15966, part of RFC #15861 by @hanthor).
//
// The hivecommonsfleet Twitch broadcast renders this view and nothing else.
// A stream is public, so dashboard authentication cannot protect it and a
// viewer's per-user Fleet visibility must never decide what it shows. This
// view is therefore anonymous by construction: it reads no session, cookie
// or Authorization header, and the same request returns the same rows for
// everyone. It keeps only:
//
//   - hives the registry lists as public (the flag the public registry and
//     leaderboard already honour) that target public github.com;
//   - items and events the spoke marked public. The spoke marks every repo
//     private unless it is known public, so unknown visibility is omitted.
//
// It drops every count that can include private items (totals, digest
// truncation, dropped events) and the hive owner. Filtering happens on the
// copied store data BEFORE the view is built, so a private row never enters
// the response or its ETag.

const (
	activityPublicPagePath = "/activity/public"
	activityPublicPath     = "/api/hub/activity/public"
)

// publicActivityHives returns the registry hives the public-safe view may
// show. It takes no viewer: no per-user grant can widen it.
func publicActivityHives(registry []activityHive) []activityHive {
	var out []activityHive
	for _, h := range registry {
		if h.Public && !h.Missing {
			out = append(out, activityHive{ID: h.ID, Name: h.Name, LastHeartbeat: h.LastHeartbeat, Public: true})
		}
	}
	return out
}

// publicSafeActivityBlocks keeps only public items and drops the counts
// that include private or capped items.
func publicSafeActivityBlocks(blocks map[string]storedActivity) map[string]storedActivity {
	out := make(map[string]storedActivity, len(blocks))
	for id, b := range blocks {
		var items []hubactivity.Item
		for _, it := range b.digest.Items {
			if !it.Private {
				items = append(items, it)
			}
		}
		out[id] = storedActivity{
			digest:     hubactivity.Digest{GeneratedAt: b.digest.GeneratedAt, Items: items},
			receivedAt: b.receivedAt,
		}
	}
	return out
}

// publicSafeActivityEvents keeps only public events; per-hive dropped
// counts are omitted because they may count private events.
func publicSafeActivityEvents(snap activityEventsSnapshot) activityEventsSnapshot {
	out := activityEventsSnapshot{dropped: map[string]int{}}
	for _, e := range snap.events {
		if !e.event.Private {
			out.events = append(out.events, e)
		}
	}
	return out
}

// buildPublicActivityView is the whole public-safe view: registry-public
// hives, public rows only, no viewer input beyond the query filters.
func (s *HubServer) buildPublicActivityView(f activityFilter, now time.Time) ActivityView {
	hives := publicActivityHives(s.activityRegistry())
	ids := make([]string, len(hives))
	for i, h := range hives {
		ids[i] = h.ID
	}
	blocks := publicSafeActivityBlocks(s.activity.snapshot(ids))
	view := buildActivityView(hives, blocks, f, now)
	addActivityEvents(&view, hives, blocks, publicSafeActivityEvents(s.activity.eventsSnapshot(ids)), f)
	return view
}

// handleHubActivityPublic serves GET /api/hub/activity/public. It is not
// behind requireAuth and never reads the request's credentials, so a token
// or session presented with it changes nothing.
func (s *HubServer) handleHubActivityPublic(w http.ResponseWriter, r *http.Request) {
	f, err := parseActivityFilter(r.URL.Query())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := json.Marshal(s.buildPublicActivityView(f, time.Now().UTC()))
	if err != nil {
		s.logger.Error("hub public activity: encode view", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	etag := webstatic.ETagFor(body)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if activityETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// handleActivityPublicPage serves the Fleet activity shell anonymously. The
// shell carries no data; on this path activity.js polls activityPublicPath.
func (s *HubServer) handleActivityPublicPage(w http.ResponseWriter, r *http.Request) {
	s.serveActivityShell(w, r)
}

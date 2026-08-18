package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// Focus-aware presence. A live session (a valid hive_session cookie / an open
// tab) says nothing about whether a human is actually THERE — an idle tab left
// open over a weekend used to accumulate "time in hive" the whole time. The
// frontend therefore reports classic idle-detection presence: it pings this
// endpoint only while the tab is VISIBLE and input (mouse/key/scroll/touch)
// occurred within the idle window. The absence of pings is what marks a user
// idle — there is no explicit "idle" message to spoof or to miss.
//
// The engaged set feeds the heartbeat (EngagedSessionUsers) so the hub can
// accumulate honest per-user engaged time next to the back-compat open-tab
// session time.
const (
	// presencePingIntervalSecs is the cadence at which an ENGAGED frontend
	// pings /api/presence. It must match PRESENCE_PING_MS in static/index.html.
	presencePingIntervalSecs = 30
	// presenceFreshness is how long after the last engaged ping a user still
	// counts as engaged. Three ping intervals absorbs up to two dropped/late
	// pings before the user is (correctly) demoted to idle.
	presenceFreshness = 3 * presencePingIntervalSecs * time.Second
	// maxPresenceBodyBytes caps the beacon's request body. The real payload is
	// a dozen bytes of JSON; anything bigger is not a presence ping.
	maxPresenceBodyBytes = 1024
	// maxPresenceUsers bounds the engaged map so a stream of unique usernames
	// (misbehaving proxy, forged headers upstream) cannot grow it without
	// bound. A real hive has a handful of users; purely defensive.
	maxPresenceUsers = 500
)

// handlePresence records an engaged-presence ping from the dashboard frontend.
// Identity comes ONLY from the X-Hive-User header, which the auth middleware
// either injected from a server-side session or received from the trusted hub
// proxy — never from the request body. An unidentified caller (open local/dev
// dashboard with no session) is a silent no-op: there is no user to credit,
// and inventing one would be the exact dishonesty this feature removes.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Engaged bool `json:"engaged"`
	}
	// A malformed body is treated as not-engaged rather than an error: this is
	// a fire-and-forget beacon and the client ignores the response.
	r.Body = http.MaxBytesReader(w, r.Body, maxPresenceBodyBytes)
	_ = json.NewDecoder(r.Body).Decode(&body)
	user := r.Header.Get("X-Hive-User")
	if body.Engaged && user != "" {
		s.markUserEngaged(user, time.Now())
	}
	w.WriteHeader(http.StatusNoContent)
}

// markUserEngaged stamps user as engaged as of now. Lazily initializes the map
// so the zero-value Server used throughout the tests needs no constructor
// change.
func (s *Server) markUserEngaged(user string, now time.Time) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	if s.presenceEngagedAt == nil {
		s.presenceEngagedAt = make(map[string]time.Time)
	}
	if _, known := s.presenceEngagedAt[user]; !known && len(s.presenceEngagedAt) >= maxPresenceUsers {
		return
	}
	s.presenceEngagedAt[user] = now
}

// EngagedSessionUsernames returns the DISTINCT usernames whose last engaged
// presence ping is within presenceFreshness — the users a human is actually
// behind right now, as opposed to ActiveSessionUsernames' open-tab set. Stale
// entries are pruned on read so the map cannot grow without bound. Sorted for a
// stable heartbeat payload. Non-secret: bare usernames only.
func (s *Server) EngagedSessionUsernames() []string {
	cutoff := time.Now().Add(-presenceFreshness)
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	out := make([]string, 0, len(s.presenceEngagedAt))
	for user, seen := range s.presenceEngagedAt {
		if seen.Before(cutoff) {
			delete(s.presenceEngagedAt, user)
			continue
		}
		out = append(out, user)
	}
	sort.Strings(out)
	return out
}

package dashboard

// The hub's OWN decisions about one contributor, kept where an operator can read
// them (#7330 — item 4 of #7317).
//
// After #7322 a struggling contributor's RUNS are readable from the dashboard:
// what the relay reported, why, and how long it took. What stays invisible is
// the other half of the conversation — the moments the hub REFUSED, IGNORED or
// FENCED something the relay sent. Those are slog lines on the hub's stdout and
// nowhere else, so from the page a contributor whose reports are being silently
// dropped looks exactly like one whose relay never sent any.
//
// The motivating session (hosted-projectbluefin-knuckle-gjvq, 2026-09-17): a
// litellm contributor picked up 11 tasks in ~2h, completed none, and ended 10 of
// them as "released: gave the task back" with no `failed` in sight — even though
// the relay's pane-stall and CLI-ready timeouts both call failCurrentTask(),
// which sends task_failed BEFORE ready. Either those task_failed messages never
// arrived, or the hub's #2568 generation fence rejected them (the relay's
// up-front rejection paths send task_failed with no task_gen, and the fence
// drops a 0 generation once the connection has sent a non-zero one). The hub
// knows which; the operator could not.
//
// DECLARE, never ROUTE — the same boundary task_run_log.go draws. Nothing here
// is read on a decision path: recording is a bounded append under its own mutex,
// and no routing, cooldown, trust or offer consults the ring. A recording
// failure is impossible by construction (no I/O), but a recording is also never
// load-bearing: every call site keeps the slog line it already had, because the
// ring is in memory only and a hub restart empties it.
//
// IN MEMORY ONLY, and the endpoint says so. `since` on the response is the hub's
// start time, so an empty list after a restart reads as "nothing since boot"
// rather than "nothing happened". Persisting is a follow-up if the ring earns
// it; the issue deliberately did not ask for it up front.

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// The closed decision vocabulary. Closed and stable by design, exactly like
// task_run_log.go's scenario list: these strings are what an operator filters
// and what a future ratchet would count, so comparison depends on their
// spelling never changing. Extend by adding values; never rename.
const (
	// decisionStaleGenRejected: the #2568 generation fence dropped a report
	// whose task_gen no longer matches the assignment. THE event this issue
	// exists for — it is how a task_failed can vanish between a relay that
	// believes it reported and a hub that believes it never did.
	decisionStaleGenRejected = "stale_gen_rejected"
	// decisionUnassignedIgnored: a terminal report arrived for a task this
	// connection does not hold, and was dropped rather than applied.
	decisionUnassignedIgnored = "unassigned_ignored"
	// decisionAbandoned: a held task ended with no terminal report — the relay
	// asked for new work, or the socket dropped. The run log records these too
	// (#7322); the decision ring carries the hub's side of the same moment.
	decisionAbandoned = "abandoned"
	// decisionResumeRejected: a task_progress tried to adopt a task with no
	// matching server-issued lease, or the admission gate refused the resume.
	decisionResumeRejected = "resume_rejected"
	// decisionLeaseExpired: the hub auto-released a task whose lease went
	// unrenewed past its TTL.
	decisionLeaseExpired = "lease_expired"
	// decisionRefused: the hub declined to hand out work at all — role not
	// permitted, tier disabled, concurrency cap, rate limit, failure streak.
	// The specific reason rides in Detail rather than splitting the vocabulary
	// five ways, because an operator asks "why did this clanker get nothing?"
	// before they ask which of the five it was.
	decisionRefused = "refused"
)

// HubDecision is one hub-side decision about one contributor.
//
// Detail carries the structured fields the matching slog line already emits —
// the generations that disagreed, the lease that did not match, the limit that
// was hit — flattened to one operator-readable string. It is hub-authored
// throughout: no client free text reaches this struct, which is part of why the
// endpoint can serve it without a redaction pass.
type HubDecision struct {
	TS       string `json:"ts"`
	Username string `json:"username"`
	Event    string `json:"event"`
	TaskID   string `json:"task_id,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Number   int    `json:"number,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// seq is the ring's own arrival counter, unexported so it never reaches the
	// wire. Ordering CANNOT use TS: it is RFC3339 second-resolution (matching
	// the run log's format, which operators read), and a burst of fences — the
	// exact thing this ring exists to capture — lands many entries inside one
	// second. Sorting equal timestamps leaves insertion order, so "newest
	// first" silently returned oldest first, and the eviction below picked an
	// arbitrary login instead of the least recently active one.
	seq uint64
}

const (
	// hubDecisionsPerUser bounds one contributor's ring. 200 is ~an hour of the
	// motivating session's worst behaviour (11 tasks, a handful of events each)
	// with room to spare, and the whole struct is small and string-only.
	hubDecisionsPerUser = 200
	// hubDecisionUsers bounds how many DISTINCT logins are tracked. The per-user
	// cap alone does not bound the ring: one flapping relay is capped, but a
	// stream of new logins is not, and the map would grow for the life of the
	// process. On overflow the login whose newest entry is oldest is evicted
	// whole — the one an operator is least likely to be looking at.
	hubDecisionUsers = 200
)

// hubDecisionLog is the per-username ring. Its own mutex, never h.mu and never
// contributor.mu: it is written from the connection goroutines and read from an
// HTTP handler, and taking a hub lock here would put a diagnostic on the same
// lock graph as the routing it observes. Zero value is ready to use.
type hubDecisionLog struct {
	mu     sync.Mutex
	byUser map[string][]HubDecision
	// seq is the arrival counter stamped on each entry — see HubDecision.seq
	// for why TS cannot be used for ordering. Guarded by mu like byUser.
	seq uint64
	// since is stamped on first use and reported by the endpoint, so an empty
	// answer after a restart is legible as "nothing since boot".
	since time.Time
}

// record appends one decision, evicting oldest-first within the user's ring and
// least-recently-active user when the map is full.
func (l *hubDecisionLog) record(d HubDecision) {
	if l == nil || strings.TrimSpace(d.Username) == "" || d.Event == "" {
		return
	}
	d.TS = time.Now().UTC().Format(time.RFC3339)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser = make(map[string][]HubDecision)
	}
	if l.since.IsZero() {
		l.since = time.Now().UTC()
	}
	l.seq++
	d.seq = l.seq

	ring := append(l.byUser[d.Username], d)
	if len(ring) > hubDecisionsPerUser {
		// Copy forward rather than reslicing in place: a reslice keeps the old
		// backing array alive and the evicted entries with it.
		ring = append([]HubDecision(nil), ring[len(ring)-hubDecisionsPerUser:]...)
	}
	l.byUser[d.Username] = ring

	for len(l.byUser) > hubDecisionUsers {
		oldest, oldestSeq := "", uint64(0)
		for user, entries := range l.byUser {
			if len(entries) == 0 {
				oldest = user
				break
			}
			newest := entries[len(entries)-1].seq
			if oldestSeq == 0 || newest < oldestSeq {
				oldest, oldestSeq = user, newest
			}
		}
		if oldest == "" {
			break
		}
		delete(l.byUser, oldest)
	}
}

// forUser returns one contributor's decisions, newest first, at most limit.
func (l *hubDecisionLog) forUser(username string, limit int) ([]HubDecision, time.Time) {
	if l == nil {
		return []HubDecision{}, time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// The ring is already in arrival order, so newest-first is a reverse rather
	// than a sort — deterministic whatever the clock resolution, which a
	// TS-keyed sort is not (see HubDecision.seq).
	ring := l.byUser[username]
	out := make([]HubDecision, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		out = append(out, ring[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, l.since
}

// recordDecision is the hub-side entry point used by the call sites in
// contribute_ws.go. Tolerates a nil hub and a nil connection so a call site can
// never be the reason a path panics.
func (h *ContributeWSHub) recordDecision(username, event, taskID string, repo string, number int, detail string) {
	if h == nil {
		return
	}
	h.decisions.record(HubDecision{
		Username: username,
		Event:    event,
		TaskID:   taskID,
		Repo:     repo,
		Number:   number,
		Detail:   detail,
	})
}

// decisionUsername is the key the ring is written under: the GitHub LOGIN, which
// is what an operator types and what /api/contribute/activity and the run
// history already use.
//
// Deliberately not identityOf(): that returns ContributorID[#session], a
// different key space, and the refusal sites happen to log it. Keying the ring
// on it would have produced entries the endpoint could never find — visible
// only as an endpoint that always answered empty for a contributor the operator
// could see being refused in the logs.
func decisionUsername(c *ContributorConnection) string {
	if c == nil || c.profile == nil {
		return ""
	}
	return c.profile.GitHubUsername
}

// recordTaskDecision is recordDecision for the common case where the task is in
// hand, so repo/number come off the assignment rather than being threaded
// through every call site.
func (h *ContributeWSHub) recordTaskDecision(username, event string, task *WSTaskAssign, detail string) {
	if task == nil {
		h.recordDecision(username, event, "", "", 0, detail)
		return
	}
	h.recordDecision(username, event, task.TaskID, task.Repo, task.Number, detail)
}

// hubDecisionViewer reports whether this request may read the decision ring.
//
// Owner and read-write only. Unlike a run's failure reason — which
// /api/contribute/fleet already serves anonymously as last_failure — these
// events describe the hub's internal protocol state: generation numbers, lease
// identity, the rate-limit thresholds a hive is configured with. None of that is
// public anywhere else, and a stripped version would be an empty list with a
// timestamp, so the endpoint refuses rather than degrades.
//
// The empty-role case mirrors requestRoleAllowsOwner: on a spoke with any auth
// boundary an absent X-Hive-Role is an anonymous caller and gets nothing, while
// on a genuinely open spoke (no token, no direct-route authz) the whole
// dashboard is already anonymous and gating one panel from its only operator
// would be theatre.
//
// NOTE for whoever merges this after #7329: that PR introduces paneTailViewer
// with this exact predicate for pane output. They are the same gate for the same
// reason and should collapse into one helper once both have landed — left
// separate here only because #7329 is still open and this branch is based on v4.
func (s *Server) hubDecisionViewer(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role == config.RoleOwner || role == config.RoleReadWrite {
		return true
	}
	if role == "" {
		return s.authToken == "" && !s.directRouteAuthzEnabled()
	}
	return false
}

// handleContributeDecisions serves GET /api/contribute/decisions?username=<u>&limit=N.
//
// Owner/read-write only — see hubDecisionViewer. 403 rather than a stripped
// list: there is nothing useful left once the protocol fields are removed.
func (s *Server) handleContributeDecisions(w http.ResponseWriter, r *http.Request) {
	if !s.hubDecisionViewer(r) {
		jsonError(w, "hub decisions are visible to the hive's owner and read-write users only", http.StatusForbidden)
		return
	}
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	if username == "" {
		jsonError(w, "username is required", http.StatusBadRequest)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= hubDecisionsPerUser {
			limit = n
		}
	}
	var (
		decisions []HubDecision
		since     time.Time
	)
	if s.contributeHub != nil {
		decisions, since = s.contributeHub.decisions.forUser(username, limit)
	} else {
		decisions = []HubDecision{}
	}
	body := map[string]any{
		"username":  username,
		"limit":     limit,
		"returned":  len(decisions),
		"decisions": decisions,
		// In-memory only: an empty list means "nothing since the hub started",
		// which is a different claim from "nothing ever happened". Zero when the
		// ring has never been written.
		"in_memory_only": true,
	}
	if !since.IsZero() {
		body["since"] = since.Format(time.RFC3339)
	}
	jsonResponse(w, body)
}

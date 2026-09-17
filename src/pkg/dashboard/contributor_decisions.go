package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hub-side contributor decisions, made readable (#7317 item 4).
//
// When a contributor is visibly struggling, the questions an operator asks are
// "why did the hub refuse that report?" and "why did six tasks get handed back
// in the same second?". The hub already answers both — and then throws the
// answer away. Every decision below was a slog line on the hub process's
// stdout and nowhere else, which is exactly the access a hosted-hive operator
// does not have. task_run_log.go's own header already concedes the shape of
// the problem: "the slog line rotates away".
//
// #7317 offered two ways to fix it and named a preference: a structured,
// bounded, per-username event buffer rather than an operator-facing log tail,
// because a log tail turns the dashboard into a log viewer and hands back
// unstructured text an operator still has to parse. This is that buffer.
//
// DECLARE, never ROUTE. Recording is observational only: nothing here feeds
// cooldowns, trust, routing or offers, and a full buffer never changes a
// decision — it only forgets the oldest one. That is the same boundary
// task_run_log.go and contribute_protocol.go draw, and it is what makes this
// safe to call from inside the message loop.
//
// IN MEMORY ON PURPOSE. Unlike task_runs.jsonl these are not per-run facts
// worth a disk write; they are the recent context around one. Bounded twice —
// per user, and in number of users — so a hostile or merely busy fleet cannot
// grow it without limit.

// The decision vocabulary. Closed and stable by design, for the same reason
// task_run_log.go's scenario list is: these strings are what an operator (and
// any future aggregation) filters on, so week-over-week comparison depends on
// their spelling never changing. Extend by adding values; never rename.
const (
	// decisionTaskAbandoned: the relay asked for new work while still holding
	// a task. This is the event behind "released: gave the task back" on the
	// activity rail, and the one that appeared eleven times in the session
	// that prompted #7317.
	decisionTaskAbandoned = "task_abandoned"
	// decisionNoTasksAvailable: the hub had nothing to offer. Benign once;
	// a wall of them explains a contributor that looks idle but is healthy.
	decisionNoTasksAvailable = "no_tasks_available"
	// decisionStaleProgressRejected: a task_progress arrived carrying an
	// older generation than the hub's current assignment, so it was dropped.
	decisionStaleProgressRejected = "stale_progress_rejected"
	// decisionStaleFailureRejected: as above for task_failed. This is the one
	// that makes a hand-back look silent: the relay DID report a failure, and
	// the hub refused it, so the task ended as a bare `ready` instead.
	decisionStaleFailureRejected = "stale_failure_rejected"
	// decisionUnassignedCompleteIgnored: a task_complete named a task this
	// contributor did not hold, so the assignment was left intact.
	decisionUnassignedCompleteIgnored = "unassigned_complete_ignored"
)

// ContributorDecision is one hub-side decision about a named contributor.
//
// Deliberately narrow: a timestamp, the closed-vocabulary kind, the task it
// concerned, and a short structured detail. No pane output, no tokens, no free
// text copied off the wire — see the endpoint's posture note below.
type ContributorDecision struct {
	At       time.Time `json:"at"`
	Username string    `json:"username"`
	Kind     string    `json:"kind"`
	Task     string    `json:"task,omitempty"`
	// Detail is a short, hub-authored qualifier such as the client generation
	// that made a report stale. Never contributor-supplied text.
	Detail string `json:"detail,omitempty"`
}

const (
	// maxDecisionsPerUser is the ring depth. The session in #7317 produced
	// ~30 decisions over six hours, so this comfortably covers "what just
	// happened to this contributor" without holding a day of traffic.
	maxDecisionsPerUser = 50
	// maxDecisionUsers bounds how many contributors are remembered at once.
	// Far above any real fleet size; it exists so an unbounded stream of
	// distinct usernames cannot grow the map.
	maxDecisionUsers = 500
)

// contributorDecisionLog is a bounded per-username ring of recent decisions.
// Zero value is ready to use.
type contributorDecisionLog struct {
	mu sync.Mutex
	// byUser holds each user's decisions oldest-first, capped at
	// maxDecisionsPerUser.
	byUser map[string][]ContributorDecision
	// order tracks least-recently-recorded usernames first, so eviction drops
	// the contributor nobody is asking about rather than an arbitrary one.
	order []string
}

// record appends one decision for username. Unknown or empty usernames are
// dropped rather than bucketed under "": a decision nobody can look up is not
// worth the memory, and an empty key would collide unrelated contributors.
func (l *contributorDecisionLog) record(username, kind, task, detail string) {
	username = strings.TrimSpace(username)
	if username == "" || kind == "" {
		return
	}
	key := strings.ToLower(username)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser = make(map[string][]ContributorDecision)
	}
	if _, seen := l.byUser[key]; !seen && len(l.byUser) >= maxDecisionUsers {
		// Evict the least-recently-recorded user to make room.
		if len(l.order) > 0 {
			delete(l.byUser, l.order[0])
			l.order = l.order[1:]
		}
	}
	l.touchLocked(key)
	entries := append(l.byUser[key], ContributorDecision{
		At:       time.Now().UTC(),
		Username: username,
		Kind:     kind,
		Task:     task,
		Detail:   detail,
	})
	if len(entries) > maxDecisionsPerUser {
		entries = entries[len(entries)-maxDecisionsPerUser:]
	}
	l.byUser[key] = entries
}

// touchLocked moves key to the most-recently-recorded end of the order list.
func (l *contributorDecisionLog) touchLocked(key string) {
	for i, k := range l.order {
		if k == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, key)
}

// forUser returns one contributor's decisions newest first, at most limit.
//
// The username match is exact and case-insensitive, matching
// readTaskRunsForUser: GitHub logins are case-preserving but case-insensitive,
// and an operator pasting a login off the activity rail should not get an
// empty answer over capitalization.
func (l *contributorDecisionLog) forUser(username string, limit int) []ContributorDecision {
	key := strings.ToLower(strings.TrimSpace(username))
	out := []ContributorDecision{}
	if key == "" {
		return out
	}
	l.mu.Lock()
	entries := l.byUser[key]
	// Copy before releasing: the caller must never hold a slice the recorder
	// can append into. Reversed into newest-first here because an operator
	// opening this is asking "what just happened".
	//
	// Reversed, NOT sorted by timestamp. The ring is already in record order,
	// and time.Now() on a coarse clock hands several decisions in the same
	// burst an identical timestamp — exactly the burst this endpoint exists to
	// explain. A comparison sort would then leave those tied entries in
	// arrival order, i.e. oldest-first, silently inverting the answer for the
	// worst case. Insertion order is the ground truth; use it.
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i])
	}
	l.mu.Unlock()

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// recordContributorDecision is the call site helper. A method on Server so the
// message loop can record without reaching for the log directly, and so a nil
// Server (as some tests construct) is a no-op rather than a panic.
func (s *Server) recordContributorDecision(username, kind, task, detail string) {
	if s == nil {
		return
	}
	s.contributorDecisions.record(username, kind, task, detail)
}

// decisionsMaxLimit bounds one response, for the same reason taskRunsMaxLimit
// does: the buffer is already bounded, this bounds what a browser renders.
const decisionsMaxLimit = maxDecisionsPerUser

// handleContributeDecisions serves
// GET /api/contribute/decisions?username=<u>&limit=N — the hub-side decisions
// that explain a struggling contributor (#7317 item 4).
//
// Public read-only like the sibling /api/contribute* GETs, and for the same
// inherited reason handleContributeRuns gives: the username is already public
// on /api/contribute/activity, and everything else returned here is the hub's
// own bookkeeping — a closed-vocabulary kind, a task id already visible on the
// activity rail, and a generation number. No contributor-supplied text, no
// pane output (that is item 3, which wants a gate), no credential material.
func (s *Server) handleContributeDecisions(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	limit := decisionsMaxLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= decisionsMaxLimit {
			limit = n
		}
	}
	decisions := s.contributorDecisions.forUser(username, limit)
	jsonResponse(w, map[string]any{
		"username":  username,
		"limit":     limit,
		"returned":  len(decisions),
		"decisions": decisions,
	})
}

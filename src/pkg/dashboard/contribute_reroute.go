package dashboard

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
)

// Operator "Reroute to another clanker" (hivecommons/hive#15038).
//
// A PR returned to its contributor (stage 072 / 071) may sit while that
// contributor's hive is away. Instead of waiting out the offline grace
// window, an operator can hand the PR's follow-up work to another online
// registered contributor clanker. The hand-off reuses the stage-072 fix
// machinery (contribute_fixqueue.go): the PR is enqueued as a fixqueue.Item
// on the TARGET contributor's queue, flagged RerouteFrom so the fix-pr
// task_assign prompt tells the relay the PR is someone else's and how to
// carry the original commits + sign-off, and dispatched at once when the
// relay has a free slot (else at its next task boundary, ahead of new
// issues). If the target has not picked it up within
// governor.reroute.pickup_timeout the decision handler falls back to Adopt
// now (api_repo_item_decision_reroute.go).

// RerouteAny is the `to` value meaning "any online clanker".
const RerouteAny = "any"

// ErrNoRerouteTarget is returned when no online clanker can take the work.
var ErrNoRerouteTarget = errors.New("no clankers online — use Adopt now")

// RerouteNoneOnlineReason is the menu item's disabled reason when no
// contributor clanker is online.
const RerouteNoneOnlineReason = "no clankers online — use Adopt now"

// RerouteCandidate is one online registered contributor clanker that can
// take rerouted work (the Reroute submenu's rows).
type RerouteCandidate struct {
	Login string `json:"login"`
	// HiveName is the contributor's hive (its ContributorID, with the
	// relay's session label when it declared one).
	HiveName    string     `json:"hive_name,omitempty"`
	OnlineSince *time.Time `json:"online_since,omitempty"`
	// Load is the clanker's current work: its primary task (1 when set)
	// plus fix tasks in flight plus fixes queued for it.
	Load int `json:"load"`
	// Capable reports whether the relay advertised fix-pr tasks; a relay
	// without the capability is listed but cannot take a reroute.
	Capable bool `json:"capable"`
}

// RerouteCandidates lists the online, fix-pr-capable contributor clankers
// other than exclude (the PR's own author), least loaded first.
func (h *ContributeWSHub) RerouteCandidates(now time.Time, exclude string) []RerouteCandidate {
	if h == nil {
		return nil
	}
	window := h.presenceWindow()
	sched := h.fixQueue()
	byLogin := map[string]*RerouteCandidate{}
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		login := ""
		hive := ""
		if c.profile != nil {
			login = strings.TrimSpace(c.profile.GitHubUsername)
			hive = c.profile.ContributorID
		}
		if c.session != "" {
			hive += "#" + c.session
		}
		pong := c.lastPong
		since := c.connectedAt
		busy := 0
		if c.currentTask != nil {
			busy++
		}
		busy += len(c.fixTasks)
		capable := relaySupportsFixPR(c)
		c.mu.Unlock()
		if login == "" || strings.EqualFold(login, exclude) || pong.IsZero() || now.Sub(pong) > window {
			continue
		}
		cand, ok := byLogin[strings.ToLower(login)]
		if !ok {
			cand = &RerouteCandidate{Login: login, HiveName: hive}
			byLogin[strings.ToLower(login)] = cand
		}
		if !since.IsZero() && (cand.OnlineSince == nil || since.Before(*cand.OnlineSince)) {
			t := since.UTC()
			cand.OnlineSince = &t
		}
		cand.Load += busy
		cand.Capable = cand.Capable || capable
	}
	h.mu.RUnlock()
	out := make([]RerouteCandidate, 0, len(byLogin))
	for _, cand := range byLogin {
		if !cand.Capable {
			continue
		}
		cand.Load += len(sched.Queued(cand.Login))
		out = append(out, *cand)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Load != out[j].Load {
			return out[i].Load < out[j].Load
		}
		return strings.ToLower(out[i].Login) < strings.ToLower(out[j].Login)
	})
	return out
}

// RerouteRequest is one PR's follow-up work to hand to another clanker.
type RerouteRequest struct {
	Repo    string // owner/name
	Number  int
	HeadSHA string
	Title   string
	URL     string
	// Author is the PR's author (excluded from the candidates); To is the
	// requested target login or RerouteAny; By is the operator.
	Author, To, By string
	// Reason is the stage-072 return reason / the reviewer's ask, shipped
	// in the prompt; Check is the failing check when the cause is CI.
	Reason, Check         string
	HumanChangesRequested bool
	OpenedAt              time.Time
}

// RerouteOutcome says where the work went.
type RerouteOutcome struct {
	To string `json:"to"`
	// Mode is the dispatch mode ("spawn", "preempt") when the relay took
	// the task at once, else "queued".
	Mode       string `json:"mode"`
	QueueDepth int    `json:"queue_depth"`
	// RelayFailed is set when the relay refused the interrupt; the item
	// stays queued for its next task boundary.
	RelayFailed bool   `json:"relay_failed,omitempty"`
	RelayError  string `json:"relay_error,omitempty"`
}

// Reroute enqueues req on the target's fix queue and dispatches it when the
// relay has a slot. ErrNoRerouteTarget when the named clanker (or, for
// RerouteAny, every clanker) is offline or cannot take fix-pr tasks.
func (h *ContributeWSHub) Reroute(req RerouteRequest) (RerouteOutcome, error) {
	now := time.Now()
	target := strings.TrimPrefix(strings.TrimSpace(req.To), "@")
	cands := h.RerouteCandidates(now, req.Author)
	if target == "" || strings.EqualFold(target, RerouteAny) {
		if len(cands) == 0 {
			return RerouteOutcome{}, ErrNoRerouteTarget
		}
		target = cands[0].Login
	} else {
		found := false
		for _, c := range cands {
			if strings.EqualFold(c.Login, target) {
				target, found = c.Login, true
				break
			}
		}
		if !found {
			if strings.EqualFold(target, req.Author) {
				return RerouteOutcome{}, errors.New("@" + target + " is the PR's author; reroute to another clanker or use Adopt now")
			}
			return RerouteOutcome{}, errors.New("@" + target + " is not online (or its relay cannot take fix-pr tasks) — " + RerouteNoneOnlineReason)
		}
	}
	item := fixqueue.Item{
		Repo: req.Repo, Number: req.Number, HeadSHA: req.HeadSHA, Title: req.Title, URL: req.URL,
		OpenedAt: req.OpenedAt, HumanChangesRequested: req.HumanChangesRequested,
		Cause:       fixqueue.Cause{Check: req.Check, Signature: req.Reason},
		Summary:     req.Reason,
		RerouteFrom: req.Author, RerouteBy: req.By, RerouteReason: req.Reason,
	}
	// The author's own queue no longer owns the PR: a hive that comes back
	// must not race the target on the same head.
	h.CancelReroute(req.Author, req.Repo, req.Number)
	sched := h.fixQueue()
	sched.Enqueue(target, item)
	h.rememberFixOwner(target, item.Key())
	out := RerouteOutcome{To: target, Mode: "queued"}
	conns, online := h.liveConnectionsFor(target, now)
	if online {
		failed := h.beginFixWait()
		defer h.endFixWait()
		if msg := h.dispatchFixes(target, conns, false); msg != nil {
			out.Mode = msg.Mode
			if reason, refused := h.awaitFixTaskRefusal(msg.TaskID, failed); refused {
				out.Mode = "queued"
				out.RelayFailed = true
				out.RelayError = reason
			}
		}
	}
	out.QueueDepth = sched.Status(target, fixqueue.Capacity{Max: 1}).Queued
	h.logger.Info("[contribute-ws] PR rerouted", "repo", req.Repo, "pr", req.Number, "from", req.Author, "to", target, "by", req.By, "mode", out.Mode, "queued", out.QueueDepth)
	return out, nil
}

// ReroutePickedUp reports whether user's relay has taken repo#number: the
// item is in flight on a dispatch (or already completed: neither queued
// nor in flight after a dispatch happened).
func (h *ContributeWSHub) ReroutePickedUp(user, repo string, number int) bool {
	if h == nil {
		return false
	}
	sched := h.fixQueue()
	key := fixqueue.Key(repo, number)
	for _, d := range sched.InFlight(user) {
		for _, it := range d.Items {
			if it.Key() == key {
				return true
			}
		}
	}
	return false
}

// RerouteQueued reports whether repo#number still waits in user's queue.
func (h *ContributeWSHub) RerouteQueued(user, repo string, number int) bool {
	if h == nil {
		return false
	}
	key := fixqueue.Key(repo, number)
	for _, it := range h.fixQueue().Queued(user) {
		if it.Key() == key {
			return true
		}
	}
	return false
}

// CancelReroute removes repo#number from user's fix queue (the pickup window
// lapsed, or the work went elsewhere). It does not recall a task already in
// flight.
func (h *ContributeWSHub) CancelReroute(user, repo string, number int) {
	if h == nil || strings.TrimSpace(user) == "" {
		return
	}
	h.fixQueue().Remove(user, repo, number)
	h.forgetFixOwner(user, fixqueue.Key(repo, number))
}

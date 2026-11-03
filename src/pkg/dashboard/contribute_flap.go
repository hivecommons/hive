package dashboard

// Contributor flap cool-off (hivecommons/hive#13579).
//
// A relay that connects, is handed an issue, and drops the socket a minute
// later — then redials and resumes the same lease — leaves the issue claimed
// for as long as it keeps flapping. Each resume renewed the claim, each drop
// released nothing, and the hive's own agents saw "live issue claim" on
// every eval cycle: #13579 sat unclaimed by anyone who would actually work
// it for 15 hours behind 18 such drops.
//
// Two things fix that. The disconnect release now releases the claim
// (contribute_ws.go bookAbandonmentAfterGrace), so a single drop frees the
// issue for hive agents on the next cycle. And this file counts the drops
// per (contributor, issue) (#13763): once one contributor takes
// contributor_flap_threshold disconnect releases of one issue inside
// contributor_flap_window_s, that issue is withheld from that contributor —
// selectTask skips it for them and a resume of their lease is refused — for
// contributor_flap_cooloff_s, so a relay in a reconnect loop cannot keep
// re-taking the one issue it keeps dropping. Other contributors are still
// offered the issue, and hive agents are never withheld; the cool-off is
// what hands them the issue (ContributorCoolOffReason).
//
// The contributor half of the key is the registered ContributorID (or
// GitHubUsername), never the client-declared session label: a relay that
// rotated its session on every redial would otherwise mint a fresh key per
// drop and never trip.
//
// In-memory only: a hub restart forgets the drops. The claim release is the
// durable half; this is the rate limiter on top of it.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ContributorCoolOff is one contributor's active flap cool-off from one issue.
type ContributorCoolOff struct {
	// Drops is how many disconnect releases tripped the cool-off.
	Drops int
	// Window is the window the drops were counted over.
	Window time.Duration
	// Until is when contributors may be offered the issue again.
	Until time.Time
}

// Reason is the operator-facing clause shown wherever the cool-off is
// surfaced: the hub decision ring, the work detail reason, the log line.
func (c ContributorCoolOff) Reason() string {
	return fmt.Sprintf("contributor cool-off: %d drops in %s", c.Drops, c.Window.Round(time.Minute))
}

// contributorDropRecord is one (contributor, issue) pair's drop history.
type contributorDropRecord struct {
	drops []time.Time
	// coolOff is the active cool-off, zero Until when none tripped.
	coolOff ContributorCoolOff
}

// contributorFlapSettings reads governor.claims' flap knobs with defaults.
func (h *ContributeWSHub) contributorFlapSettings() (threshold int, window, cooloff time.Duration) {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return (config.ClaimsConfig{}).EffectiveContributorFlap()
	}
	return h.server.deps.Config.Governor.Claims.EffectiveContributorFlap()
}

// contributorFlapSep joins the contributor and item halves of a flap key.
// NUL cannot appear in a ContributorID, username or worksource key.
const contributorFlapSep = "\x00"

// contributorFlapKey scopes the flap tracker to one contributor's drops of
// one item (#13763): the contributor that keeps dropping an issue is cooled
// off from it, while other contributors are still offered it. The select,
// resume and release paths all derive the key here so they agree.
func contributorFlapKey(identity, itemKey string) string {
	if itemKey == "" {
		return ""
	}
	return identity + contributorFlapSep + itemKey
}

// contributorFlapIdentity is the contributor half of a flap key: the
// registered ContributorID, or GitHubUsername for profiles without one. It
// deliberately omits identityOf's "#session" suffix — the session label is
// chosen by the relay on each auth_response, so keying on it would let a
// relay that rotates the label per redial sidestep the cool-off.
func contributorFlapIdentity(c *ContributorConnection) string {
	if c == nil || c.profile == nil {
		return ""
	}
	if c.profile.ContributorID != "" {
		return c.profile.ContributorID
	}
	return c.profile.GitHubUsername
}

// noteContributorDrop records one disconnect release of the item at now.
// It returns the cool-off and true the moment the threshold trips (exactly
// once per cool-off: further drops inside an active cool-off extend
// nothing and report false), so the caller logs one INFO per trip.
func (h *ContributeWSHub) noteContributorDrop(itemKey string, now time.Time) (ContributorCoolOff, bool) {
	if h == nil || itemKey == "" {
		return ContributorCoolOff{}, false
	}
	threshold, window, cooloff := h.contributorFlapSettings()
	if threshold <= 0 {
		return ContributorCoolOff{}, false
	}
	h.flapMu.Lock()
	defer h.flapMu.Unlock()
	if h.contributorDrops == nil {
		h.contributorDrops = make(map[string]*contributorDropRecord)
	}
	rec := h.contributorDrops[itemKey]
	if rec == nil {
		rec = &contributorDropRecord{}
		h.contributorDrops[itemKey] = rec
	}
	kept := rec.drops[:0]
	for _, t := range rec.drops {
		if now.Sub(t) <= window {
			kept = append(kept, t)
		}
	}
	rec.drops = append(kept, now)
	if !rec.coolOff.Until.IsZero() && now.Before(rec.coolOff.Until) {
		return rec.coolOff, false
	}
	if len(rec.drops) < threshold {
		return ContributorCoolOff{}, false
	}
	rec.coolOff = ContributorCoolOff{Drops: len(rec.drops), Window: window, Until: now.Add(cooloff)}
	// The window's drops have been spent on this cool-off; the next one
	// needs threshold fresh drops after it lapses.
	rec.drops = nil
	return rec.coolOff, true
}

// contributorCoolOffAt reports the flap key's active cool-off at now. Lapsed
// cool-offs are dropped on read.
func (h *ContributeWSHub) contributorCoolOffAt(flapKey string, now time.Time) (ContributorCoolOff, bool) {
	if h == nil || flapKey == "" {
		return ContributorCoolOff{}, false
	}
	h.flapMu.Lock()
	defer h.flapMu.Unlock()
	return h.liveCoolOffLocked(flapKey, now)
}

// liveCoolOffLocked is contributorCoolOffAt under flapMu.
func (h *ContributeWSHub) liveCoolOffLocked(flapKey string, now time.Time) (ContributorCoolOff, bool) {
	rec, ok := h.contributorDrops[flapKey]
	if !ok || rec.coolOff.Until.IsZero() {
		return ContributorCoolOff{}, false
	}
	if !now.Before(rec.coolOff.Until) {
		rec.coolOff = ContributorCoolOff{}
		if len(rec.drops) == 0 {
			delete(h.contributorDrops, flapKey)
		}
		return ContributorCoolOff{}, false
	}
	return rec.coolOff, true
}

// inContributorCoolOff is contributorCoolOffAt at the wall clock.
func (h *ContributeWSHub) inContributorCoolOff(flapKey string) (ContributorCoolOff, bool) {
	return h.contributorCoolOffAt(flapKey, time.Now())
}

// anyContributorCoolOffAt reports whether any contributor is in an active
// cool-off from the item at now — the issue-wide view the governor wants,
// since the keys are per contributor (#13763). With several active, the one
// lasting longest is returned. Lapsed cool-offs met on the scan are dropped.
func (h *ContributeWSHub) anyContributorCoolOffAt(itemKey string, now time.Time) (ContributorCoolOff, bool) {
	if h == nil || itemKey == "" {
		return ContributorCoolOff{}, false
	}
	suffix := contributorFlapSep + itemKey
	h.flapMu.Lock()
	defer h.flapMu.Unlock()
	var best ContributorCoolOff
	found := false
	for flapKey := range h.contributorDrops {
		if !strings.HasSuffix(flapKey, suffix) {
			continue
		}
		co, ok := h.liveCoolOffLocked(flapKey, now)
		if ok && (!found || co.Until.After(best.Until)) {
			best, found = co, true
		}
	}
	return best, found
}

// ContributorCoolOffReason is the governor-facing seam: the cool-off clause
// for a GitHub issue, or "" when no contributor is cooled off from it.
// Cool-offs are per contributor (#13763), so this scans the issue's records
// for any live one: the contributor that kept dropping the issue holds (or
// held) its claim, and that cool-off is what lets hive agents take the issue.
// cmd/hive hands it to the live-claim filter so the work detail says why.
func (s *Server) ContributorCoolOffReason(repo string, number int) (string, bool) {
	if s == nil || s.contributeHub == nil || repo == "" || number <= 0 {
		return "", false
	}
	co, ok := s.contributeHub.anyContributorCoolOffAt(worksource.Ref{Repo: repo, Number: number}.Key(), time.Now())
	if !ok {
		return "", false
	}
	return co.Reason(), true
}

// releaseContributorClaimOnDisconnect is the claim half of a disconnect
// release that survived the #7838 grace window: the relay is gone (or at
// least not back), so the issue claim it carried is released — the ranked
// ledger entry, the lease's mirror of it, and (through the ledger's
// OnReleased hook) the forge marker and labels — and the drop is counted
// toward the flap cool-off. The lease itself stays, so a relay that does
// come back inside leaseTTL can still resume; the resume re-asserts the
// claim (contribute_ws.go) if nobody took the issue meanwhile.
func (h *ContributeWSHub) releaseContributorClaimOnDisconnect(c *ContributorConnection, task *WSTaskAssign, now time.Time) {
	if h == nil || c == nil || task == nil {
		return
	}
	key := task.identityKey()
	if key == "" || task.Number <= 0 {
		return
	}
	h.clearLeaseClaim(identityOf(c), task.TaskID)
	h.releaseClaimForLease(identityOf(c), key, claimReleaseReasonDisconnect)
	if co, tripped := h.noteContributorDrop(contributorFlapKey(contributorFlapIdentity(c), key), now); tripped {
		username := ""
		if c.profile != nil {
			username = c.profile.GitHubUsername
		}
		h.logger.Info("[contribute-ws] contributor cool-off: issue withheld from the dropping contributor after repeated disconnect releases (#13579)",
			"repo", task.Repo, "number", task.Number, "key", key, "last_username", username,
			"drops", co.Drops, "window", co.Window.String(), "until", co.Until.UTC().Format(time.RFC3339))
		h.recordDecision(username, decisionAbandoned, task.TaskID, task.Repo, task.Number,
			co.Reason()+"; withheld from this contributor until "+co.Until.UTC().Format(time.RFC3339))
	}
}

// claimReleaseReasonDisconnect is the ledger release reason for a
// disconnect release. It is not an expiry, so the OnReleased hook posts the
// release marker that ends the forge-visible 🔒 until=.
const claimReleaseReasonDisconnect = "released on disconnect"

// flapMu guards contributorDrops.
type contributorFlapState struct {
	flapMu           sync.Mutex
	contributorDrops map[string]*contributorDropRecord
}

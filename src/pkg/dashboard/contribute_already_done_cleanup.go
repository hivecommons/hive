package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	gh "github.com/google/go-github/v72/github"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// The unverified already-done cleanup is retried until it lands.
//
// A contributor relay can put the already-done label on an issue with the
// task credential when it reports a no_work_needed verdict. When the verdict
// does not verify, markAlreadyDoneUnverified takes that label off and posts the
// claim-worded comment. Both are single GitHub calls, and when GitHub was
// failing ("github pacing: slot would miss request deadline") they failed with
// it, a warning was logged and nothing tried again. The label then stayed for
// good: it withholds the issue from contributor offers (the skip labels in
// contribute_withheld.go) and from agent kicks, so no second verdict ever
// came to retry it. One issue sat withheld until a person took the label off
// by hand.
//
// Now the part that failed is booked here and retried by
// unverifiedAlreadyDoneCleanupLoop on its own clock until it succeeds. A
// record ends when:
//
//   - the removal succeeds, or RemoveHiveLabelAddedSince finds nothing it may
//     remove: the label is already gone, a person added (or re-added) it, or
//     the hive added it before the task was dispatched. A label a person put
//     on is never taken off;
//   - the comment is posted, or is already on the issue (IssueCommentsContain).
//     A pending comment is dropped unposted once its premise is gone: the
//     unverified hold it announces was lifted, expired or cleared (a later
//     completion with a PR, for one). A stuck label is still taken off;
//   - a verified merged claim now covers the issue (a later verdict that did
//     verify, or the claim scan): the label then means what it says, so it
//     stays and the claim-worded comment is not posted either;
//   - GitHub refuses a part for good (403, 404, 410, 422, as
//     unverifiedAlreadyDoneRefused reads it): that part is logged as given up
//     at once. A pacing or context deadline, a network error, a rate limit, a
//     5xx or an error with no GitHub status is retried;
//   - it is older than unverifiedAlreadyDoneCleanupMaxAge, logged as given up,
//     so a repository that keeps failing does not retry forever.
//
// The hive's own label removal or comment bumps the issue's updated_at, which
// would void the unverified hold the first pass booked after its own writes
// for exactly that reason. So while the hold is on the books, each retry
// first reads the issue: when its updated_at is already newer than the hold,
// someone else touched the issue, and the hold is left to lift as it should.
// Otherwise, once the retry has actually written (the label came off, or the
// comment was created rather than found), the hold's start moves to after
// that write and its end stays where it was. A failed read is a failed
// attempt: the pass writes nothing it could not account for and tries again
// next time.
//
// The ledger is persisted beside the no-PR streaks, like its neighbours, so
// a hub restart does not forget a pending cleanup.

// unverifiedAlreadyDoneCleanupsFileName is the on-disk ledger, beside the
// no-PR streaks.
const unverifiedAlreadyDoneCleanupsFileName = "unverified-already-done-cleanups.json"

// unverifiedAlreadyDoneCleanupInterval is how often the retry pass runs. It
// reads only the hub's own ledger unless a cleanup is pending.
const unverifiedAlreadyDoneCleanupInterval = 5 * time.Minute

// unverifiedAlreadyDoneCleanupMaxAge is how long a cleanup is retried before
// the pass gives up on it. A day outlasts a GitHub outage; a repository the
// hive can no longer write to stops costing calls after it.
const unverifiedAlreadyDoneCleanupMaxAge = 24 * time.Hour

// unverifiedAlreadyDoneCleanupMaxPerPass caps the cleanups one pass retries.
const unverifiedAlreadyDoneCleanupMaxPerPass = 20

// The parts of a cleanup, as the retry and give-up logs name them.
const (
	unverifiedAlreadyDonePartLabel   = "label"
	unverifiedAlreadyDonePartComment = "comment"
	unverifiedAlreadyDonePartRead    = "issue read"
)

// unverifiedAlreadyDoneCleanup is the in-memory and on-disk shape of one
// pending cleanup: what markAlreadyDoneUnverified could not finish for an
// issue, and what it needs to try again.
type unverifiedAlreadyDoneCleanup struct {
	Repo         string    `json:"repo"`
	Number       int       `json:"number"`
	DispatchedAt time.Time `json:"dispatched_at"`
	Reporter     string    `json:"reporter,omitempty"`
	HoldHours    float64   `json:"hold_hours,omitempty"`
	// Label: the relay's label is still to be taken off. Comment: the
	// claim-worded comment is still to be posted.
	Label      bool      `json:"label,omitempty"`
	Comment    bool      `json:"comment,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	Attempts   int       `json:"attempts,omitempty"`
}

// pending reports whether anything is left to do.
func (r unverifiedAlreadyDoneCleanup) pending() bool {
	return r.Label || r.Comment
}

// expired: a cleanup is retried for unverifiedAlreadyDoneCleanupMaxAge.
func (r unverifiedAlreadyDoneCleanup) expired(now time.Time) bool {
	return r.RecordedAt.IsZero() || now.Sub(r.RecordedAt) >= unverifiedAlreadyDoneCleanupMaxAge
}

// hold is the offer hold the comment states, in the length the first pass
// worded it with.
func (r unverifiedAlreadyDoneCleanup) hold() time.Duration {
	return time.Duration(r.HoldHours * float64(time.Hour))
}

func (h *ContributeWSHub) unverifiedAlreadyDoneCleanupsPath() string {
	if h != nil && h.unverifiedAlreadyDoneCleanupsFile != "" {
		return h.unverifiedAlreadyDoneCleanupsFile
	}
	return filepath.Join(filepath.Dir(h.noPRStreaksPath()), unverifiedAlreadyDoneCleanupsFileName)
}

func (h *ContributeWSHub) loadUnverifiedAlreadyDoneCleanups() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.unverifiedAlreadyDoneCleanupsPath())
	if err != nil {
		return
	}
	records := make(map[string]unverifiedAlreadyDoneCleanup)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now()
	for k, rec := range records {
		if rec.expired(now) || !rec.pending() || rec.Repo == "" || rec.Number <= 0 {
			continue
		}
		if h.unverifiedAlreadyDoneCleanups == nil {
			h.unverifiedAlreadyDoneCleanups = make(map[string]unverifiedAlreadyDoneCleanup)
		}
		h.unverifiedAlreadyDoneCleanups[k] = rec
	}
	h.logger.Info("[contribute-ws] loaded unverified already-done cleanups", "count", len(h.unverifiedAlreadyDoneCleanups))
}

func (h *ContributeWSHub) saveUnverifiedAlreadyDoneCleanups() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]unverifiedAlreadyDoneCleanup, len(h.unverifiedAlreadyDoneCleanups))
	for k, rec := range h.unverifiedAlreadyDoneCleanups {
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] unverified already-done cleanups marshal failed", "error", err)
		return
	}
	path := h.unverifiedAlreadyDoneCleanupsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] unverified already-done cleanups directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] unverified already-done cleanups write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] unverified already-done cleanups rename failed", "error", err)
	}
}

// queueUnverifiedAlreadyDoneCleanup books rec for the retry pass. It replaces
// any earlier cleanup for the same issue: the newer verdict's dispatch time
// and wording are the ones that apply.
func (h *ContributeWSHub) queueUnverifiedAlreadyDoneCleanup(rec unverifiedAlreadyDoneCleanup) {
	if h == nil || rec.Repo == "" || rec.Number <= 0 || !rec.pending() {
		return
	}
	if rec.RecordedAt.IsZero() {
		rec.RecordedAt = time.Now()
	}
	key := worksource.Ref{Repo: rec.Repo, Number: rec.Number}.Key()
	h.completedMu.Lock()
	if h.unverifiedAlreadyDoneCleanups == nil {
		h.unverifiedAlreadyDoneCleanups = make(map[string]unverifiedAlreadyDoneCleanup)
	}
	h.unverifiedAlreadyDoneCleanups[key] = rec
	h.completedMu.Unlock()
	h.saveUnverifiedAlreadyDoneCleanups()
}

// dropUnverifiedAlreadyDoneCleanup forgets the pending cleanup for an issue,
// if there is one.
func (h *ContributeWSHub) dropUnverifiedAlreadyDoneCleanup(repo string, number int) {
	if h == nil || repo == "" || number <= 0 {
		return
	}
	key := worksource.Ref{Repo: repo, Number: number}.Key()
	h.completedMu.Lock()
	_, had := h.unverifiedAlreadyDoneCleanups[key]
	if had {
		delete(h.unverifiedAlreadyDoneCleanups, key)
	}
	h.completedMu.Unlock()
	if had {
		h.saveUnverifiedAlreadyDoneCleanups()
	}
}

// unverifiedAlreadyDoneCleanupLoop runs the retry pass on its own clock, like
// blockedClaimsLoop and authorReplyReleaseLoop, so a slow GitHub call never
// delays heartbeat reaping, and passes never overlap.
func (h *ContributeWSHub) unverifiedAlreadyDoneCleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			h.sweepUnverifiedAlreadyDoneCleanups(ctx, time.Now())
			cancel()
		}
	}
}

// sweepUnverifiedAlreadyDoneCleanups retries every pending unverified
// already-done cleanup, oldest first, and drops the ones that finished, no
// longer apply, were refused or have run out of time. It returns how many
// records a retry closed this pass (expired ones are not counted).
func (h *ContributeWSHub) sweepUnverifiedAlreadyDoneCleanups(ctx context.Context, now time.Time) int {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return 0
	}
	h.completedMu.Lock()
	if len(h.unverifiedAlreadyDoneCleanups) == 0 {
		h.completedMu.Unlock()
		return 0
	}
	var pending, expired []unverifiedAlreadyDoneCleanup
	for key, rec := range h.unverifiedAlreadyDoneCleanups {
		if rec.expired(now) || !rec.pending() {
			delete(h.unverifiedAlreadyDoneCleanups, key)
			if rec.pending() {
				expired = append(expired, rec)
			}
			continue
		}
		pending = append(pending, rec)
	}
	h.completedMu.Unlock()
	for _, rec := range expired {
		h.logger.Warn("[contribute-ws] giving up on the unverified already-done cleanup; a maintainer should check the issue",
			"repo", rec.Repo, "number", rec.Number, "label_pending", rec.Label, "comment_pending", rec.Comment,
			"attempts", rec.Attempts, "recorded_at", rec.RecordedAt.UTC().Format(time.RFC3339))
	}
	dirty := len(expired) > 0
	sort.Slice(pending, func(i, j int) bool { return pending[i].RecordedAt.Before(pending[j].RecordedAt) })

	finished := 0
	for i, rec := range pending {
		if i >= unverifiedAlreadyDoneCleanupMaxPerPass || ctx.Err() != nil {
			break
		}
		key := worksource.Ref{Repo: rec.Repo, Number: rec.Number}.Key()
		next := h.retryUnverifiedAlreadyDoneCleanup(ctx, key, rec)
		if !h.storeUnverifiedAlreadyDoneCleanup(key, rec.RecordedAt, next) {
			continue
		}
		dirty = true
		if !next.pending() {
			finished++
		}
	}
	if dirty {
		h.saveUnverifiedAlreadyDoneCleanups()
	}
	return finished
}

// retryUnverifiedAlreadyDoneCleanup makes one attempt at rec and returns what
// is still left to do.
func (h *ContributeWSHub) retryUnverifiedAlreadyDoneCleanup(ctx context.Context, key string, rec unverifiedAlreadyDoneCleanup) unverifiedAlreadyDoneCleanup {
	next := rec
	next.Attempts++
	if claim, ok := h.issueClaimedByOpenPR(rec.Repo, "", rec.Number); ok && claim.MergedPR {
		// The label now stands for a verified merged claim: leave it,
		// and say nothing claim-worded over it.
		h.logger.Info("[contribute-ws] unverified already-done cleanup dropped: a verified merged claim now covers the issue",
			"repo", rec.Repo, "number", rec.Number, "pr", claim.PRNumber)
		next.Label, next.Comment = false, false
		return next
	}
	hold, live := h.liveUnverifiedAlreadyDoneHold(key, time.Now())
	if next.Comment && !live {
		// The comment announces a hold that is no longer on the books.
		h.logger.Info("[contribute-ws] unverified already-done comment dropped: its offer hold was lifted or cleared",
			"repo", rec.Repo, "number", rec.Number)
		next.Comment = false
	}
	if !next.pending() {
		return next
	}
	// While the hold stands, read the issue first: activity newer than the
	// hold is someone else's, and the hold must stay free to lift on it.
	restamp := false
	if live {
		snap, err := h.server.deps.GHClient.ReadIssueSnapshot(ctx, rec.Repo, rec.Number)
		if err != nil {
			if !h.unverifiedAlreadyDoneRetryable(rec.Repo, rec.Number, unverifiedAlreadyDonePartRead, next.Attempts, err) {
				next.Label, next.Comment = false, false
			}
			return next
		}
		restamp = !snap.UpdatedAt.After(hold.RecordedAt)
	}
	wrote := false
	if next.Label {
		removed, err := h.removeUnverifiedAlreadyDoneLabel(ctx, rec.Repo, rec.Number, rec.DispatchedAt)
		if err != nil {
			next.Label = h.unverifiedAlreadyDoneRetryable(rec.Repo, rec.Number, unverifiedAlreadyDonePartLabel, next.Attempts, err)
		} else {
			next.Label = false
			wrote = wrote || removed
		}
	}
	if next.Comment {
		created, err := h.noteAlreadyDoneUnverified(ctx, rec.Repo, rec.Number, rec.Reporter, rec.hold())
		if err != nil {
			next.Comment = h.unverifiedAlreadyDoneRetryable(rec.Repo, rec.Number, unverifiedAlreadyDonePartComment, next.Attempts, err)
		} else {
			next.Comment = false
			wrote = wrote || created
		}
	}
	if wrote && restamp {
		h.restampUnverifiedAlreadyDoneHold(key, hold.RecordedAt, time.Now())
	}
	return next
}

// unverifiedAlreadyDoneRefused reports whether err is GitHub refusing a call
// for good (403, 404, 410, 422) rather than failing for now, so retrying it
// would only spend calls until the maximum age. Whatever
// ghpkg.IsRetryableError calls transient (a pacing or context deadline, a
// network error, a rate limit, a 5xx) is not a refusal, and neither is an
// error that carries no GitHub status at all (a journal refusal, say).
func unverifiedAlreadyDoneRefused(err error) bool {
	if err == nil || ghpkg.IsRetryableError(err) {
		return false
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	switch ghErr.Response.StatusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// unverifiedAlreadyDoneRetryable logs one failed part of a cleanup and
// reports whether it stays pending: true for a failure a later pass may get
// past, false (logged as given up) when GitHub refused it for good.
func (h *ContributeWSHub) unverifiedAlreadyDoneRetryable(repo string, number int, part string, attempts int, err error) bool {
	if unverifiedAlreadyDoneRefused(err) {
		if h.logger != nil {
			h.logger.Warn("[contribute-ws] giving up on the unverified already-done cleanup; a maintainer should check the issue",
				"repo", repo, "number", number, "part", part, "attempts", attempts, "error", err.Error())
		}
		return false
	}
	if h.logger != nil {
		h.logger.Warn("[contribute-ws] unverified already-done cleanup failed; a later pass retries it",
			"repo", repo, "number", number, "part", part, "attempts", attempts, "error", err.Error())
	}
	return true
}

// liveUnverifiedAlreadyDoneHold returns the issue's unverified already-done
// offer hold while it is on the books and inside its window.
func (h *ContributeWSHub) liveUnverifiedAlreadyDoneHold(key string, now time.Time) (noWorkVerdictRecord, bool) {
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	rec, ok := h.noWorkVerdicts[key]
	if !ok || !rec.AlreadyDoneUnverified || now.Sub(rec.RecordedAt) >= rec.suppressWindow() {
		return noWorkVerdictRecord{}, false
	}
	return rec, true
}

// storeUnverifiedAlreadyDoneCleanup writes back the outcome of one retry, or
// drops the record when nothing is left to do. It changes nothing when a newer
// verdict replaced or dropped the record during the pass (its RecordedAt no
// longer matches), and reports whether it changed the ledger.
func (h *ContributeWSHub) storeUnverifiedAlreadyDoneCleanup(key string, recordedAt time.Time, next unverifiedAlreadyDoneCleanup) bool {
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	cur, ok := h.unverifiedAlreadyDoneCleanups[key]
	if !ok || !cur.RecordedAt.Equal(recordedAt) {
		return false
	}
	if next.pending() {
		h.unverifiedAlreadyDoneCleanups[key] = next
	} else {
		delete(h.unverifiedAlreadyDoneCleanups, key)
	}
	return true
}

// restampUnverifiedAlreadyDoneHold moves the start of the issue's unverified
// already-done hold to now, after the hive's own retry wrote to the issue, so
// that write does not count as the newer activity that lifts the hold. The
// hold's end stays where it was. It changes nothing unless the hold is still
// the one the caller read (its RecordedAt is unchanged): a hold replaced or
// lifted meanwhile is not the retry's to move.
func (h *ContributeWSHub) restampUnverifiedAlreadyDoneHold(key string, readAt, now time.Time) {
	h.completedMu.Lock()
	rec, ok := h.noWorkVerdicts[key]
	if !ok || !rec.AlreadyDoneUnverified || !rec.RecordedAt.Equal(readAt) || !now.After(rec.RecordedAt) {
		h.completedMu.Unlock()
		return
	}
	left := rec.suppressWindow() - now.Sub(rec.RecordedAt)
	if left <= 0 {
		h.completedMu.Unlock()
		return
	}
	rec.RecordedAt = now
	rec.SuppressHours = left.Hours()
	h.noWorkVerdicts[key] = rec
	h.completedMu.Unlock()
	h.saveNoWorkVerdicts()
}

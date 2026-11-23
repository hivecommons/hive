package dashboard

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The Overview card's two caches. Lock order: cardStashMu and
// cardDispatchMu are leaves. Nothing takes another lock while holding
// either, and neither is ever held across disk I/O, so the card may read
// them under statusMu without waiting on a file. The stash is built once per
// publish outside statusMu and stored under statusMu's write lock together
// with the snapshot it belongs to (a dropped build never stores one). The
// dispatch state is read lazily by each serving entry point before it takes
// statusMu.

// overviewCardCaches are the Server's card caches (embedded in Server).
type overviewCardCaches struct {
	cardStashMu    sync.Mutex
	cardStash      *cardStash
	cardDispatchMu sync.Mutex
	cardDispatch   cardDispatchCache
}

// cardStash is the publish-time copy of what the card reads besides the
// rows: the contributor queue's verdicts from the ladder pass
// UpdateStatusIfFresh already runs, the stage-record fields and the
// write-back gate's bot-rebase answer for every row of the snapshot being
// published. A stored stash is never mutated; each publish stores a new one.
type cardStash struct {
	// at is when the admission verdicts were taken (zero: none, as on a
	// hive with no contributor hub); admissionSeq is the StatusSeq of the
	// snapshot the ladder read, for display only.
	at           time.Time
	admissionSeq int64
	// queued, withheld and queueHeld are keyed by cardStashKey.
	queued    map[string]bool
	withheld  map[string]AdmissionWithheldItem
	queueHeld map[string]string
	// capped is set when the ladder's bound may have cut the queue or the
	// withheld list: rows past it have no verdict.
	capped bool
	// builtAt is when the published snapshot's rows were built; stages are
	// the stage records copied for them.
	builtAt time.Time
	stages  map[string]cardStageFacts
	// botRebaseGated is set when a write-back gate is installed; botRebase
	// then holds, per dependency-bot candidate PR, whether the gate would
	// let the hive ask its bot to rebase (cardGateAllows).
	botRebaseGated bool
	botRebase      map[string]bool
}

// cardStageFacts are the stage-record fields the card reads.
type cardStageFacts struct {
	Stage            stagelabels.Stage
	Reason           string
	EnteredAt        time.Time
	AdoptionBlocked  string
	PersonNoticeAt   time.Time
	LabelsSuppressed bool
}

// cardStashKey is the lower-case owner/repo#N every stash map is keyed by.
func cardStashKey(repoFull string, number int) string {
	return strings.ToLower(strings.TrimSpace(repoFull)) + "#" + strconv.Itoa(number)
}

// cardStashSnapshot returns the stored stash (nil before the first publish).
func (s *Server) cardStashSnapshot() *cardStash {
	s.cardStashMu.Lock()
	defer s.cardStashMu.Unlock()
	return s.cardStash
}

// storeCardStash replaces the stored stash. UpdateStatusIfFresh calls it
// under statusMu's write lock, beside the snapshot it was built for.
func (s *Server) storeCardStash(st *cardStash) {
	if s == nil || st == nil {
		return
	}
	s.cardStashMu.Lock()
	defer s.cardStashMu.Unlock()
	s.cardStash = st
}

// newCardStash starts the stash for a snapshot whose rows were built at
// builtAt.
func newCardStash(builtAt time.Time) *cardStash {
	return &cardStash{builtAt: builtAt, stages: map[string]cardStageFacts{}}
}

// addAdmission keeps the verdicts of the admission pass UpdateStatusIfFresh
// runs for standby dispatch (withheldAll), keyed for the card. It changes
// nothing the pass decided: .queue is read, never edited.
func (st *cardStash) addAdmission(snap queueAdmissionSnapshot, seq int64, at time.Time) {
	queued := make(map[string]bool, len(snap.queue))
	queueHeld := map[string]string{}
	for _, item := range snap.queue {
		key := cardAdmissionKey(item.Key, item.Repo, item.Number)
		if key == "" {
			continue
		}
		if item.Held {
			queueHeld[key] = item.HeldReason
			continue
		}
		queued[key] = true
	}
	withheld := make(map[string]AdmissionWithheldItem, len(snap.withheld))
	for _, item := range snap.withheld {
		if key := cardAdmissionKey(item.Key, item.Repo, item.Number); key != "" {
			withheld[key] = item
		}
	}
	st.at, st.admissionSeq = at.UTC(), seq
	st.queued, st.withheld, st.queueHeld = queued, withheld, queueHeld
	// The collector stops at the limit without saying whether more were
	// refused, so a full list counts as capped.
	st.capped = snap.offerableTotal > readyQueueDefaultLimit || len(snap.withheld) >= readyQueueDefaultLimit
}

// cardAdmissionKey keys a queue or withheld row: its canonical key when it
// is owner/repo#N, else repo#number. An external work key never matches a
// snapshot row.
func cardAdmissionKey(key, repo string, number int) string {
	if key = strings.TrimSpace(key); key != "" {
		return strings.ToLower(key)
	}
	if repo == "" || number <= 0 {
		return ""
	}
	return cardStashKey(repo, number)
}

// addStages copies the stage-record fields the card reads for every issue
// and PR row of the snapshot being published. It runs beside
// attachSuppressedStages, before the status lock; store.Get is a map read.
func (st *cardStash) addStages(status *StatusPayload, store *stagelabels.Store) {
	if status == nil || store == nil {
		return
	}
	copyRecord := func(repoFull string, number int, head string) {
		if number <= 0 {
			return
		}
		rec, ok := store.Get(stagelabels.Key(repoFull, number))
		if !ok {
			return
		}
		st.stages[cardStashKey(repoFull, number)] = cardStageFacts{
			Stage:            rec.Stage,
			Reason:           rec.Reason,
			EnteredAt:        rec.EnteredAt,
			AdoptionBlocked:  rec.AdoptionBlocked(head),
			PersonNoticeAt:   rec.PersonNoticeAt,
			LabelsSuppressed: rec.LabelsSuppressed,
		}
	}
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		for _, held := range []bool{false, true} {
			for _, issue := range frontendRepoIssues(repo, held) {
				copyRecord(cardRepoFull(issue.Repo, repoName), issue.Number, "")
			}
			for _, entry := range frontendRepoPRs(repo, held) {
				copyRecord(cardRepoFull(entry.pr.Repo, repoName), entry.pr.Number, entry.pr.HeadSHA)
			}
		}
		for _, raw := range repo.NonActionableIssues {
			if item, ok := repoWorkIssueRow(raw); ok {
				copyRecord(cardRepoFull(item.Repo, repoName), item.Number, "")
			}
		}
		for _, raw := range repo.NonActionablePrs {
			if item, ok := repoWorkPR(raw); ok {
				copyRecord(cardRepoFull(item.Repo, repoName), item.Number, item.HeadSHA)
			}
		}
	}
}

// addBotRebase records, for every open or held PR of the snapshot that is a
// dependency-bot rebase candidate, whether the write-back gate would let the
// hive ask its bot to rebase. policy is nil when no gate is installed, which
// allows everything, as a nil gate does in cmd/hive.
func (st *cardStash) addBotRebase(status *StatusPayload, policy *cardGatePolicy) {
	if status == nil || policy == nil {
		return
	}
	st.botRebaseGated = true
	st.botRebase = map[string]bool{}
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		for _, held := range []bool{false, true} {
			for _, entry := range frontendRepoPRs(repo, held) {
				if !cardBotRebaseCandidate(entry.pr) {
					continue
				}
				repoFull := cardRepoFull(entry.pr.Repo, repoName)
				st.botRebase[cardStashKey(repoFull, entry.pr.Number)] = cardGateAllows(*policy, repoFull, cardBotRebaseItem(entry.pr))
			}
		}
	}
}

// cardGatePolicyNow copies the write-back gate's policy for the card, or
// nil when no gate is installed. It takes the gate's mutex, so it runs only
// at publish, outside statusMu.
func (s *Server) cardGatePolicyNow(now time.Time) *cardGatePolicy {
	g := s.writebackGateRef()
	if g == nil {
		return nil
	}
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	p := cardGatePolicyFor(g.Config(), g.Snapshot().Repos, cfg, now)
	return &p
}

// cardDispatchTTL is how long the cached fix dispatch state is served
// before the file's mtime is checked again.
const cardDispatchTTL = 30 * time.Second

// cardDispatchCache is the lazily loaded fix dispatch state. load and stat
// replace the file reads in tests; nil reads review.ReviewDispatchStatePath.
type cardDispatchCache struct {
	state     review.DispatchState
	loaded    bool
	checkedAt time.Time
	mtime     time.Time
	load      func() (review.DispatchState, time.Time, error)
	stat      func() time.Time
}

// cardDispatchLoadFile reads the dispatch state file and its mtime.
func cardDispatchLoadFile() (review.DispatchState, time.Time, error) {
	mtime := cardDispatchStatFile()
	state, err := review.LoadDispatchState("")
	return state, mtime, err
}

// cardDispatchStatFile reads the dispatch state file's mtime: the durable
// path, else the legacy path LoadDispatchState falls back to.
func cardDispatchStatFile() time.Time {
	for _, path := range []string{review.ReviewDispatchStatePath, review.LegacyReviewDispatchStatePath} {
		if info, err := os.Stat(path); err == nil {
			return info.ModTime()
		}
	}
	return time.Time{}
}

// cardFixDispatch returns the cached fix dispatch state, reloading it when
// it is older than cardDispatchTTL and the file changed. Serving entry
// points call it BEFORE taking statusMu: it may read a file. The file is
// read without cardDispatchMu held and the result swapped in under it, so
// cardDispatchCached (read under statusMu) never waits on I/O. A failed
// read keeps the last good copy and is not retried inside the TTL.
func (s *Server) cardFixDispatch(now time.Time) (review.DispatchState, bool) {
	if s == nil {
		return review.DispatchState{}, false
	}
	s.cardDispatchMu.Lock()
	cached := s.cardDispatch
	s.cardDispatchMu.Unlock()
	if !cached.checkedAt.IsZero() && now.Sub(cached.checkedAt) < cardDispatchTTL {
		return cached.state, cached.loaded
	}
	load, stat := cached.load, cached.stat
	if load == nil {
		load = cardDispatchLoadFile
	}
	if stat == nil {
		stat = cardDispatchStatFile
	}
	if cached.loaded && stat().Equal(cached.mtime) {
		s.cardDispatchMu.Lock()
		s.cardDispatch.checkedAt = now
		s.cardDispatchMu.Unlock()
		return cached.state, true
	}
	state, mtime, err := load()
	s.cardDispatchMu.Lock()
	defer s.cardDispatchMu.Unlock()
	c := &s.cardDispatch
	c.checkedAt = now
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.state, c.loaded
	}
	c.state, c.mtime, c.loaded = state, mtime, true
	return c.state, true
}

// cardDispatchCached returns the cached copy without any I/O.
func (s *Server) cardDispatchCached() (review.DispatchState, bool) {
	s.cardDispatchMu.Lock()
	defer s.cardDispatchMu.Unlock()
	return s.cardDispatch.state, s.cardDispatch.loaded
}

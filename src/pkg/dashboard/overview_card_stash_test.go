package dashboard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestRecordCardAdmissionKeysTheVerdicts(t *testing.T) {
	snap := queueAdmissionSnapshot{
		queue: []ReadyQueueItem{
			{Repo: "Octo/Demo", Number: 1, Key: "Octo/Demo#1"},
			{Repo: "octo/demo", Number: 2, Key: "octo/demo#2", Held: true, HeldReason: "waiting on design"},
		},
		withheld:       []AdmissionWithheldItem{{Repo: "octo/demo", Number: 3, Key: "octo/demo#3", Reason: withheldReasonTracker}, {Repo: "octo/demo", Number: 4, Reason: withheldReasonCooldown}},
		offerableTotal: 1,
	}
	st := newCardStash(cardTestNow)
	st.addAdmission(snap, 42, cardTestNow)
	if st.at.IsZero() || st.admissionSeq != 42 || st.capped {
		t.Fatalf("stash = %+v", st)
	}
	if !st.queued["octo/demo#1"] {
		t.Errorf("admitted key not lower-case owner/repo#N: %v", st.queued)
	}
	if st.queueHeld["octo/demo#2"] != "waiting on design" || st.queued["octo/demo#2"] {
		t.Errorf("held queue row: queued %v held %v", st.queued, st.queueHeld)
	}
	if st.withheld["octo/demo#3"].Reason != withheldReasonTracker || st.withheld["octo/demo#4"].Reason != withheldReasonCooldown {
		t.Errorf("withheld = %v", st.withheld)
	}
	// A row whose Repo is the bare name finds its verdict through the card's
	// owner/repo.
	in := cardTestInputs(st)
	if v, ok := in.verdictFor(cardRepoFull("demo", "octo/demo"), 3); !ok || v.item.Reason != withheldReasonTracker {
		t.Errorf("bare-name row did not find its verdict: %+v %v", v, ok)
	}

	st.addAdmission(queueAdmissionSnapshot{offerableTotal: readyQueueDefaultLimit + 1}, 43, cardTestNow)
	if !st.capped {
		t.Error("an offerable total past the cap must set capped")
	}
	st.addAdmission(queueAdmissionSnapshot{withheld: make([]AdmissionWithheldItem, readyQueueDefaultLimit)}, 44, cardTestNow)
	if !st.capped {
		t.Error("a full withheld list must set capped")
	}
	card := buildOverviewCard(newCardTally(), FrontendOverviewTotals{}, st)
	if !card.Admission.Present || !card.Admission.Capped || card.Admission.AdmissionSeq != 44 {
		t.Errorf("admission block = %+v", card.Admission)
	}
}

func TestRecordCardStagesCoversEveryRow(t *testing.T) {
	store := stagelabels.NewStore("", nil)
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "demo", Full: "octo/demo",
		ActionableIssues:    []any{github.Issue{Repo: "demo", Number: 1}},
		HeldIssues:          []any{github.HoldItem{Repo: "octo/demo", Number: 2, Type: "issue"}},
		NonActionableIssues: []any{github.RepoWorkIssue{Repo: "octo/demo", Number: 3, Bucket: repoWorkBucketFiltered}},
		OpenPrs:             []any{FrontendPR{PullRequest: github.PullRequest{Repo: "octo/demo", Number: 4, HeadSHA: "abc"}}},
		HeldPrs:             []any{github.PullRequest{Repo: "octo/demo", Number: 5}},
		NonActionablePrs:    []any{github.RepoWorkPR{Repo: "octo/demo", Number: 6, Draft: true}},
	}}}
	for n := 1; n <= 6; n++ {
		stage := stagelabels.ReviewPending
		if n <= 3 {
			stage = stagelabels.Actionable
		}
		store.Observe(stagelabels.Key("octo/demo", n), stage, "reason")
	}
	st := newCardStash(cardTestNow)
	st.addStages(status, store)
	for n := 1; n <= 6; n++ {
		if _, ok := st.stages[cardStashKey("octo/demo", n)]; !ok {
			t.Errorf("row #%d has no stage facts (%v)", n, st.stages)
		}
	}
	if f := st.stages["octo/demo#4"]; f.Stage != stagelabels.ReviewPending || f.Reason != "reason" || f.EnteredAt.IsZero() {
		t.Errorf("facts for #4 = %+v", f)
	}
	// Adding the admission half keeps the stage copy.
	st.addAdmission(queueAdmissionSnapshot{}, 7, cardTestNow)
	if len(st.stages) != 6 || st.admissionSeq != 7 {
		t.Errorf("admission record dropped the stage copy: %+v", st)
	}
}

// A dropped build never stores its stash: UpdateStatusIfFresh stores it
// under the status write lock, right where the snapshot is published.
func TestCardStashIsStoredOnlyWithThePublishedSnapshot(t *testing.T) {
	s := newTestServer()
	first := minimalPayload()
	if !s.UpdateStatusIfFresh(first, s.BeginStatusSnapshot()) {
		t.Fatal("first publish dropped")
	}
	published := s.cardStashSnapshot()
	if published == nil {
		t.Fatal("no stash stored with the published snapshot")
	}
	epoch := s.BeginStatusSnapshot()
	s.noteStatusMutation()
	if s.UpdateStatusIfFresh(minimalPayload(), epoch) {
		t.Skip("the stale build was published (starvation guard); nothing to check")
	}
	if s.cardStashSnapshot() != published {
		t.Fatal("a dropped build replaced the stash")
	}
}

func TestCardFixDispatchIsLazyAndCached(t *testing.T) {
	s := &Server{}
	if _, loaded := s.cardDispatchCached(); loaded {
		t.Fatal("dispatch state loaded before anyone asked")
	}
	loads := 0
	mtime := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	s.cardDispatch.load = func() (review.DispatchState, time.Time, error) {
		loads++
		return review.DispatchState{FixAttempts: []review.FixAttempt{{Repo: "octo/demo", Number: loads}}}, mtime, nil
	}
	s.cardDispatch.stat = func() time.Time { return mtime }
	now := cardTestNow
	if _, ok := s.cardFixDispatch(now); !ok || loads != 1 {
		t.Fatalf("first call: loaded %v, loads %d", ok, loads)
	}
	s.cardFixDispatch(now.Add(10 * time.Second))
	if loads != 1 {
		t.Errorf("reloaded inside the TTL: %d loads", loads)
	}
	s.cardFixDispatch(now.Add(cardDispatchTTL + time.Second))
	if loads != 1 {
		t.Errorf("reloaded an unchanged file: %d loads", loads)
	}
	mtime = mtime.Add(time.Minute)
	state, _ := s.cardFixDispatch(now.Add(3 * cardDispatchTTL))
	if loads != 2 || state.FixAttempts[0].Number != 2 {
		t.Errorf("did not reload a changed file: %d loads, %+v", loads, state)
	}
	if cached, ok := s.cardDispatchCached(); !ok || !reflect.DeepEqual(cached, state) {
		t.Errorf("cached copy %+v differs from the loaded one", cached)
	}
	missing := &Server{}
	missing.cardDispatch.load = func() (review.DispatchState, time.Time, error) {
		return review.DispatchState{}, time.Time{}, os.ErrNotExist
	}
	if _, ok := missing.cardFixDispatch(now); !ok {
		t.Error("a missing dispatch file reads as an empty, loaded state")
	}
}

// Inside statusMu the card reads only its caches: the dispatch loader is
// never called there (it panics if it is), and nothing it builds reads a
// file or the stage store.
func TestStatusWithOverviewBandsReadsNoFileUnderTheLock(t *testing.T) {
	s := &Server{}
	s.cardDispatch.load = func() (review.DispatchState, time.Time, error) {
		if !s.statusMu.TryLock() {
			panic("the dispatch state was loaded under statusMu")
		}
		s.statusMu.Unlock()
		return review.DispatchState{}, time.Time{}, nil
	}
	s.cardDispatch.stat = func() time.Time { return time.Time{} }
	status := cardPartitionStatus()
	s.statusMu.RLock()
	out := s.statusWithOverviewBands(status, cardTestNow)
	s.statusMu.RUnlock()
	if out.OverviewCard == nil {
		t.Fatal("no overview_card on the served copy")
	}
	if _, loaded := s.cardDispatchCached(); loaded {
		t.Fatal("statusWithOverviewBands loaded the dispatch state")
	}
	// Outside the lock the entry point may load it.
	s.cardFixDispatch(cardTestNow)
	if _, loaded := s.cardDispatchCached(); !loaded {
		t.Fatal("cardFixDispatch did not load")
	}
}

// The status lock, the stage store's saves and the card's stash never
// deadlock or race (run under -race in CI).
func TestOverviewCardConcurrentWithStoreSaves(t *testing.T) {
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stages.json"), nil)
	s := &Server{stageStore: store}
	s.cardDispatch.load = func() (review.DispatchState, time.Time, error) { return review.DispatchState{}, time.Time{}, nil }
	s.cardDispatch.stat = func() time.Time { return time.Time{} }
	status := cardPartitionStatus()
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	var wg sync.WaitGroup
	const rounds = 40
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			store.Observe(stagelabels.Key(cardTestRepo, 20+i%3), stagelabels.ReviewPending, "r")
			_ = store.Save()
			st := newCardStash(cardTestNow)
			st.addStages(status, store)
			s.storeCardStash(st)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			s.cardFixDispatch(cardTestNow.Add(time.Duration(i) * time.Minute))
			s.statusMu.RLock()
			_ = s.statusWithOverviewBands(s.status, cardTestNow)
			s.statusMu.RUnlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			st := newCardStash(cardTestNow)
			st.addAdmission(queueAdmissionSnapshot{}, int64(i), cardTestNow)
			s.storeCardStash(st)
			s.statusMu.Lock()
			s.statusSeq++
			s.statusMu.Unlock()
		}
	}()
	wg.Wait()
}

// The scope switch at the publish path keeps the standby queue identical and
// adds no ladder run: the same hooks are asked the same number of times.
// #601 reaches the owner-grace step (the hook holds it) and #700 passes it.
func TestAdmissionScopeAllKeepsTheQueue(t *testing.T) {
	hub, s := withheldHub(t, nil)
	calls := map[withheldScope]int{}
	var scope withheldScope
	s.deps.ContributorHold = func(_ string, number int, _ time.Time) string {
		calls[scope]++
		if number == 601 {
			return "filed by an owner moments ago"
		}
		return ""
	}
	scope = withheldNone
	none := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldNone)
	scope = withheldAll
	all := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	if !reflect.DeepEqual(none.queue, all.queue) {
		t.Fatalf("queue differs:\n none %+v\n all  %+v", none.queue, all.queue)
	}
	if calls[withheldNone] == 0 || calls[withheldNone] != calls[withheldAll] {
		t.Errorf("owner-grace hook calls: none %d, all %d (want equal and above 0)", calls[withheldNone], calls[withheldAll])
	}
	if len(none.withheld) != 0 {
		t.Errorf("withheldNone kept %d rows", len(none.withheld))
	}
	if item, ok := findWithheld(all.withheld, withheldKey601); !ok || item.Reason != contributorAdmissionReasonOwnerGrace {
		t.Errorf("withheldAll did not keep #601's owner-grace refusal: %+v", all.withheld)
	}
}

// UpdateStatusIfFresh runs the admission ladder once per publish, with
// withheldAll, and keeps its verdicts for the card.
func TestUpdateStatusIfFreshRunsTheLadderOnce(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (s *Server) UpdateStatusIfFresh(")
	if start < 0 {
		t.Fatal("UpdateStatusIfFresh not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	fn := body[start : start+end]
	if n := strings.Count(fn, "admissionQueueSnapshot("); n != 1 {
		t.Fatalf("UpdateStatusIfFresh calls admissionQueueSnapshot %d times, want 1", n)
	}
	for _, want := range []string{"admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)", "cardNext.addAdmission(", "cardNext.addStages(", "s.buildCardStash(status)"} {
		if !strings.Contains(fn, want) {
			t.Fatalf("UpdateStatusIfFresh must contain %s", want)
		}
	}
	// The stash is stored with the snapshot, after the stale-drop check.
	drop := strings.Index(fn, "return false\n\t\t}\n\t}")
	store := strings.Index(fn, "s.storeCardStash(cardNext)")
	publish := strings.Index(fn, "s.status = status")
	if drop < 0 || store < 0 || store < drop || store < publish {
		t.Fatalf("storeCardStash must follow the stale-drop return and the publish (drop %d, publish %d, store %d)", drop, publish, store)
	}
}

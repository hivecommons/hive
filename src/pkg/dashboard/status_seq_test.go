package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Regression tests for #4348: the restart-count reset flickered back to the
// stale value because the cached status snapshot is rebuilt asynchronously
// after a mutation — a poll/SSE frame carrying the pre-reset snapshot
// repainted the old count over the optimistic 0. The guard is a monotonic
// StatusSeq on every published snapshot plus a mutation epoch that drops
// in-flight rebuilds started before the mutation.

// Every published snapshot must carry a strictly increasing StatusSeq and the
// process instance ID so the frontend can order responses (and detect a spoke
// restart, which restarts seqs at 1).
func TestUpdateStatusAssignsMonotonicSeq(t *testing.T) {
	s := newTestServer()

	s.UpdateStatus(minimalPayload())
	first := s.status.StatusSeq
	if first == 0 {
		t.Fatal("first published snapshot has StatusSeq 0, want >= 1")
	}
	if s.status.StatusInstance == "" {
		t.Error("published snapshot has empty StatusInstance")
	}

	s.UpdateStatus(minimalPayload())
	if s.status.StatusSeq <= first {
		t.Errorf("second StatusSeq = %d, want > %d", s.status.StatusSeq, first)
	}
}

// A snapshot whose build began before a mutation must be dropped: publishing
// it would overwrite (and broadcast) pre-mutation values, reverting what the
// operator was just told succeeded.
func TestUpdateStatusIfFreshDropsSnapshotBuiltBeforeMutation(t *testing.T) {
	s := newTestServer()

	fresh := minimalPayload()
	fresh.Agents = []FrontendAgent{{Name: "scanner", Restarts: 0}}
	s.UpdateStatus(fresh)
	published := s.status

	// A slow periodic rebuild captured its epoch, then a mutation landed.
	staleEpoch := s.BeginStatusSnapshot()
	s.noteStatusMutation()

	stale := minimalPayload()
	stale.Agents = []FrontendAgent{{Name: "scanner", Restarts: 7}} // pre-reset value
	if s.UpdateStatusIfFresh(stale, staleEpoch) {
		t.Fatal("stale snapshot (built before mutation) was published")
	}
	if s.status != published {
		t.Error("stale snapshot replaced the cached status")
	}

	// A rebuild started after the mutation publishes normally.
	if !s.UpdateStatusIfFresh(minimalPayload(), s.BeginStatusSnapshot()) {
		t.Error("fresh snapshot (built after mutation) was dropped")
	}
}

// noteStatusMutation returns the floor: the minimum StatusSeq a snapshot must
// carry to be guaranteed post-mutation. The next published fresh snapshot must
// meet it, and a dropped stale snapshot must not consume a seq below it.
func TestNoteStatusMutationFloorIsMetByNextFreshSnapshot(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())

	staleEpoch := s.BeginStatusSnapshot()
	floor := s.noteStatusMutation()
	if floor != s.status.StatusSeq+1 {
		t.Errorf("floor = %d, want %d (last published seq + 1)", floor, s.status.StatusSeq+1)
	}

	// Dropped snapshots must not burn a sequence number.
	s.UpdateStatusIfFresh(minimalPayload(), staleEpoch)
	if !s.UpdateStatusIfFresh(minimalPayload(), s.BeginStatusSnapshot()) {
		t.Fatal("post-mutation snapshot was dropped")
	}
	if s.status.StatusSeq < floor {
		t.Errorf("post-mutation snapshot seq = %d, below floor %d", s.status.StatusSeq, floor)
	}
	if s.status.StatusSeq != floor {
		t.Errorf("post-mutation snapshot seq = %d, want exactly floor %d (drops must not consume seqs)", s.status.StatusSeq, floor)
	}
}

// The reset-restarts endpoint must hand the frontend the minStatusSeq floor so
// it can discard in-flight pre-reset status responses.
func TestResetRestartsResponseCarriesMinStatusSeq(t *testing.T) {
	s, _ := apiServer(t)
	s.UpdateStatus(minimalPayload())
	seqBefore := s.status.StatusSeq

	rec := doPost(s, "/api/reset-restarts/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK           bool   `json:"ok"`
		MinStatusSeq uint64 `json:"minStatusSeq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !body.OK {
		t.Error("response ok = false")
	}
	if body.MinStatusSeq <= seqBefore {
		t.Errorf("minStatusSeq = %d, want > last published seq %d", body.MinStatusSeq, seqBefore)
	}
}

// The budget-window reset shares the same race class (#4315/#4320 surface) and
// must carry the same floor.
func TestBudgetResetResponseCarriesMinStatusSeq(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.SetBudgetLimit(100)
	s.UpdateStatus(minimalPayload())
	seqBefore := s.status.StatusSeq

	rec := doPost(s, "/api/config/governor/budget/reset", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK           bool   `json:"ok"`
		MinStatusSeq uint64 `json:"minStatusSeq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !body.OK {
		t.Error("response ok = false")
	}
	if body.MinStatusSeq <= seqBefore {
		t.Errorf("minStatusSeq = %d, want > last published seq %d", body.MinStatusSeq, seqBefore)
	}
}

// /api/status must serve the seq-stamped snapshot so late poll responses are
// orderable client-side.
func TestHandleStatusServesStatusSeq(t *testing.T) {
	s, _ := apiServer(t)
	s.UpdateStatus(minimalPayload())

	rec := doGet(s, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		StatusSeq      uint64 `json:"statusSeq"`
		StatusInstance string `json:"statusInstance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.StatusSeq == 0 {
		t.Error("served snapshot has statusSeq 0, want >= 1")
	}
	if body.StatusInstance == "" {
		t.Error("served snapshot has empty statusInstance")
	}
}

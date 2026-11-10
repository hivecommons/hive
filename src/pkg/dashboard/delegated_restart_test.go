package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// getDelegatedStatusDirect calls the status handler the way the mux would,
// on a bare Server (no routes, no deps).
func getDelegatedStatusDirect(t *testing.T, s *Server, owner, repo string, number int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/repos/"+owner+"/"+repo+"/items/"+strconv.Itoa(number)+"/decide-delegated", nil)
	req.SetPathValue("owner", owner)
	req.SetPathValue("repo", repo)
	req.SetPathValue("number", strconv.Itoa(number))
	rr := httptest.NewRecorder()
	s.handleRepoItemDecideDelegatedStatus(rr, req)
	return rr
}

// A let-hive-decide still pending when the hive restarts (its job lived in
// the old process's memory) is reported failed by the next process, never
// resumed, and the status endpoint answers it instead of a 404. Shaped on
// projectbluefin/utah#1160: job projectbluefin/utah#1160@dmr35pr6zzmt
// started 2026-11-09T23:57:45Z, the hive restarted at 01:31Z, and the
// status read answered 404 "no delegated decision for this item".
func TestDelegatedJobInterruptedByRestartIsReportedFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	at := time.Date(2026, 11, 9, 23, 57, 45, 0, time.UTC)
	const runID = "projectbluefin/utah#1160@dmr35pr6zzmt"

	// What the previous process left on disk.
	before := newOperatorActionStore(path)
	if err := before.Record("projectbluefin/utah", 1160, OperatorAction{Action: operatorActionLetHiveDecide, Actor: "Danathar", At: at, Outcome: operatorActionOutcomePending, Stage: 85, JobID: runID}); err != nil {
		t.Fatal(err)
	}
	// A pending action of another kind is not a delegated job: left alone.
	if err := before.Record("projectbluefin/utah", 1161, OperatorAction{Action: operatorActionMergeNow, Actor: "Danathar", At: at, Outcome: operatorActionOutcomePending, Stage: 85}); err != nil {
		t.Fatal(err)
	}

	// The restarted process: a fresh Server on the same ledger file, with
	// no job in memory.
	s := &Server{operatorActionStore: newOperatorActionStore(path)}
	rr := getDelegatedStatusDirect(t, s, "projectbluefin", "utah", 1160)
	if rr.Code != http.StatusOK {
		t.Fatalf("status after restart: code=%d body=%s (want 200 with the interrupted outcome, not a 404)", rr.Code, rr.Body.String())
	}
	var job delegatedDecisionJob
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if job.Status != delegatedJobStatusFailed || job.ID != runID || job.Requester != "Danathar" || !job.FromLedger || !job.StartedAt.Equal(at) {
		t.Fatalf("job = %+v, want failed %s by Danathar from the ledger", job, runID)
	}
	if !strings.Contains(job.Error, "restart") || !strings.Contains(job.Error, "not resumed") {
		t.Fatalf("error = %q, want the interrupted-by-restart words", job.Error)
	}

	got := s.operatorActions().List("projectbluefin/utah", 1160)
	if len(got) != 1 || got[0].Outcome != operatorActionOutcomeFailed || got[0].Detail != delegatedRestartInterruptedDetail {
		t.Fatalf("ledger 1160 = %+v, want failed with the interrupted detail", got)
	}
	// Persisted, so a third process does not see it pending again.
	if again := newOperatorActionStore(path).List("projectbluefin/utah", 1160); len(again) != 1 || again[0].Outcome != operatorActionOutcomeFailed {
		t.Fatalf("persisted ledger 1160 = %+v", again)
	}
	if other := s.operatorActions().List("projectbluefin/utah", 1161); len(other) != 1 || other[0].Outcome != operatorActionOutcomePending {
		t.Fatalf("merge-now record touched: %+v", other)
	}
	// The row view turns failed, not "pending" forever.
	if view := stageTrackerOperatorAction(got, 85, nil, nil, at.Add(2*time.Hour), time.Hour); view == nil || view.State != operatorActionStateFailed {
		t.Fatalf("row view = %+v, want failed", view)
	}

	// A decision this process starts afterwards is its own and stays
	// pending until its job settles it.
	if err := s.operatorActions().Record("projectbluefin/utah", 1162, OperatorAction{Action: operatorActionLetHiveDecide, Actor: "Danathar", At: time.Now().UTC(), Outcome: operatorActionOutcomePending, JobID: "projectbluefin/utah#1162@x"}); err != nil {
		t.Fatal(err)
	}
	rr = getDelegatedStatusDirect(t, s, "projectbluefin", "utah", 1162)
	if rr.Code != http.StatusOK {
		t.Fatalf("status of this process's job: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil || job.Status != delegatedJobStatusRunning {
		t.Fatalf("this process's pending record must read running, got %+v (%v)", job, err)
	}
}

// The status endpoint keeps an item's last result after the finished job
// is evicted from memory (delegatedDecisionJobTTL), instead of a 404.
func TestDelegatedStatusKeepsLastResultAfterEviction(t *testing.T) {
	s := &Server{operatorActionStore: newOperatorActionStore("")}
	const repo, number = "o/r", 7
	key := delegatedJobKey(repo, number)
	now := time.Now()
	id := key + "@" + strconv.FormatInt(now.UnixNano(), 36)
	if err := s.operatorActions().Record(repo, number, OperatorAction{Action: operatorActionLetHiveDecide, Actor: "alice", At: now.UTC(), Outcome: operatorActionOutcomePending, JobID: id}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.delegatedJobs.start(key, &delegatedDecisionJob{ID: id, Repo: repo, Number: number, Type: "pr", Status: delegatedJobStatusRunning, Requester: "alice", StartedAt: now}, now); !ok {
		t.Fatal("job did not start")
	}
	s.delegatedJobs.finish(key, func(j *delegatedDecisionJob) {
		j.Status = delegatedJobStatusDone
		j.Decision = &delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "green-ci"}
	})
	s.settleDelegatedJob(key, repo, number)

	// Another job starting past the TTL evicts the finished one.
	later := time.Now().Add(delegatedDecisionJobTTL + time.Minute)
	if _, ok := s.delegatedJobs.start(delegatedJobKey(repo, 8), &delegatedDecisionJob{ID: "o/r#8@y", Status: delegatedJobStatusRunning, StartedAt: later}, later); !ok {
		t.Fatal("second job did not start")
	}
	if _, inMemory := s.delegatedJobs.get(key); inMemory {
		t.Fatal("finished job should have been evicted")
	}

	rr := getDelegatedStatusDirect(t, s, "o", "r", number)
	if rr.Code != http.StatusOK {
		t.Fatalf("status after eviction: code=%d body=%s (want the last result, not a 404)", rr.Code, rr.Body.String())
	}
	var job delegatedDecisionJob
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if job.ID != id || job.Status != delegatedJobStatusDone || !job.FromLedger || job.Decision == nil ||
		job.Decision.Verdict != delegatedecision.VerdictApprove || job.Decision.Rule != "green-ci" {
		t.Fatalf("job = %+v, want done approve (green-ci) from the ledger", job)
	}

	// An item with no let-hive-decide on record is still a 404.
	if rr := getDelegatedStatusDirect(t, s, "o", "r", 99); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown item: code=%d", rr.Code)
	}
}

func TestOperatorActionJobID(t *testing.T) {
	cases := map[string]string{
		`{"id":"o/r#7@abc","status":"running"}`: "o/r#7@abc",
		`{"status":"running"}`:                  "",
		`not json`:                              "",
		``:                                      "",
	}
	for body, want := range cases {
		if got := operatorActionJobID([]byte(body)); got != want {
			t.Errorf("operatorActionJobID(%q) = %q, want %q", body, got, want)
		}
	}
}

// The ledger keeps the job id of a let-hive-decide request that started
// its job, and not the id a 409 answers with (another request's job).
func TestRecordOperatorActionKeepsDelegatedJobID(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
	}{
		{http.StatusAccepted, "org/repo#7@abc"},
		{http.StatusConflict, ""},
	} {
		s := &Server{operatorActionStore: newOperatorActionStore("")}
		h := s.recordOperatorAction(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			jsonResponse(w, map[string]string{"id": "org/repo#7@abc", "status": delegatedJobStatusRunning})
		})
		r := httptest.NewRequest(http.MethodPost, "/api/repos/org/repo/items/7/decide-delegated", strings.NewReader(`{}`))
		r.SetPathValue("owner", "org")
		r.SetPathValue("repo", "repo")
		r.SetPathValue("number", "7")
		h(httptest.NewRecorder(), r)
		got := s.operatorActions().List("org/repo", 7)
		if len(got) != 1 || got[0].JobID != c.want {
			t.Fatalf("status %d: records=%+v, want job_id %q", c.status, got, c.want)
		}
	}
}

package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

// hivecommons/hive#10070: POST /api/plan/{epic}/design/approve must check the
// Spektacular spec-not-ready gate BEFORE applying the approval label and
// marking the epic approved, not after. Before the fix, a 409 toast left the
// forge/epic state diverged from what it reported.
func designApproveTestServer(t *testing.T, repo string, number int) (*Server, *beads.Store, *beads.Bead, string) {
	t.Helper()
	srv, store, _ := planServer(t)
	epic, err := store.Create("design epic", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	runKey := repo + "#" + strconv.Itoa(number)
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaIssueRepo] = repo
		b.Metadata[planning.MetaIssueNumber] = strconv.Itoa(number)
		b.Metadata[planning.MetaDesignVia] = planning.DesignViaSpektacular
		b.Metadata[planning.MetaDesignStatus] = planning.DesignStatusRequested
	}); err != nil {
		t.Fatalf("mark design epic: %v", err)
	}
	if err := srv.contributeHub.recordLeaseForKeyStage("alice", "task-"+strconv.Itoa(number), repo, number, "", "contributor", StageSpec, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	epic, _ = store.Get(epic.ID)
	return srv, store, epic, runKey
}

// TestHandlePlanDesignApprove_SpecNotReadyRefusesBeforeMutating confirms that
// when the Spektacular spec is still actively running (lease stage "spec",
// no parked checkpoint receipt), the handler refuses with 409 WITHOUT first
// applying the approval label or marking the epic design-approved.
func TestHandlePlanDesignApprove_SpecNotReadyRefusesBeforeMutating(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
	}))
	defer gh.Close()

	srv, store, epic, _ := designApproveTestServer(t, "acme/widgets", 42)
	srv.deps.GHClient = ghpkg.NewClientForTest(gh.URL, "acme", []string{"widgets"}, srv.logger)

	rec := doOwnerPost(srv, "/api/plan/"+epic.ID+"/design/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body.String())
	}

	got, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if planning.DesignStatus(got) == planning.DesignStatusApproved {
		t.Fatalf("design status must not be approved after a 409 refusal, got %q", got.Meta(planning.MetaDesignStatus))
	}
}

// TestHandlePlanDesignApprove_HeldSpecCheckpointApproves confirms a spec
// that has already produced its receipt and parked at the checkpoint (ready
// for review, even though its lease stage is still "spec") is approvable.
func TestHandlePlanDesignApprove_HeldSpecCheckpointApproves(t *testing.T) {
	var labeled bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/issues/42/labels" {
			labeled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer gh.Close()

	srv, store, epic, runKey := designApproveTestServer(t, "acme/widgets", 42)
	srv.deps.GHClient = ghpkg.NewClientForTest(gh.URL, "acme", []string{"widgets"}, srv.logger)

	oldReceipts := runReceiptsDir
	runReceiptsDir = t.TempDir()
	t.Cleanup(func() { runReceiptsDir = oldReceipts })
	if _, err := writeStageReceipt(runKey, StageSpec, 1, []byte(`{}`)); err != nil {
		t.Fatalf("write spec receipt: %v", err)
	}

	rec := doOwnerPost(srv, "/api/plan/"+epic.ID+"/design/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !labeled {
		t.Fatalf("expected approval label to be applied once the spec checkpoint is held")
	}
	got, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if planning.DesignStatus(got) != planning.DesignStatusApproved {
		t.Fatalf("design status = %q, want approved", got.Meta(planning.MetaDesignStatus))
	}
}

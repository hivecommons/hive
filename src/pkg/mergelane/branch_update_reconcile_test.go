package mergelane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	ghub "github.com/hivecommons/hive/pkg/github"
)

const (
	reconcileHead      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reconcileMovedHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// branchUpdateServer answers the lane's update-branch PUT with a scripted
// status and its PR GET with the current head.
type branchUpdateServer struct {
	mu     sync.Mutex
	status []int // update-branch statuses, one per PUT; the last repeats
	head   string
	puts   int
	gets   int
}

func (s *branchUpdateServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widgets/pulls/7/update-branch":
		code := s.status[len(s.status)-1]
		if s.puts < len(s.status) {
			code = s.status[s.puts]
		}
		s.puts++
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "scripted"})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/7":
		s.gets++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "state": "open",
			"head": map[string]any{"sha": s.head, "ref": "feature"},
			"base": map[string]any{"ref": "main"},
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
	}
}

func (s *branchUpdateServer) counts() (puts, gets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.gets
}

func (s *branchUpdateServer) setHead(head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.head = head
}

func journalingLaneGitHub(t *testing.T, s *branchUpdateServer) *RESTGitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "claims.json"), 1)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	b := &mutation.Boundary{
		Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: proof.ModeEnforce},
		Holder:   "test",
	}
	client := ghub.NewClientForTest(srv.URL, "acme", []string{"widgets"}, nil)
	client.SetMutationBoundary(b)
	return &RESTGitHub{Client: func() *ghub.Client { return client }}
}

// An update whose outcome GitHub never confirmed (5xx) is journaled Unknown.
// The lane retries the same pinned update next round; before #18726 the
// boundary refused that retry forever ("retries refused pending
// reconciliation"). Now the retry reads the PR: the head is still the pinned
// one, so the update did not land and it runs again.
func TestUpdateBranchUnknownReconcilesNotAppliedAndRetries(t *testing.T) {
	s := &branchUpdateServer{status: []int{http.StatusBadGateway, http.StatusAccepted}, head: reconcileHead}
	g := journalingLaneGitHub(t, s)
	ctx := context.Background()

	if err := g.UpdateBranch(ctx, testRepo, 7, reconcileHead); err == nil || errors.Is(err, ErrHeadMoved) {
		t.Fatalf("first update = %v, want an uncertain failure", err)
	}
	putsBefore, getsBefore := s.counts()
	if err := g.UpdateBranch(ctx, testRepo, 7, reconcileHead); err != nil {
		t.Fatalf("retry of an Unknown update = %v, want it reconciled and run", err)
	}
	puts, gets := s.counts()
	if gets <= getsBefore || puts <= putsBefore {
		t.Fatalf("retry made gets %d->%d puts %d->%d, want a reconciling read and a new update", getsBefore, gets, putsBefore, puts)
	}
}

// When the head moved off the pinned SHA, the Unknown update landed (or the
// branch moved on, which makes it moot): the retry replays as success and
// does not call update-branch again.
func TestUpdateBranchUnknownReconcilesAppliedWithoutRepeating(t *testing.T) {
	s := &branchUpdateServer{status: []int{http.StatusBadGateway}, head: reconcileHead}
	g := journalingLaneGitHub(t, s)
	ctx := context.Background()

	if err := g.UpdateBranch(ctx, testRepo, 7, reconcileHead); err == nil {
		t.Fatal("first update must fail")
	}
	s.setHead(reconcileMovedHead)
	putsBefore, _ := s.counts()
	if err := g.UpdateBranch(ctx, testRepo, 7, reconcileHead); err != nil {
		t.Fatalf("retry after the head moved = %v, want idempotent success", err)
	}
	if puts, _ := s.counts(); puts != putsBefore {
		t.Fatalf("update-branch called %d more time(s); an applied update must not repeat", puts-putsBefore)
	}
}

// A 422 is GitHub refusing the update outright: it is journaled not-applied,
// so the next attempt runs directly with no reconciling read and no refusal.
func TestUpdateBranchRefusalIsNotAppliedAndNeverStuck(t *testing.T) {
	s := &branchUpdateServer{status: []int{http.StatusUnprocessableEntity, http.StatusAccepted}, head: reconcileHead}
	g := journalingLaneGitHub(t, s)
	ctx := context.Background()

	if err := g.UpdateBranch(ctx, testRepo, 7, reconcileHead); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("422 update = %v, want ErrHeadMoved", err)
	}
	if err := g.UpdateBranch(ctx, testRepo, 7, reconcileHead); err != nil {
		t.Fatalf("retry after a definite refusal = %v, want it to run", err)
	}
	if puts, gets := s.counts(); puts != 2 || gets != 0 {
		t.Fatalf("puts=%d gets=%d, want two updates and no reconciling read", puts, gets)
	}
}

package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/reviewgate"
)

// giteaGateFake serves PR o/r#7 on a Forgejo / Gitea API with configurable
// reviews and collaborator permissions, counting the writes the open
// change-request gate (hivecommons/hive#20067) must prevent.
type giteaGateFake struct {
	mu      sync.Mutex
	reviews []map[string]any
	perms   map[string]string
	merges  int
	labels  int
}

func (f *giteaGateFake) counts() (merges, labels int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merges, f.labels
}

func (f *giteaGateFake) forge(t *testing.T) *giteaForge {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/pulls/7/reviews":
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_ = json.NewEncoder(w).Encode(f.reviews)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/o/r/collaborators/"):
			login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/o/r/collaborators/"), "/permission")
			p, ok := f.perms[login]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": p})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/pulls/7":
			_ = json.NewEncoder(w).Encode(openPR("h7"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/o/r/pulls/7/merge":
			f.merges++
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/o/r/issues/7/labels":
			f.labels++
			_, _ = w.Write([]byte(`[{"id":1,"name":"lgtm"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	g, err := newGiteaForge("test-token", Options{BaseURL: srv.URL, Org: "o"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func giteaReview(id int64, login, state string, dismissed bool, submitted string) map[string]any {
	return map[string]any{"id": id, "user": map[string]string{"login": login}, "state": state, "dismissed": dismissed, "commit_id": "h6", "submitted_at": submitted}
}

func TestGiteaOpenChangeRequestBlocksMergeAndApprovalLabel(t *testing.T) {
	f := &giteaGateFake{
		reviews: []map[string]any{giteaReview(42, "maint", "REQUEST_CHANGES", false, "2026-11-26T10:00:00Z")},
		perms:   map[string]string{"maint": "write"},
	}
	g := f.forge(t)
	err := g.Merge(context.Background(), "o/r", 7, MergeOptions{HeadSHA: "h7"})
	var blocked *reviewgate.BlockedError
	if !errors.As(err, &blocked) || len(blocked.Blocks) != 1 || blocked.Blocks[0].Reviewer != "maint" || blocked.Blocks[0].ReviewID != 42 {
		t.Fatalf("Merge err = %v, want a BlockedError naming maint's review 42", err)
	}
	if err := g.AddLabels(context.Background(), "o/r", 7, []string{"LGTM"}); !errors.Is(err, reviewgate.ErrChangesRequested) {
		t.Fatalf("AddLabels(lgtm) err = %v, want ErrChangesRequested", err)
	}
	if err := g.AddLabels(context.Background(), "o/r", 7, []string{"approved"}); !errors.Is(err, reviewgate.ErrChangesRequested) {
		t.Fatalf("AddLabels(approved) err = %v, want ErrChangesRequested", err)
	}
	if merges, labels := f.counts(); merges != 0 || labels != 0 {
		t.Fatalf("merges = %d, labels = %d; want neither written", merges, labels)
	}
}

func TestGiteaChangeRequestReleased(t *testing.T) {
	for name, f := range map[string]*giteaGateFake{
		"approval from the same reviewer": {
			reviews: []map[string]any{
				giteaReview(42, "maint", "REQUEST_CHANGES", false, "2026-11-26T10:00:00Z"),
				giteaReview(43, "maint", "APPROVED", false, "2026-11-26T11:00:00Z"),
			},
			perms: map[string]string{"maint": "write"},
		},
		"dismissed review": {
			reviews: []map[string]any{giteaReview(42, "maint", "REQUEST_CHANGES", true, "2026-11-26T10:00:00Z")},
			perms:   map[string]string{"maint": "admin"},
		},
		"change request from a non-writer": {
			reviews: []map[string]any{giteaReview(42, "visitor", "REQUEST_CHANGES", false, "2026-11-26T10:00:00Z")},
			perms:   map[string]string{"visitor": "read"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := f.forge(t)
			if err := g.Merge(context.Background(), "o/r", 7, MergeOptions{HeadSHA: "h7"}); err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if merges, _ := f.counts(); merges != 1 {
				t.Fatalf("merges = %d, want 1", merges)
			}
		})
	}
}

func TestGiteaNonApprovalLabelSkipsGate(t *testing.T) {
	f := &giteaGateFake{
		reviews: []map[string]any{giteaReview(42, "maint", "REQUEST_CHANGES", false, "2026-11-26T10:00:00Z")},
		perms:   map[string]string{"maint": "write"},
	}
	g := f.forge(t)
	// The fake answers the add with lgtm only, so the follow-up apply of a
	// missing label is not what this test is about; the write itself is.
	_ = g.AddLabels(context.Background(), "o/r", 7, []string{"kind/bug"})
	if _, labels := f.counts(); labels == 0 {
		t.Fatal("a non-approval label was refused by the change-request gate")
	}
}

func TestGiteaReviewReadFailureFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/pulls/7":
			_ = json.NewEncoder(w).Encode(openPR("h7"))
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			http.Error(w, "down", http.StatusBadGateway)
		case r.Method == http.MethodPost:
			t.Errorf("POST %s reached the forge with unreadable reviews", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, err := newGiteaForge("test-token", Options{BaseURL: srv.URL, Org: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Merge(context.Background(), "o/r", 7, MergeOptions{}); !errors.Is(err, reviewgate.ErrUnreadable) {
		t.Fatalf("Merge err = %v, want ErrUnreadable", err)
	}
}

// gateStub is a GitHub forge stub whose approval-label gate refuses.
type gateStub struct {
	githubWriter
	githubReader
	writes int
}

func (s *gateStub) AddLabelsToIssue(context.Context, string, string, int, []string) ([]*gh.Label, *gh.Response, error) {
	s.writes++
	return nil, nil, nil
}

func (s *gateStub) GateApprovalLabels(_ context.Context, repo string, number int, labels []string) error {
	return &reviewgate.BlockedError{Repo: repo, Number: number, Blocks: []reviewgate.Block{{Reviewer: "maint", ReviewID: 42}}}
}

func TestGitHubForgeLabelRelayIsGated(t *testing.T) {
	stub := &gateStub{}
	f := &gitHubForge{writer: stub, gate: stub, org: "o"}
	err := f.AddLabels(context.Background(), "o/r", 7, []string{"lgtm"})
	if !errors.Is(err, reviewgate.ErrChangesRequested) {
		t.Fatalf("AddLabels err = %v, want ErrChangesRequested", err)
	}
	if stub.writes != 0 {
		t.Fatalf("writes = %d, want the label write refused", stub.writes)
	}
}

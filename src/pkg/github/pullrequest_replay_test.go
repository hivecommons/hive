package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
)

const (
	replayTestPRNumber    = 42
	replayTestPRURL       = "https://github.com/org/repo/pull/42"
	replayTestMaxWriters  = 1
	replayTestCreateCalls = 1
)

func replayTestBoundary(t *testing.T) *mutation.Boundary {
	t.Helper()
	dir := t.TempDir()
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "claims.json"), replayTestMaxWriters)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	return &mutation.Boundary{
		Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: proof.ModeEnforce},
		Holder:   "test",
	}
}

// TestCreatePR_DedupReplayReturnsRecordedPR is the consumer-level
// reproduction for #8347: when the mutation boundary deduplicates a second
// CreatePR for the same logical operation, the effect (the POST) must not run
// again, and the caller must still receive the recorded PR number and URL,
// not a zero result with a nil error.
func TestCreatePR_DedupReplayReturnsRecordedPR(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/org/repo"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": "main"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			// The open-PR-by-head and identical-tree lookups see nothing, so
			// only the journal can stop the second create.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"number":   replayTestPRNumber,
				"html_url": replayTestPRURL,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())
	c.SetMutationBoundary(replayTestBoundary(t))

	first, err := c.CreatePR(context.Background(), "repo", "feature", "main", "Add tests", "body")
	if err != nil {
		t.Fatalf("first CreatePR: %v", err)
	}
	if first.Number != replayTestPRNumber || first.URL != replayTestPRURL {
		t.Fatalf("first CreatePR = %+v", first)
	}

	replay, err := c.CreatePR(context.Background(), "repo", "feature", "main", "Add tests", "body")
	if err != nil {
		t.Fatalf("replayed CreatePR must be idempotent success, got %v", err)
	}
	if got := creates.Load(); got != replayTestCreateCalls {
		t.Fatalf("POST /pulls ran %d times, want %d (the journal must dedup the replay)", got, replayTestCreateCalls)
	}
	if replay.Number != first.Number || replay.URL != first.URL {
		t.Fatalf("replayed CreatePR = %+v, want the recorded number/URL of %+v", replay, first)
	}
	if !replay.AlreadyExisted {
		t.Fatalf("replayed CreatePR must report AlreadyExisted: %+v", replay)
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

func TestStandbySuspensionReconcilesClosedDonatedPRs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/", func(w http.ResponseWriter, r *http.Request) {
		num := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/pulls/")
		n, _ := strconv.Atoi(num)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": n,
			"state":  "closed",
			"merged": false,
			"user":   map[string]any{"login": "alice"},
			"base": map[string]any{
				"repo": map[string]any{
					"name":      "widgets",
					"full_name": "acme/widgets",
					"owner":     map[string]any{"login": "acme"},
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	s := NewServer(0, logger)
	s.deps = &Dependencies{
		Ctx:      context.Background(),
		GHClient: ghpkg.NewClientForTest(ts.URL, "acme", []string{"widgets"}, logger),
	}
	h := NewContributeWSHub(logger, s)
	t.Cleanup(h.Close)
	h.standbyOutcomesFile = filepath.Join(t.TempDir(), standbyOutcomesFileName)
	key := standbyOutcomeKey("alice", standbypkg.Configuration{Backend: "copilot", Model: "gpt-5.4-mini"})
	h.standbyOutcomes = []standbyOutcomeRecord{
		{Key: key, Lane: "quality", Repo: "acme/widgets", Number: 41, Kind: standbypkg.OutcomeOpen, DispatchedAt: time.Now().Add(-time.Hour)},
		{Key: key, Lane: "quality", Repo: "acme/widgets", Number: 42, Kind: standbypkg.OutcomeOpen, DispatchedAt: time.Now().Add(-time.Hour)},
	}

	suspended, streak := h.standbySuspended(key)
	if !suspended || streak != 2 {
		t.Fatalf("standbySuspended = %v, %d; want suspended after two reconciled closed PRs", suspended, streak)
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	closed := 0
	for _, rec := range h.standbyOutcomes {
		if rec.Kind == standbypkg.OutcomeClosedUnmerged {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("closed_unmerged records = %d, want 2; ledger=%+v", closed, h.standbyOutcomes)
	}
}

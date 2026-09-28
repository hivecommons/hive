package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

// #9184: two reconciles racing over the same closed donated PR must record its
// closure once. Before the fix each snapshotted the ledger, did its GitHub GET,
// and appended its own closed_unmerged row, so one closed PR counted twice and
// suspended the configuration on its own.
func TestStandbyReconcileConcurrentRecordsClosureOnce(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	var gets atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/", func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		arrived <- struct{}{}
		<-release
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"))
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
		{Key: key, Lane: "quality", Repo: "acme/widgets", Number: 57, Kind: standbypkg.OutcomeOpen, DispatchedAt: time.Now().Add(-time.Hour)},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.reconcileOpenStandbyOutcomes() }()
	<-arrived // first reconcile is inside its GitHub GET
	go func() { defer wg.Done(); h.reconcileOpenStandbyOutcomes() }()
	// Give the second reconcile the window the race needs: if it is not
	// serialized behind the first it snapshots the still-open ledger and
	// issues its own GET, which arrives here.
	select {
	case <-arrived:
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	wg.Wait()

	h.completedMu.Lock()
	rows := append([]standbyOutcomeRecord(nil), h.standbyOutcomes...)
	h.completedMu.Unlock()
	closed := 0
	for _, rec := range rows {
		if rec.Kind == standbypkg.OutcomeClosedUnmerged {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("closed_unmerged rows for PR #57 = %d, want 1 (GETs=%d); ledger=%+v", closed, gets.Load(), rows)
	}
	if suspended, streak := h.standbySuspended(key); suspended || streak != 1 {
		t.Fatalf("standbySuspended = %v, %d; one closed PR must not suspend", suspended, streak)
	}
}

// #9184: concurrent appends must not lose rows on disk. Before the fix every
// save wrote the same fixed .tmp path from a snapshot taken outside any lock
// spanning the rename, so a stale snapshot could land last (or a writer could
// rename another's half-written temp) and the reloaded ledger missed rows.
// A preloaded ledger makes every save's snapshot→rename long enough for the
// writers to overlap; the scenario is still repeated to make the check firm.
func TestStandbyOutcomesConcurrentAppendsPersistEveryRow(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const rounds, writers, preload = 10, 8, 4000
	for round := range rounds {
		dir := t.TempDir()
		path := filepath.Join(dir, standbyOutcomesFileName)
		h := NewContributeWSHub(logger, nil)
		h.standbyOutcomesFile = path
		for i := range preload {
			h.standbyOutcomes = append(h.standbyOutcomes, standbyOutcomeRecord{
				Key:    "bob|copilot",
				Repo:   "acme/widgets",
				Number: 100000 + i,
				Kind:   standbypkg.OutcomeMerged,
			})
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 1; i <= writers; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				<-start
				h.appendStandbyOutcome(standbyOutcomeRecord{
					Key:    "alice|copilot",
					Repo:   "acme/widgets",
					Number: n,
					Kind:   standbypkg.OutcomeOpen,
				})
			}(i)
		}
		close(start)
		wg.Wait()
		h.Close()

		reloaded := NewContributeWSHub(logger, nil)
		reloaded.standbyOutcomesFile = path
		reloaded.loadStandbyOutcomes()
		reloaded.completedMu.Lock()
		got := len(reloaded.standbyOutcomes)
		reloaded.completedMu.Unlock()
		reloaded.Close()
		if got != preload+writers {
			t.Fatalf("round %d: reloaded ledger has %d rows, want %d: a concurrent save landed a stale or torn snapshot", round, got, preload+writers)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("round %d: leftover temp file %q after saves", round, e.Name())
			}
		}
	}
}

// #9184: a ledger that cannot be parsed is the record of who is suspended; it
// must not be dropped without a trace, because every suspension it held is
// silently reinstated.
func TestLoadStandbyOutcomesLogsUnreadableLedger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	path := filepath.Join(t.TempDir(), standbyOutcomesFileName)
	if err := os.WriteFile(path, []byte(`[{"key":"alice|copilot","outcome":"closed_unm`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewContributeWSHub(logger, nil)
	h.standbyOutcomesFile = path
	t.Cleanup(h.Close)
	buf.Reset()
	h.loadStandbyOutcomes()
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "standby outcomes") {
		t.Fatalf("unreadable standby ledger was not reported at error level; log=%q", out)
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// reviewQueueSnapshotServer serves both Overview exports, stamping each with
// the snapshot sequence the next entry of seqs names. A refresh between the
// two GETs shows up as the issues and PR exports consuming different entries.
func reviewQueueSnapshotServer(issueSeqs, prSeqs []uint64) *Server {
	s := &Server{mux: http.NewServeMux()}
	serve := func(seqs *[]uint64, number int, rank int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			seq := (*seqs)[0]
			*seqs = (*seqs)[1:]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"snapshot_instance": "i1",
				"snapshot_seq":      seq,
				"snapshot_at":       "2026-10-13T00:00:00Z",
				"bands":             []any{},
				"rows": []any{
					map[string]any{"repo": "o/a", "number": number, "band": "outside", "updated_at": "2026-01-01T00:00:00Z", "your_turn": true, "verb": "REVIEW", "reason_code": "level_gate_hold", "reason": "held", "inbox_rank": rank},
				},
			})
		}
	}
	s.mux.HandleFunc("GET /api/overview/issues.json", serve(&issueSeqs, 1, 2))
	s.mux.HandleFunc("GET /api/overview/prs.json", serve(&prSeqs, 2, 1))
	return s
}

func TestDashboardAdminMCPReviewQueueRereadsAfterSnapshotChange(t *testing.T) {
	// The first issues read is snapshot 7 but the PRs were read after a
	// refresh (8); the re-read of both lands on one snapshot.
	s := reviewQueueSnapshotServer([]uint64{7, 8}, []uint64{8, 8})
	result, err := dashboardAdminMCPProvider{server: s}.Read(context.Background(), adminmcp.ToolReviewQueue, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Total int `json:"total"`
		Rows  []struct {
			Kind   string  `json:"kind"`
			Number float64 `json:"number"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || len(body.Rows) != 2 || body.Rows[0].Kind != "pr" || body.Rows[1].Kind != "issue" {
		t.Fatalf("queue = %s, want the pr then the issue from the matching re-read", encoded)
	}
}

func TestDashboardAdminMCPReviewQueueRefusesWhenSnapshotKeepsChanging(t *testing.T) {
	s := reviewQueueSnapshotServer([]uint64{7, 9}, []uint64{8, 10})
	_, err := dashboardAdminMCPProvider{server: s}.Read(context.Background(), adminmcp.ToolReviewQueue, map[string]any{})
	if !errors.Is(err, adminmcp.ErrReviewQueueSnapshotChanged) {
		t.Fatalf("err = %v, want ErrReviewQueueSnapshotChanged", err)
	}
}

// TestOverviewExportsNameTheirStatusSnapshot pins that both Overview JSON
// exports stamp the snapshot identity of the status they read, so
// review_queue can tell when its two GETs straddled a status refresh.
func TestOverviewExportsNameTheirStatusSnapshot(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Dashboard: config.DashboardConfig{AuthToken: "secret"}}}
	status := parityStatus9102(t)
	status.StatusSeq = 41
	status.StatusInstance = "inst-1"
	status.Timestamp = "2026-10-13T01:02:03Z"
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()

	for _, path := range []string{"/api/overview/issues.json", "/api/overview/prs.json"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var body overviewJSONResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if body.SnapshotSeq != 41 || body.SnapshotInstance != "inst-1" || body.SnapshotAt != "2026-10-13T01:02:03Z" {
			t.Fatalf("%s snapshot = %d/%q/%q, want the status's seq, instance and timestamp", path, body.SnapshotSeq, body.SnapshotInstance, body.SnapshotAt)
		}
	}
}

// TestOverviewExportRowsMatchTheirSnapshotIdentityUnderConcurrentHolds toggles
// an item's hold (which rewrites the shared status in place and bumps its
// identity) while exports are read. Every export must carry rows from exactly
// the snapshot it names: the item is held after an odd number of toggles from
// the starting seq, never the other way round.
func TestOverviewExportRowsMatchTheirSnapshotIdentityUnderConcurrentHolds(t *testing.T) {
	srv := newRepoHoldServer(t, &repoHoldMock{})
	const startSeq = 10
	srv.status = &StatusPayload{
		StatusSeq: startSeq,
		Repos: []FrontendRepo{{
			Name: "testrepo",
			Full: "testorg/testrepo",
			ActionableIssues: []any{github.Issue{
				Repo: "testorg/testrepo", Number: 7, Title: "t", URL: "https://github.test/testorg/testrepo/issues/7",
			}},
			HeldIssues: []any{},
			OpenPrs:    []any{},
			HeldPrs:    []any{},
		}},
	}
	srv.statusSeq = startSeq

	done := make(chan struct{})
	go func() {
		defer close(done)
		held := false
		for range 300 {
			held = !held
			srv.applyRepoHoldToStatus("testorg/testrepo", 7, "issue", held, "hive-pause/h1", nil)
		}
	}()
	reads := 0
	for {
		rec := httptest.NewRecorder()
		srv.handleOverviewIssuesJSON(rec, httptest.NewRequest(http.MethodGet, "/api/overview/issues.json", nil))
		var body overviewJSONResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Rows) != 1 {
			t.Fatalf("rows = %d, want the one issue", len(body.Rows))
		}
		heldRow, _ := body.Rows[0].(map[string]any)["held"].(bool)
		if wantHeld := (body.SnapshotSeq-startSeq)%2 == 1; heldRow != wantHeld {
			t.Fatalf("snapshot_seq %d carries held=%v, want %v: rows and identity came from different snapshots", body.SnapshotSeq, heldRow, wantHeld)
		}
		reads++
		select {
		case <-done:
			if reads < 2 {
				t.Fatalf("only %d exports read", reads)
			}
			return
		default:
		}
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/tokens"
)

type fakeLaunchTokens struct {
	summary  *tokens.AggregateSummary
	launches []tokens.LaunchRecord
}

func (f *fakeLaunchTokens) Summary() *tokens.AggregateSummary { return f.summary }
func (f *fakeLaunchTokens) Launches() []tokens.LaunchRecord   { return f.launches }

func getLaunchCost(s *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/launch-cost"+query, nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func launchFixture(t *testing.T) (*tokens.AggregateSummary, []tokens.LaunchRecord, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-2 * time.Hour)
	launches := []tokens.LaunchRecord{
		{Agent: "scanner", Trigger: "governor-eval", LaunchedAtMs: rcTime(base, 10).UnixMilli(),
			Refs: []tokens.LaunchRef{{Ref: "o/r#1", Kind: tokens.LaunchRefKindIssue, Repo: "o/r", Number: 1}}},
		{Agent: "reviewer", Trigger: "review-review", LaunchedAtMs: rcTime(base, 10).UnixMilli(),
			Refs: []tokens.LaunchRef{tokens.PRLaunchRef("o/r", 9, "abc")}},
	}
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner", rcUsage(rcTime(base, 15), 200, 20)),
		rcSession("s2", "reviewer", rcUsage(rcTime(base, 15), 50, 5)),
	}}
	return summary, launches, now
}

func TestHandleLaunchCostServesSnapshotAndRefFilter(t *testing.T) {
	summary, launches, _ := launchFixture(t)
	persist := filepath.Join(t.TempDir(), "launch-cost.json")
	lc := collect.NewLaunchCostCollector(&fakeLaunchTokens{summary: summary, launches: launches}, nil)
	lc.EnablePersistence(persist)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lc.Start(ctx) // one collect, then ctx is already done

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{LaunchCost: lc})

	rec := getLaunchCost(s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body collect.LaunchCostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || len(body.ByLaunch) != 2 || len(body.ByRef) != 2 {
		t.Fatalf("unexpected body: ready=%v launches=%d refs=%d", body.Ready, len(body.ByLaunch), len(body.ByRef))
	}
	if body.AttributedTokens != 275 || body.TotalTokens != 275 {
		t.Fatalf("tokens attributed=%d total=%d", body.AttributedTokens, body.TotalTokens)
	}

	rec = getLaunchCost(s, "?ref=O%2FR%239") // case-insensitive match
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.ByRef) != 1 || body.ByRef[0].Ref != "o/r#9" || body.ByRef[0].HeadSHAs[0] != "abc" {
		t.Fatalf("ref filter by_ref = %+v", body.ByRef)
	}
	if len(body.ByLaunch) != 1 || body.ByLaunch[0].Agent != "reviewer" {
		t.Fatalf("ref filter by_launch = %+v", body.ByLaunch)
	}
	if body.TotalTokens != 275 {
		t.Fatalf("ref filter must not narrow totals, got %d", body.TotalTokens)
	}

	// Persistence round-trips so a restart serves the last snapshot.
	restored := collect.NewLaunchCostCollector(&fakeLaunchTokens{}, nil)
	restored.EnablePersistence(persist)
	if snap, ready := restored.Snapshot(); !ready || len(snap.ByLaunch) != 2 {
		t.Fatalf("restored snapshot ready=%v launches=%d", ready, len(snap.ByLaunch))
	}
}

func TestHandleLaunchCostNotReadyBeforeFirstCollect(t *testing.T) {
	lc := collect.NewLaunchCostCollector(&fakeLaunchTokens{summary: &tokens.AggregateSummary{}}, nil)
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{LaunchCost: lc})

	var body collect.LaunchCostResponse
	if err := json.Unmarshal(getLaunchCost(s, "").Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ready || len(body.ByLaunch) != 0 || len(body.ByRef) != 0 || !body.CollectedAt.IsZero() {
		t.Fatalf("not-ready must be visibly not-ready: %+v", body)
	}
}

func TestHandleLaunchCostInlineFallbackWithoutCollector(t *testing.T) {
	c := tokens.NewCollector(t.TempDir(), nil)
	c.SetPersistPath(filepath.Join(t.TempDir(), "summary.json"))
	c.SetLaunchLog(tokens.NewLaunchLog("", nil))
	c.RecordLaunch(tokens.LaunchRecord{Agent: "scanner", Trigger: "governor-eval",
		Refs: []tokens.LaunchRef{{Ref: "o/r#1", Kind: tokens.LaunchRefKindIssue, Repo: "o/r", Number: 1}}})

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{Tokens: c})
	rec := getLaunchCost(s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body collect.LaunchCostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.LaunchesRetained != 1 {
		t.Fatalf("inline path must read the collector's launch log, got retained=%d", body.LaunchesRetained)
	}
}
